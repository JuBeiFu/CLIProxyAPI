package proxyrouting

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestResolvePrefersExplicitAuthProxyURL(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global:8080"}}
	auth := &coreauth.Auth{Provider: "codex", ProxyURL: "socks5://direct:1080"}

	selection := Resolve(cfg, auth)
	if selection.ProxyURL != "socks5://direct:1080" {
		t.Fatalf("selection.ProxyURL = %q, want direct auth proxy", selection.ProxyURL)
	}
	if selection.SelectionSource != "auth-proxy-url" {
		t.Fatalf("selection.SelectionSource = %q", selection.SelectionSource)
	}
}

func TestResolveUsesAuthProxyProfile(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyProfiles: []config.ProxyProfile{{Name: "warp-free", ProxyURL: "socks5://warp:1080"}}}}
	auth := &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"proxy_profile": "warp-free"}}

	selection := Resolve(cfg, auth)
	if selection.ProxyURL != "socks5://warp:1080" {
		t.Fatalf("selection.ProxyURL = %q, want profile proxy", selection.ProxyURL)
	}
	if selection.ProxyProfile != "warp-free" {
		t.Fatalf("selection.ProxyProfile = %q, want warp-free", selection.ProxyProfile)
	}
	if selection.SelectionSource != "auth-proxy-profile" {
		t.Fatalf("selection.SelectionSource = %q", selection.SelectionSource)
	}
}

func TestResolveMatchesProxyRoutingRuleByProviderPlanAndAuthKind(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{
		ProxyProfiles: []config.ProxyProfile{{Name: "free-warp", ProxyURL: "socks5://warp:1080"}},
		ProxyRouting: config.ProxyRoutingConfig{Rules: []config.ProxyRoutingRule{{
			Name:         "free-traffic",
			Providers:    []string{"codex", "claude-code"},
			PlanTypes:    []string{"free"},
			AuthKinds:    []string{"oauth"},
			ProxyProfile: "free-warp",
		}}},
	}}
	auth := &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"plan_type": "free", "email": "free@example.com"}}

	selection := Resolve(cfg, auth)
	if selection.ProxyURL != "socks5://warp:1080" {
		t.Fatalf("selection.ProxyURL = %q, want free profile proxy", selection.ProxyURL)
	}
	if selection.RoutingRule != "free-traffic" {
		t.Fatalf("selection.RoutingRule = %q, want free-traffic", selection.RoutingRule)
	}
	if selection.AuthKind != "oauth" {
		t.Fatalf("selection.AuthKind = %q, want oauth", selection.AuthKind)
	}
}

func TestResolveFallsBackToGlobalProxyURL(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global:8080"}}
	auth := &coreauth.Auth{Provider: "antigravity"}

	selection := Resolve(cfg, auth)
	if selection.ProxyURL != "http://global:8080" {
		t.Fatalf("selection.ProxyURL = %q, want global proxy", selection.ProxyURL)
	}
	if selection.SelectionSource != "global-proxy-url" {
		t.Fatalf("selection.SelectionSource = %q", selection.SelectionSource)
	}
}
