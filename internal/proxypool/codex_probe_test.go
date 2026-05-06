package proxypool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeRunnerCertifiesStandbyRoute(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
	}))
	defer srv.Close()

	reg := NewCodexRouteRegistry(CodexRouteConfig{
		PromotionWinThreshold:        2,
		QuarantineFirstByteThreshold: 60 * time.Second,
	})
	runner := NewCodexProbeRunner(CodexProbeConfig{ProbeURL: srv.URL})
	route := RouteDescriptor{Pool: "pool-a", Entry: "proxy-2"}

	outcome, err := runner.Probe(context.Background(), ProbeTarget{
		AuthID: "auth-1",
		Route:  route,
	})
	if err != nil {
		t.Fatalf("Probe returned error: %v", err)
	}
	reg.ApplyProbeOutcome("auth-1", route, outcome)

	if state := reg.RouteState("auth-1", route); state != RouteStateStandby {
		t.Fatalf("state = %v, want %v", state, RouteStateStandby)
	}
}

func TestProbeRunnerQuarantinesInconsistentRoute(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":"plan_mismatch","message":"free route"}}`, http.StatusForbidden)
	}))
	defer srv.Close()

	reg := NewCodexRouteRegistry(CodexRouteConfig{
		PromotionWinThreshold:        2,
		QuarantineFirstByteThreshold: 60 * time.Second,
	})
	runner := NewCodexProbeRunner(CodexProbeConfig{ProbeURL: srv.URL})
	route := RouteDescriptor{Pool: "pool-a", Entry: "proxy-9"}

	outcome, err := runner.Probe(context.Background(), ProbeTarget{
		AuthID: "auth-1",
		Route:  route,
	})
	if err != nil {
		t.Fatalf("Probe returned error: %v", err)
	}
	reg.ApplyProbeOutcome("auth-1", route, outcome)

	if state := reg.RouteState("auth-1", route); state != RouteStateQuarantined {
		t.Fatalf("state = %v, want %v", state, RouteStateQuarantined)
	}
}
