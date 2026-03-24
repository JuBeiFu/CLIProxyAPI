package executor

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestDialCodexWebsocket_AllInvalidProxiesReturnsError(t *testing.T) {
	exec := NewCodexWebsocketsExecutor(&config.Config{
		SDKConfig: config.SDKConfig{
			ProxyURL: "bad://proxy:1,also-bad://proxy:2",
		},
	})

	conn, resp, err := exec.dialCodexWebsocket(context.Background(), nil, "wss://example.invalid", http.Header{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if conn != nil {
		t.Fatalf("expected nil conn, got %v", conn)
	}
	if resp != nil {
		t.Fatalf("expected nil resp, got %v", resp.Status)
	}
}

func TestNewProxyAwareWebsocketDialer_DirectModeDisablesProxyFunc(t *testing.T) {
	dialer, ok := newProxyAwareWebsocketDialer("direct")
	if !ok {
		t.Fatal("expected direct proxy mode to be accepted")
	}
	if dialer == nil {
		t.Fatal("expected dialer, got nil")
	}
	if dialer.Proxy != nil {
		t.Fatal("expected direct websocket dialer to disable proxy function")
	}
}

func TestNewProxyAwareWebsocketDialer_NoneModeDisablesProxyFunc(t *testing.T) {
	dialer, ok := newProxyAwareWebsocketDialer("none")
	if !ok {
		t.Fatal("expected none proxy mode to be accepted")
	}
	if dialer == nil {
		t.Fatal("expected dialer, got nil")
	}
	if dialer.Proxy != nil {
		t.Fatal("expected none websocket dialer to disable proxy function")
	}
}

func TestResolveProxyPoolURLs_UsesProxyRoutingRuleProfile(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			ProxyProfiles: []config.ProxyProfile{
				{Name: "free-warp", ProxyURL: "socks5://warp-a:1080,socks5://warp-b:1080"},
			},
			ProxyRouting: config.ProxyRoutingConfig{
				Rules: []config.ProxyRoutingRule{{
					Name:         "free-codex",
					Providers:    []string{"codex"},
					PlanTypes:    []string{"free"},
					ProxyProfile: "free-warp",
				}},
			},
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"plan_type": "free", "email": "free@example.com"},
	}

	got := resolveProxyPoolURLs(cfg, auth)
	if len(got) != 2 {
		t.Fatalf("resolveProxyPoolURLs returned %d entries, want 2", len(got))
	}
	wantSet := map[string]struct{}{
		"socks5://warp-a:1080": {},
		"socks5://warp-b:1080": {},
	}
	for _, proxyURL := range got {
		if _, ok := wantSet[proxyURL]; !ok {
			t.Fatalf("unexpected proxy URL %q", proxyURL)
		}
	}
}

func TestResolveProxyPoolURLs_InvalidExplicitAuthProxyFallsBackToGlobal(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
	}
	auth := &cliproxyauth.Auth{Provider: "codex", ProxyURL: "bad-value"}

	got := resolveProxyPoolURLs(cfg, auth)
	if len(got) != 1 || got[0] != "http://global-proxy.example.com:8080" {
		t.Fatalf("resolveProxyPoolURLs = %#v, want global proxy fallback", got)
	}
}

func TestResolveProxyPoolURLs_DirectExplicitAuthProxyOverridesGlobal(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
	}
	auth := &cliproxyauth.Auth{Provider: "codex", ProxyURL: "direct"}

	got := resolveProxyPoolURLs(cfg, auth)
	if len(got) != 1 || got[0] != "direct" {
		t.Fatalf("resolveProxyPoolURLs = %#v, want direct", got)
	}
}
