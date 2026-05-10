package executor

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/proxypool"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestAdvanceCodexFailoverStateDirectV6NetworkUnreachableFallsBackToProxyPool(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			DefaultProxyPool: "free-egress",
			ProxyPools: []config.ProxyPool{{
				Name: "free-egress",
				IPv6BindLeaseRanges: []config.IPv6BindLeaseRange{
					{CIDR: "2602:294:0:eb::/126"},
				},
				Entries: []config.ProxyPoolEntry{{
					Name: "free-proxy-1",
					URL:  "socks5://127.0.0.1:11081",
				}},
			}},
		},
	}
	auth := &cliproxyauth.Auth{ID: "auth-direct-v6", Index: "idx-1", Provider: "codex"}
	manager := proxypool.DefaultCodexFailoverManager()
	t.Cleanup(func() { manager.Clear(auth.ID) })
	manager.PreferDirectV6WithReason(cfg, auth.ID, "warmup", time.Now())

	changed := advanceCodexFailoverState(cfg, auth, proxypool.Resolution{
		Source: "direct-v6-sticky",
	}, "network-unreachable")
	if !changed {
		t.Fatal("advanceCodexFailoverState returned false, want true")
	}

	snapshot := manager.Snapshot(auth.ID, time.Now())
	if snapshot.Mode != proxypool.CodexFailoverModeProxy {
		t.Fatalf("Mode = %q, want %q", snapshot.Mode, proxypool.CodexFailoverModeProxy)
	}
	if snapshot.Reason != "network-unreachable" {
		t.Fatalf("Reason = %q, want %q", snapshot.Reason, "network-unreachable")
	}
}

func TestAdvanceCodexFailoverStateDirectV6StillRotatesForOtherTransportErrors(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			DefaultProxyPool: "free-egress",
			ProxyPools: []config.ProxyPool{{
				Name: "free-egress",
				IPv6BindLeaseRanges: []config.IPv6BindLeaseRange{
					{CIDR: "2602:294:0:eb::/126"},
				},
			}},
		},
	}
	auth := &cliproxyauth.Auth{ID: "auth-direct-v6-rotate", Index: "idx-1", Provider: "codex"}
	cliproxyauth.SetIPv6BindLease(auth, cliproxyauth.IPv6BindLeaseInfo{
		Pool:      "free-egress",
		EntryName: "acct-v6-auth",
		IP:        "2602:294:0:eb::",
		URL:       "bind://[2602:294:0:eb::]",
	})
	manager := proxypool.DefaultCodexFailoverManager()
	t.Cleanup(func() { manager.Clear(auth.ID) })
	manager.PreferDirectV6WithReason(cfg, auth.ID, "warmup", time.Now())

	changed := advanceCodexFailoverState(cfg, auth, proxypool.Resolution{
		Source: "direct-v6-sticky",
	}, "connection-reset")
	if !changed {
		t.Fatal("advanceCodexFailoverState returned false, want true")
	}

	snapshot := manager.Snapshot(auth.ID, time.Now())
	if snapshot.Mode != proxypool.CodexFailoverModeDirectV6 {
		t.Fatalf("Mode = %q, want %q", snapshot.Mode, proxypool.CodexFailoverModeDirectV6)
	}
	if snapshot.Lease.IP == "" || snapshot.Lease.IP == "2602:294:0:eb::" {
		t.Fatalf("Lease.IP = %q, want rotated concrete lease", snapshot.Lease.IP)
	}
}
