package proxypool

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestResolveUsesStableProxyPoolAssignment(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			DefaultProxyPool: "shared-egress",
			ProxyPools: []config.ProxyPool{
				{
					Name:             "shared-egress",
					FallbackToDirect: true,
					Entries: []config.ProxyPoolEntry{
						{Name: "proxy-a", URL: "http://proxy-a.local:8080"},
						{Name: "proxy-b", URL: "http://proxy-b.local:8080"},
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{ID: "auth-1"}

	first := Resolve(cfg, auth)
	second := Resolve(cfg, auth)

	if first.ProxyURL == "" {
		t.Fatal("expected proxy url from pool, got empty")
	}
	if first.ProxyURL != second.ProxyURL {
		t.Fatalf("expected stable pool assignment, got %q then %q", first.ProxyURL, second.ProxyURL)
	}
	if first.ProxyPool != "shared-egress" {
		t.Fatalf("expected proxy pool shared-egress, got %q", first.ProxyPool)
	}
	if !first.FallbackToDirect {
		t.Fatal("expected fallback-to-direct to be enabled")
	}
}

func TestResolvePrefersExplicitAuthProxyURLOverPool(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			DefaultProxyPool: "shared-egress",
			ProxyPools: []config.ProxyPool{
				{
					Name: "shared-egress",
					Entries: []config.ProxyPoolEntry{
						{Name: "proxy-a", URL: "http://proxy-a.local:8080"},
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{
		ID:       "auth-2",
		ProxyURL: "direct",
	}

	got := Resolve(cfg, auth)
	if got.ProxyURL != "direct" {
		t.Fatalf("expected explicit auth proxy to win, got %q", got.ProxyURL)
	}
	if got.ProxyPool != "" {
		t.Fatalf("expected no proxy pool when auth proxy-url is explicit, got %q", got.ProxyPool)
	}
}

func TestResolveSkipsUnhealthyProxyPoolEntries(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			DefaultProxyPool: "shared-egress",
			ProxyPools: []config.ProxyPool{
				{
					Name:             "shared-egress",
					FallbackToDirect: true,
					Entries: []config.ProxyPoolEntry{
						{Name: "proxy-a", URL: "http://proxy-a.local:8080"},
						{Name: "proxy-b", URL: "http://proxy-b.local:8080"},
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{ID: "auth-3"}
	manager := NewHealthManager()
	now := time.Unix(1_744_419_200, 0)
	manager.StoreResult("shared-egress", "proxy-a", ProbeResult{
		Healthy:   false,
		Error:     "dial tcp timeout",
		CheckedAt: now,
	})
	manager.StoreResult("shared-egress", "proxy-b", ProbeResult{
		Healthy:   true,
		StatusCode: 204,
		CheckedAt: now,
	})

	got := ResolveWithHealth(cfg, auth, manager)
	if got.ProxyName != "proxy-b" {
		t.Fatalf("expected healthy proxy-b, got %q (%s)", got.ProxyName, got.ProxyURL)
	}
}

func TestResolveAllUnhealthyUsesDirectFallback(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			DefaultProxyPool: "shared-egress",
			ProxyPools: []config.ProxyPool{
				{
					Name:             "shared-egress",
					FallbackToDirect: true,
					Entries: []config.ProxyPoolEntry{
						{Name: "proxy-a", URL: "http://proxy-a.local:8080"},
						{Name: "proxy-b", URL: "http://proxy-b.local:8080"},
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{ID: "auth-4"}
	manager := NewHealthManager()
	now := time.Unix(1_744_419_200, 0)
	manager.StoreResult("shared-egress", "proxy-a", ProbeResult{
		Healthy:   false,
		Error:     "refused",
		CheckedAt: now,
	})
	manager.StoreResult("shared-egress", "proxy-b", ProbeResult{
		Healthy:   false,
		Error:     "refused",
		CheckedAt: now,
	})

	got := ResolveWithHealth(cfg, auth, manager)
	if !got.FallbackToDirect {
		t.Fatal("expected fallback-to-direct to stay enabled")
	}
	if got.ProxyURL != "" {
		t.Fatalf("expected no pool proxy when all entries unhealthy, got %q", got.ProxyURL)
	}
	if transport := BuildHTTPRoundTripperForResolution(got); transport == nil {
		t.Fatal("expected direct round tripper when all pool entries are unhealthy")
	}
}
