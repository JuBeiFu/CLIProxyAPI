package api

import (
	"context"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/performance"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type serverPerformanceTestExecutor struct{}

func (serverPerformanceTestExecutor) Identifier() string { return "codex" }

func (serverPerformanceTestExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (serverPerformanceTestExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (serverPerformanceTestExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (serverPerformanceTestExecutor) CountTokens(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (serverPerformanceTestExecutor) HttpRequest(ctx context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	return nil, nil
}

type countingPerformanceScorer struct {
	mu        sync.Mutex
	snapshots int
}

func (s *countingPerformanceScorer) SnapshotCandidate(provider, authID, model string, inflight int) performance.ScoreCandidate {
	s.mu.Lock()
	s.snapshots++
	s.mu.Unlock()
	return performance.ScoreCandidate{Provider: provider, AuthID: authID, Model: model, Inflight: inflight}
}

func (s *countingPerformanceScorer) ScoreCandidates(candidates []performance.ScoreCandidate, cfg performance.Config) []performance.Score {
	return performance.NewScorer(nil).ScoreCandidates(candidates, cfg)
}

func (s *countingPerformanceScorer) SnapshotCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshots
}

func TestRoutingPerformanceConfigFromServerConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Routing.PerformanceAware = true
	cfg.Routing.PerformanceShadowLog = false
	cfg.Routing.PerformanceWindowSeconds = 180
	cfg.Routing.PerformanceMinSamples = 3
	cfg.Routing.PerformanceEWMAAlpha = 0.6
	cfg.Routing.PerformanceWeightTPS = 2
	cfg.Routing.PerformanceWeightLatency = 0.4
	cfg.Routing.PerformanceWeightFailure = 4
	cfg.Routing.PerformanceWeightInflight = 0.7

	got := routingPerformanceConfigFromServerConfig(cfg)
	if !got.Enabled || got.ShadowLog || got.Window != 180*time.Second || got.MinSamples != 3 || got.EWMAAlpha != 0.6 {
		t.Fatalf("unexpected config: %+v", got)
	}
	if got.WeightTPS != 2 || got.WeightLatency != 0.4 || got.WeightFailure != 4 || got.WeightInflight != 0.7 {
		t.Fatalf("unexpected weights: %+v", got)
	}
}

func TestNewServerAppliesPerformanceRoutingConfigToAuthManager(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const model = "server-performance-routing-gpt-5.4"
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(serverPerformanceTestExecutor{})
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("server-perf-auth-a", "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient("server-perf-auth-b", "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient("server-perf-auth-a")
		reg.UnregisterClient("server-perf-auth-b")
	})
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: "server-perf-auth-a", Provider: "codex"}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: "server-perf-auth-b", Provider: "codex"}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}
	manager.RefreshSchedulerEntry("server-perf-auth-a")
	manager.RefreshSchedulerEntry("server-perf-auth-b")

	now := time.Now()
	performance.DefaultTracker().Record(performance.Sample{Provider: "codex", AuthID: "server-perf-auth-a", Model: model, RequestedAt: now, Latency: time.Second, OutputTokens: 10})
	performance.DefaultTracker().Record(performance.Sample{Provider: "codex", AuthID: "server-perf-auth-b", Model: model, RequestedAt: now, Latency: time.Second, OutputTokens: 70})
	cfg := &config.Config{
		AuthDir: t.TempDir(),
		Routing: config.RoutingConfig{
			PerformanceAware:          true,
			PerformanceShadowLog:      true,
			PerformanceWindowSeconds:  config.DefaultRoutingPerformanceWindowSeconds,
			PerformanceMinSamples:     1,
			PerformanceEWMAAlpha:      config.DefaultRoutingPerformanceEWMAAlpha,
			PerformanceWeightTPS:      config.DefaultRoutingPerformanceWeightTPS,
			PerformanceWeightLatency:  config.DefaultRoutingPerformanceWeightLatency,
			PerformanceWeightFailure:  config.DefaultRoutingPerformanceWeightFailure,
			PerformanceWeightInflight: config.DefaultRoutingPerformanceWeightInflight,
		},
	}
	_ = NewServer(cfg, manager, sdkaccess.NewManager(), filepath.Join(t.TempDir(), "config.yaml"))

	resp, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if got := string(resp.Payload); got != "server-perf-auth-b" {
		t.Fatalf("selected auth = %q, want server-perf-auth-b", got)
	}
}

func TestNewServerSkipsPerformanceScorerWhenRoutingPerformanceDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const model = "server-disabled-performance-gpt-5.4"
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(serverPerformanceTestExecutor{})
	scorer := &countingPerformanceScorer{}
	manager.SetPerformanceScorer(scorer)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("server-disabled-auth-a", "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient("server-disabled-auth-b", "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient("server-disabled-auth-a")
		reg.UnregisterClient("server-disabled-auth-b")
	})
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: "server-disabled-auth-a", Provider: "codex"}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: "server-disabled-auth-b", Provider: "codex"}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}
	cfg := &config.Config{AuthDir: t.TempDir()}

	_ = NewServer(cfg, manager, sdkaccess.NewManager(), filepath.Join(t.TempDir(), "config.yaml"))

	if _, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if got := scorer.SnapshotCount(); got != 0 {
		t.Fatalf("SnapshotCandidate calls = %d, want 0 when performance routing is disabled", got)
	}
}
