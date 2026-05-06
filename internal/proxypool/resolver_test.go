package proxypool

import (
	"context"
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
		Healthy:    true,
		StatusCode: 204,
		CheckedAt:  now,
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

// Codex auths MUST NOT fall through to FNV-hashed pool selection when the
// probe-validated binding is missing or unhealthy. Doing so would send the
// request through a node that OpenAI may report as free for this specific
// account (the region cache is per-(client_IP, account_id)), burning free
// quota. Expected behavior: default to direct-primary egress, leaving any
// legacy bound proxy to assisted-only paths.
func TestResolveCodexUnboundSkipsFNVAndReturnsDirect(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			DefaultProxyPool: "free-egress",
			ProxyPools: []config.ProxyPool{
				{
					Name:             "free-egress",
					FallbackToDirect: true,
					Entries: []config.ProxyPoolEntry{
						{Name: "proxy-a", URL: "socks5://a.local:1080"},
						{Name: "proxy-b", URL: "socks5://b.local:1080"},
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{ID: "codex-unbound", Provider: "codex"}
	got := ResolveWithHealth(cfg, auth, nil)
	if got.Source != "direct-primary" {
		t.Fatalf("expected Source=direct-primary, got %q", got.Source)
	}
	if got.ProxyURL != "" || got.ProxyName != "" {
		t.Fatalf("expected no proxy selection, got URL=%q name=%q", got.ProxyURL, got.ProxyName)
	}
	if !got.FallbackToDirect {
		t.Fatal("expected FallbackToDirect=true for direct-primary")
	}
}

// Non-codex providers retain the original FNV-hash fallback behavior when
// unbound — the binding concept only applies to codex.
func TestResolveNonCodexUnboundUsesFNVHash(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			DefaultProxyPool: "shared",
			ProxyPools: []config.ProxyPool{
				{
					Name: "shared",
					Entries: []config.ProxyPoolEntry{
						{Name: "proxy-a", URL: "socks5://a.local:1080"},
						{Name: "proxy-b", URL: "socks5://b.local:1080"},
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{ID: "gemini-auth", Provider: "gemini"}
	got := ResolveWithHealth(cfg, auth, nil)
	if got.Source != "proxy-pool" {
		t.Fatalf("expected Source=proxy-pool (FNV hash), got %q", got.Source)
	}
	if got.ProxyName == "" {
		t.Fatal("expected a proxy to be hash-picked for non-codex auth")
	}
}

// Codex auth keeps its legacy bound entry around for assisted mode, but the
// normal primary path should stay on direct egress.
func TestResolveCodexBoundStillUsesDirectPrimary(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			DefaultProxyPool: "free-egress",
			ProxyPools: []config.ProxyPool{
				{
					Name: "free-egress",
					Entries: []config.ProxyPoolEntry{
						{Name: "proxy-a", URL: "socks5://a.local:1080"},
						{Name: "proxy-b", URL: "socks5://b.local:1080"},
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{ID: "codex-bound", Provider: "codex"}
	coreauth.SetBoundProxyEntry(auth, "proxy-b")
	got := ResolveWithHealth(cfg, auth, nil)
	if got.Source != "direct-primary" {
		t.Fatalf("expected Source=direct-primary, got %q", got.Source)
	}
	if got.ProxyName != "" || got.ProxyURL != "" {
		t.Fatalf("expected direct path without proxy selection, got URL=%q name=%q", got.ProxyURL, got.ProxyName)
	}
}

func TestResolveCodexDefaultsToDirectPrimaryEvenWhenBoundProxyExists(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			DefaultProxyPool: "free-egress",
			ProxyPools: []config.ProxyPool{
				{
					Name:             "free-egress",
					FallbackToDirect: true,
					Entries: []config.ProxyPoolEntry{
						{Name: "proxy-1", URL: "socks5://127.0.0.1:10001"},
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{Provider: "codex", ProxyPool: "free-egress"}
	coreauth.SetBoundProxyEntry(auth, "proxy-1")

	got := ResolveWithContext(context.Background(), cfg, auth, nil)
	if got.Source != "direct-primary" {
		t.Fatalf("Source = %q, want %q", got.Source, "direct-primary")
	}
	if got.ProxyURL != "" {
		t.Fatalf("ProxyURL = %q, want empty direct path", got.ProxyURL)
	}
}

func TestResolveCodexUsesLegacyBoundProxyOnlyForAssistedRequests(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			DefaultProxyPool: "free-egress",
			ProxyPools: []config.ProxyPool{
				{
					Name:             "free-egress",
					FallbackToDirect: true,
					Entries: []config.ProxyPoolEntry{
						{Name: "proxy-1", URL: "socks5://127.0.0.1:10001"},
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{Provider: "codex", ProxyPool: "free-egress"}
	coreauth.SetBoundProxyEntry(auth, "proxy-1")
	ctx := WithRequestRoute(context.Background(), RequestRoute{Assisted: true})

	got := ResolveWithContext(ctx, cfg, auth, nil)
	if got.Source != "bound-assisted" {
		t.Fatalf("Source = %q, want %q", got.Source, "bound-assisted")
	}
	if got.ProxyName != "proxy-1" {
		t.Fatalf("ProxyName = %q, want %q", got.ProxyName, "proxy-1")
	}
}

func TestResolveCodexAssistedPrefersIPv6BindLeaseOverLegacyBoundProxy(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			DefaultProxyPool: "free-egress",
			ProxyPools: []config.ProxyPool{
				{
					Name:             "free-egress",
					FallbackToDirect: true,
					IPv6BindLeaseRanges: []config.IPv6BindLeaseRange{
						{CIDR: "2602:294:0:eb::/64"},
					},
					Entries: []config.ProxyPoolEntry{
						{Name: "proxy-1", URL: "socks5://127.0.0.1:10001"},
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{Provider: "codex", ProxyPool: "free-egress"}
	coreauth.SetBoundProxyEntry(auth, "proxy-1")
	coreauth.SetIPv6BindLease(auth, coreauth.IPv6BindLeaseInfo{
		Pool:      "free-egress",
		EntryName: "lease-a",
		IP:        "2602:294:0:eb::42",
		URL:       "bind://[2602:294:0:eb::42]",
	})
	ctx := WithRequestRoute(context.Background(), RequestRoute{Assisted: true})

	got := ResolveWithContext(ctx, cfg, auth, nil)
	if got.Source != "ipv6-bind-lease" {
		t.Fatalf("Source = %q, want %q", got.Source, "ipv6-bind-lease")
	}
	if got.ProxyURL != "bind://[2602:294:0:eb::42]" {
		t.Fatalf("ProxyURL = %q, want bind lease URL", got.ProxyURL)
	}
}

func TestResolveCodexBoundSkipsPassivelyUnhealthyBinding(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			DefaultProxyPool: "free-egress",
			ProxyPools: []config.ProxyPool{
				{
					Name:             "free-egress",
					FallbackToDirect: true,
					Entries: []config.ProxyPoolEntry{
						{Name: "proxy-a", URL: "socks5://a.local:1080"},
						{Name: "proxy-b", URL: "socks5://b.local:1080"},
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{ID: "codex-bound", Provider: "codex"}
	coreauth.SetBoundProxyEntry(auth, "proxy-a")
	manager := NewHealthManager()
	now := time.Now()
	for i := 0; i < DefaultPassiveSlowStrikes; i++ {
		manager.ReportPassiveOutcome("free-egress", "proxy-a", PassiveOutcome{
			Total:         2 * time.Minute,
			ReadBody:      100 * time.Second,
			ResponseBytes: 64 * 1024,
			StatusCode:    200,
			CheckedAt:     now.Add(time.Duration(i) * time.Second),
		})
	}

	got := ResolveWithHealth(cfg, auth, manager)
	if got.Source != "direct-primary" {
		t.Fatalf("expected Source=direct-primary, got %q", got.Source)
	}
	if got.ProxyURL != "" || got.ProxyName != "" {
		t.Fatalf("expected no proxy on direct-primary path, got URL=%q name=%q", got.ProxyURL, got.ProxyName)
	}
	if !got.FallbackToDirect {
		t.Fatal("expected direct fallback on direct-primary path")
	}
}

func TestResolveCodexAssistedSkipsPassivelyUnhealthyBinding(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			DefaultProxyPool: "free-egress",
			ProxyPools: []config.ProxyPool{
				{
					Name:             "free-egress",
					FallbackToDirect: true,
					Entries: []config.ProxyPoolEntry{
						{Name: "proxy-a", URL: "socks5://a.local:1080"},
						{Name: "proxy-b", URL: "socks5://b.local:1080"},
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{ID: "codex-bound", Provider: "codex"}
	coreauth.SetBoundProxyEntry(auth, "proxy-a")
	manager := NewHealthManager()
	now := time.Now()
	for i := 0; i < DefaultPassiveSlowStrikes; i++ {
		manager.ReportPassiveOutcome("free-egress", "proxy-a", PassiveOutcome{
			Total:         2 * time.Minute,
			ReadBody:      100 * time.Second,
			ResponseBytes: 64 * 1024,
			StatusCode:    200,
			CheckedAt:     now.Add(time.Duration(i) * time.Second),
		})
	}

	ctx := WithRequestRoute(context.Background(), RequestRoute{Assisted: true})
	got := ResolveWithContext(ctx, cfg, auth, manager)
	if got.Source != "assisted-direct-fallback" {
		t.Fatalf("expected Source=assisted-direct-fallback, got %q", got.Source)
	}
	if got.ProxyURL != "" || got.ProxyName != "" {
		t.Fatalf("expected no proxy on assisted direct fallback, got URL=%q name=%q", got.ProxyURL, got.ProxyName)
	}
	if !got.FallbackToDirect {
		t.Fatal("expected direct fallback for assisted request")
	}
}

func TestResolveUsesPersistedIPv6BindLeaseBeforePoolBinding(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			DefaultProxyPool: "dedicated-v6",
			ProxyPools: []config.ProxyPool{
				{
					Name: "dedicated-v6",
					IPv6BindLeaseRanges: []config.IPv6BindLeaseRange{
						{CIDR: "2602:294:0:eb::/64"},
					},
					Entries: []config.ProxyPoolEntry{
						{Name: "proxy-a", URL: "socks5://a.local:1080"},
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{ID: "codex-bound", Provider: "codex"}
	coreauth.SetBoundProxyEntry(auth, "proxy-a")
	coreauth.SetIPv6BindLease(auth, coreauth.IPv6BindLeaseInfo{
		Pool:      "dedicated-v6",
		EntryName: "acct-v6-auth",
		IP:        "2602:294:0:eb::42",
		URL:       "bind://[2602:294:0:eb::42]",
	})

	ctx := WithRequestRoute(context.Background(), RequestRoute{Assisted: true})
	got := ResolveWithContext(ctx, cfg, auth, nil)
	if got.Source != "ipv6-bind-lease" {
		t.Fatalf("expected Source=ipv6-bind-lease, got %q", got.Source)
	}
	if got.ProxyURL != "bind://[2602:294:0:eb::42]" {
		t.Fatalf("ProxyURL = %q, want bind lease URL", got.ProxyURL)
	}
	if got.ProxyName != "acct-v6-auth" {
		t.Fatalf("ProxyName = %q, want lease entry name", got.ProxyName)
	}
}
