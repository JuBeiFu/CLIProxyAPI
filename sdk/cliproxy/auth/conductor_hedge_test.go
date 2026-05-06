package auth_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/proxypool"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type hedgeTestExecutor struct {
	mu           sync.Mutex
	behaviors    map[string]hedgeRouteBehavior
	standbyRoute string
	winnerRoute  string
	hedgeStarted bool
}

type hedgeRouteBehavior struct {
	delay            time.Duration
	secondChunkDelay time.Duration
	err              error
}

func newHedgeTestExecutor(primaryDelay, standbyDelay time.Duration) *hedgeTestExecutor {
	return &hedgeTestExecutor{
		behaviors: map[string]hedgeRouteBehavior{
			"proxy-primary": {delay: primaryDelay},
			"proxy-standby": {delay: standbyDelay},
		},
		standbyRoute: "proxy-standby",
	}
}

func (e *hedgeTestExecutor) Identifier() string { return "codex" }

func (e *hedgeTestExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *hedgeTestExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	route, ok := proxypool.RequestRouteFromContext(ctx)
	if !ok {
		return nil, &coreauth.Error{Code: "missing_route_override", Message: "missing request route override"}
	}
	behavior := e.behaviorFor(route.Entry)
	if route.Entry == e.standbyRoute {
		e.mu.Lock()
		e.hedgeStarted = true
		e.mu.Unlock()
	}
	if behavior.err != nil {
		return nil, behavior.err
	}

	out := make(chan cliproxyexecutor.StreamChunk, 1)
	go func() {
		defer close(out)
		timer := time.NewTimer(behavior.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		e.recordWinner(route.Entry)
		out <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")}
		if behavior.secondChunkDelay <= 0 {
			return
		}
		secondTimer := time.NewTimer(behavior.secondChunkDelay)
		defer secondTimer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-secondTimer.C:
		}
		out <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"next\"}\n\n")}
	}()
	return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Route": []string{route.Entry}}, Chunks: out}, nil
}

func (e *hedgeTestExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *hedgeTestExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *hedgeTestExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *hedgeTestExecutor) behaviorFor(route string) hedgeRouteBehavior {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.behaviors[route]
}

func (e *hedgeTestExecutor) SetSecondChunkDelay(route string, delay time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	behavior := e.behaviors[route]
	behavior.secondChunkDelay = delay
	e.behaviors[route] = behavior
}

func (e *hedgeTestExecutor) SetRouteError(route string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	behavior := e.behaviors[route]
	behavior.err = err
	e.behaviors[route] = behavior
}

func (e *hedgeTestExecutor) recordWinner(route string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.winnerRoute == "" {
		e.winnerRoute = route
	}
}

func (e *hedgeTestExecutor) WinningRoute() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.winnerRoute
}

func (e *hedgeTestExecutor) HedgeStarted() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hedgeStarted
}

func TestExecuteStreamHedgeUsesStandbyAfterThreshold(t *testing.T) {
	t.Parallel()

	manager, fake := newHedgeTestManager(t, 200*time.Millisecond, 10*time.Millisecond, 50*time.Millisecond)

	stream, err := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model: "gpt-5.5",
	}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}

	first := <-stream.Chunks
	if first.Err != nil {
		t.Fatalf("first chunk err = %v", first.Err)
	}
	if got := fake.WinningRoute(); got != "proxy-standby" {
		t.Fatalf("WinningRoute = %q, want %q", got, "proxy-standby")
	}
	if !fake.HedgeStarted() {
		t.Fatal("HedgeStarted = false, want true")
	}
}

func TestExecuteStreamDoesNotSwitchAfterFirstPayload(t *testing.T) {
	t.Parallel()

	manager, fake := newHedgeTestManager(t, 10*time.Millisecond, 5*time.Millisecond, 50*time.Millisecond)

	stream, err := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model: "gpt-5.4",
	}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}

	first := <-stream.Chunks
	if first.Err != nil {
		t.Fatalf("first chunk err = %v", first.Err)
	}
	if got := fake.WinningRoute(); got != "proxy-primary" {
		t.Fatalf("WinningRoute = %q, want %q", got, "proxy-primary")
	}
	if fake.HedgeStarted() {
		t.Fatal("HedgeStarted = true, want false")
	}
}

func TestExecuteStreamPrimaryWinnerContinuesStreaming(t *testing.T) {
	t.Parallel()

	manager, fake := newHedgeTestManager(t, 10*time.Millisecond, 200*time.Millisecond, 50*time.Millisecond)
	fake.SetSecondChunkDelay("proxy-primary", 20*time.Millisecond)

	stream, err := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model: "gpt-5.4",
	}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}

	first, ok := <-stream.Chunks
	if !ok {
		t.Fatal("first chunk channel closed")
	}
	if first.Err != nil {
		t.Fatalf("first chunk err = %v", first.Err)
	}
	second, ok := <-stream.Chunks
	if !ok {
		t.Fatal("second chunk channel closed, want continued stream")
	}
	if second.Err != nil {
		t.Fatalf("second chunk err = %v", second.Err)
	}
	if len(second.Payload) == 0 {
		t.Fatal("second chunk payload empty")
	}
}

func TestExecuteStreamFastPrimaryFailureFallsBackToStandby(t *testing.T) {
	t.Parallel()

	manager, fake := newHedgeTestManager(t, 0, 10*time.Millisecond, 200*time.Millisecond)
	fake.SetRouteError("proxy-primary", &coreauth.Error{Code: "primary_failed", Message: "primary failed", Retryable: true})

	stream, err := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model: "gpt-5.5",
	}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}

	first, ok := <-stream.Chunks
	if !ok {
		t.Fatal("first chunk channel closed")
	}
	if first.Err != nil {
		t.Fatalf("first chunk err = %v", first.Err)
	}
	if got := fake.WinningRoute(); got != "proxy-standby" {
		t.Fatalf("WinningRoute = %q, want %q", got, "proxy-standby")
	}
	if !fake.HedgeStarted() {
		t.Fatal("HedgeStarted = false, want true")
	}
}

func newHedgeTestManager(t *testing.T, primaryDelay, standbyDelay, hedgeAfter time.Duration) (*coreauth.Manager, *hedgeTestExecutor) {
	t.Helper()

	cfg := &internalconfig.Config{}
	cfg.ApplyRoutingPerformanceDefaults()
	cfg.CodexRouteManagement.Enabled = true
	cfg.CodexRouteManagement.FirstPayloadHedgeAfter = hedgeAfter

	executor := newHedgeTestExecutor(primaryDelay, standbyDelay)
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{
		ID:        "hedge-auth",
		Provider:  "codex",
		Status:    coreauth.StatusActive,
		ProxyPool: "pool-a",
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}, {ID: "gpt-5.5"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	routeRegistry := proxypool.NewCodexRouteRegistry(proxypool.CodexRouteConfig{
		PromotionWinThreshold:        2,
		QuarantineFirstByteThreshold: 60 * time.Second,
	})
	routeRegistry.UpsertCertifiedRoute(auth.ID, proxypool.RouteDescriptor{Pool: "pool-a", Entry: "proxy-primary"}, time.Now())
	routeRegistry.UpsertCertifiedRoute(auth.ID, proxypool.RouteDescriptor{Pool: "pool-a", Entry: "proxy-standby"}, time.Now())
	manager.SetCodexRouteController(routeRegistry)
	previousRegistry := proxypool.DefaultCodexRouteRegistry()
	proxypool.SetDefaultCodexRouteRegistry(routeRegistry)
	t.Cleanup(func() {
		proxypool.SetDefaultCodexRouteRegistry(previousRegistry)
	})

	return manager, executor
}
