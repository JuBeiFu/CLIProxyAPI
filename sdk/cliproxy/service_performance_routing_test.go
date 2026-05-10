package cliproxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/performance"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/proxypool"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

type servicePerformanceTestExecutor struct{}

func (servicePerformanceTestExecutor) Identifier() string { return "codex" }

func (servicePerformanceTestExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (servicePerformanceTestExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (servicePerformanceTestExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (servicePerformanceTestExecutor) CountTokens(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (servicePerformanceTestExecutor) HttpRequest(ctx context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestRoutingPerformanceConfigFromAppConfig(t *testing.T) {
	cfg := &sdkconfig.Config{}
	cfg.Routing.PerformanceAware = true
	cfg.Routing.PerformanceShadowLog = false
	cfg.Routing.PerformanceWindowSeconds = 120
	cfg.Routing.PerformanceMinSamples = 9
	cfg.Routing.PerformanceEWMAAlpha = 0.4
	cfg.Routing.PerformanceWeightTPS = 1.5
	cfg.Routing.PerformanceWeightLatency = 0.3
	cfg.Routing.PerformanceWeightFailure = 3
	cfg.Routing.PerformanceWeightInflight = 0.8

	got := routingPerformanceConfigFromAppConfig(cfg)
	if !got.Enabled || got.ShadowLog || got.Window != 120*time.Second || got.MinSamples != 9 || got.EWMAAlpha != 0.4 {
		t.Fatalf("unexpected config: %+v", got)
	}
	if got.WeightTPS != 1.5 || got.WeightLatency != 0.3 || got.WeightFailure != 3 || got.WeightInflight != 0.8 {
		t.Fatalf("unexpected weights: %+v", got)
	}
}

func TestServiceAppliesPerformanceRoutingConfigToManagerSelection(t *testing.T) {
	const model = "service-performance-routing-gpt-5.4"
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(servicePerformanceTestExecutor{})
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("service-perf-auth-a", "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient("service-perf-auth-b", "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient("service-perf-auth-a")
		reg.UnregisterClient("service-perf-auth-b")
	})
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: "service-perf-auth-a", Provider: "codex"}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: "service-perf-auth-b", Provider: "codex"}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}
	manager.RefreshSchedulerEntry("service-perf-auth-a")
	manager.RefreshSchedulerEntry("service-perf-auth-b")

	now := time.Now()
	performance.DefaultTracker().Record(performance.Sample{Provider: "codex", AuthID: "service-perf-auth-a", Model: model, RequestedAt: now, Latency: time.Second, OutputTokens: 10})
	performance.DefaultTracker().Record(performance.Sample{Provider: "codex", AuthID: "service-perf-auth-b", Model: model, RequestedAt: now, Latency: time.Second, OutputTokens: 60})
	svc := &Service{coreManager: manager}
	cfg := &sdkconfig.Config{}
	cfg.Routing.PerformanceAware = true
	cfg.Routing.PerformanceShadowLog = true
	cfg.Routing.PerformanceWindowSeconds = internalconfig.DefaultRoutingPerformanceWindowSeconds
	cfg.Routing.PerformanceMinSamples = 1
	cfg.Routing.PerformanceEWMAAlpha = internalconfig.DefaultRoutingPerformanceEWMAAlpha
	cfg.Routing.PerformanceWeightTPS = internalconfig.DefaultRoutingPerformanceWeightTPS
	cfg.Routing.PerformanceWeightLatency = internalconfig.DefaultRoutingPerformanceWeightLatency
	cfg.Routing.PerformanceWeightFailure = internalconfig.DefaultRoutingPerformanceWeightFailure
	cfg.Routing.PerformanceWeightInflight = internalconfig.DefaultRoutingPerformanceWeightInflight

	svc.applyPerformanceRoutingConfig(cfg)

	resp, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if got := string(resp.Payload); got != "service-perf-auth-b" {
		t.Fatalf("selected auth = %q, want service-perf-auth-b", got)
	}
}

func TestServiceApplyPerformanceRoutingConfigKeepsSelectionDeterministicWhenDisabled(t *testing.T) {
	const model = "service-performance-routing-disabled-gpt-5.4"
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	manager.RegisterExecutor(servicePerformanceTestExecutor{})
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("service-perf-disabled-auth-a", "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient("service-perf-disabled-auth-b", "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient("service-perf-disabled-auth-a")
		reg.UnregisterClient("service-perf-disabled-auth-b")
	})
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: "service-perf-disabled-auth-a", Provider: "codex"}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: "service-perf-disabled-auth-b", Provider: "codex"}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}
	manager.RefreshSchedulerEntry("service-perf-disabled-auth-a")
	manager.RefreshSchedulerEntry("service-perf-disabled-auth-b")

	now := time.Now()
	performance.DefaultTracker().Record(performance.Sample{Provider: "codex", AuthID: "service-perf-disabled-auth-a", Model: model, RequestedAt: now, Latency: time.Second, OutputTokens: 10})
	performance.DefaultTracker().Record(performance.Sample{Provider: "codex", AuthID: "service-perf-disabled-auth-b", Model: model, RequestedAt: now, Latency: time.Second, OutputTokens: 60})
	svc := &Service{coreManager: manager}
	cfg := &sdkconfig.Config{}
	cfg.Routing.PerformanceAware = false
	cfg.Routing.PerformanceShadowLog = false

	svc.applyPerformanceRoutingConfig(cfg)

	resp, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if got := string(resp.Payload); got != "service-perf-disabled-auth-a" {
		t.Fatalf("selected auth = %q, want service-perf-disabled-auth-a", got)
	}
}

func TestServiceApplyPerformanceRoutingConfigPreservesCodexRouteRegistryState(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	svc := &Service{coreManager: manager}
	cfg := &sdkconfig.Config{}
	cfg.CodexRouteManagement.Enabled = true

	previousRegistry := proxypool.DefaultCodexRouteRegistry()
	t.Cleanup(func() {
		proxypool.SetDefaultCodexRouteRegistry(previousRegistry)
	})

	svc.applyPerformanceRoutingConfig(cfg)
	reg := proxypool.DefaultCodexRouteRegistry()
	if reg == nil {
		t.Fatal("DefaultCodexRouteRegistry returned nil")
	}

	now := time.Now()
	reg.UpsertCertifiedRoute("auth-1", proxypool.RouteDescriptor{Pool: "pool-a", Entry: "proxy-1"}, now)
	reg.UpsertCertifiedRoute("auth-1", proxypool.RouteDescriptor{Pool: "pool-a", Entry: "proxy-2"}, now.Add(time.Second))

	cfg.CodexRouteManagement.PromotionWinThreshold = 3
	svc.applyPerformanceRoutingConfig(cfg)

	regAfter := proxypool.DefaultCodexRouteRegistry()
	if regAfter != reg {
		t.Fatal("applyPerformanceRoutingConfig replaced codex route registry")
	}
	primary, standby, ok := regAfter.PrimaryAndStandby("auth-1")
	if !ok {
		t.Fatal("PrimaryAndStandby returned ok=false after reload")
	}
	if primary.Entry != "proxy-1" || standby.Entry != "proxy-2" {
		t.Fatalf("primary=%q standby=%q, want proxy-1/proxy-2", primary.Entry, standby.Entry)
	}
}
