package proxypool

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type probeRoundTripper func(req *http.Request) (*http.Response, error)

func (fn probeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestProbeRunnerUsesTargetRouteAndAuthBinding(t *testing.T) {
	t.Parallel()

	const authIDHeader = "X-CLIProxy-Probe-Auth-ID"

	transport := probeRoundTripper(func(req *http.Request) (*http.Response, error) {
		route, ok := RequestRouteFromContext(req.Context())
		if !ok {
			t.Fatal("RequestRouteFromContext returned ok=false")
		}
		if route.Pool != "pool-a" || route.Entry != "proxy-2" {
			t.Fatalf("route = %+v, want pool-a/proxy-2", route)
		}
		if got := strings.TrimSpace(req.Header.Get(authIDHeader)); got != "auth-1" {
			t.Fatalf("%s = %q, want %q", authIDHeader, got, "auth-1")
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("io.ReadAll(req.Body): %v", err)
		}
		text := string(body)
		if !strings.Contains(text, `"codex_probe_auth_id":"auth-1"`) {
			t.Fatalf("probe payload missing auth metadata: %s", text)
		}
		if !strings.Contains(text, `"codex_probe_proxy_entry":"proxy-2"`) {
			t.Fatalf("probe payload missing route metadata: %s", text)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\"}\n\n")),
		}, nil
	})

	runner := NewCodexProbeRunner(CodexProbeConfig{
		ProbeURL: "http://probe.local/responses",
		Client:   &http.Client{Transport: transport},
	})

	if _, err := runner.Probe(context.Background(), ProbeTarget{
		AuthID: "auth-1",
		Route:  RouteDescriptor{Pool: "pool-a", Entry: "proxy-2"},
	}); err != nil {
		t.Fatalf("Probe returned error: %v", err)
	}
}

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
