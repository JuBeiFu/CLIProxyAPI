package executor

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestSyncCodexProbeRoutingStateClearsLeaseWhenPaidPathMovesToPoolEntry(t *testing.T) {
	auth := &cliproxyauth.Auth{Metadata: map[string]any{}}
	cliproxyauth.SetIPv6BindLease(auth, cliproxyauth.IPv6BindLeaseInfo{
		Pool:      "free-egress",
		EntryName: "lease-a",
		IP:        "2602:294:0:eb::42",
		URL:       "bind://[2602:294:0:eb::42]",
	})

	syncCodexProbeRoutingState(auth, "plus", "proxy-a")

	if lease := cliproxyauth.IPv6BindLease(auth); lease.URL != "" || lease.IP != "" {
		t.Fatalf("lease = %+v, want cleared", lease)
	}
	if got := cliproxyauth.BoundProxyEntry(auth); got != "proxy-a" {
		t.Fatalf("bound = %q, want proxy-a", got)
	}
}

func TestSyncCodexProbeRoutingStateClearsLeaseWhenPaidPathMovesToDirect(t *testing.T) {
	auth := &cliproxyauth.Auth{Metadata: map[string]any{}}
	cliproxyauth.SetIPv6BindLease(auth, cliproxyauth.IPv6BindLeaseInfo{
		Pool:      "free-egress",
		EntryName: "lease-a",
		IP:        "2602:294:0:eb::42",
		URL:       "bind://[2602:294:0:eb::42]",
	})

	syncCodexProbeRoutingState(auth, "team", cliproxyauth.BoundProxyEntryDirect)

	if lease := cliproxyauth.IPv6BindLease(auth); lease.URL != "" || lease.IP != "" {
		t.Fatalf("lease = %+v, want cleared", lease)
	}
	if got := cliproxyauth.BoundProxyEntry(auth); got != cliproxyauth.BoundProxyEntryDirect {
		t.Fatalf("bound = %q, want %q", got, cliproxyauth.BoundProxyEntryDirect)
	}
}

func TestSyncCodexProbeRoutingStatePreservesLeaseWhenLeaseStaysPaidPath(t *testing.T) {
	auth := &cliproxyauth.Auth{Metadata: map[string]any{}}
	cliproxyauth.SetIPv6BindLease(auth, cliproxyauth.IPv6BindLeaseInfo{
		Pool:      "free-egress",
		EntryName: "lease-a",
		IP:        "2602:294:0:eb::42",
		URL:       "bind://[2602:294:0:eb::42]",
	})
	cliproxyauth.SetBoundProxyEntry(auth, "legacy-proxy")

	syncCodexProbeRoutingState(auth, "plus", "")

	if lease := cliproxyauth.IPv6BindLease(auth); lease.URL == "" || lease.IP == "" {
		t.Fatalf("lease = %+v, want preserved", lease)
	}
	if got := cliproxyauth.BoundProxyEntry(auth); got != "" {
		t.Fatalf("bound = %q, want cleared legacy binding", got)
	}
}
