package config

import "testing"

func TestNormalizeProxyPoolsExpandsIPv6BindRanges(t *testing.T) {
	pools := NormalizeProxyPools([]ProxyPool{
		{
			Name: "free-egress",
			Entries: []ProxyPoolEntry{
				{Name: "existing", URL: "http://example.com", Weight: 2},
			},
			IPv6BindRanges: []IPv6BindRange{
				{
					NamePrefix: "free-v6-",
					Start:      "2602:294:0:eb::100",
					End:        "2602:294:0:eb::102",
					Weight:     3,
				},
			},
		},
	})

	if len(pools) != 1 {
		t.Fatalf("len(pools) = %d, want 1", len(pools))
	}
	entries := pools[0].Entries
	if len(entries) != 4 {
		t.Fatalf("len(entries) = %d, want 4", len(entries))
	}
	if entries[0].Name != "existing" || entries[0].URL != "http://example.com" || entries[0].Weight != 2 {
		t.Fatalf("unexpected first entry: %+v", entries[0])
	}
	wantURLs := []string{
		"bind://[2602:294:0:eb::100]",
		"bind://[2602:294:0:eb::101]",
		"bind://[2602:294:0:eb::102]",
	}
	for i, wantURL := range wantURLs {
		entry := entries[i+1]
		if entry.URL != wantURL {
			t.Fatalf("entry[%d].URL = %q, want %q", i+1, entry.URL, wantURL)
		}
		if entry.Weight != 3 {
			t.Fatalf("entry[%d].Weight = %d, want 3", i+1, entry.Weight)
		}
		if entry.Disabled {
			t.Fatalf("entry[%d] unexpectedly disabled", i+1)
		}
	}
	if entries[1].Name != "free-v6-1" || entries[2].Name != "free-v6-2" || entries[3].Name != "free-v6-3" {
		t.Fatalf("unexpected generated names: %+v", entries[1:])
	}
}

func TestNormalizeProxyPoolsSkipsInvalidIPv6BindRanges(t *testing.T) {
	pools := NormalizeProxyPools([]ProxyPool{
		{
			Name: "free-egress",
			Entries: []ProxyPoolEntry{
				{URL: "http://example.com"},
			},
			IPv6BindRanges: []IPv6BindRange{
				{
					NamePrefix: "free-v6-",
					Start:      "not-an-ip",
					End:        "still-not-an-ip",
				},
			},
		},
	})

	if len(pools) != 1 {
		t.Fatalf("len(pools) = %d, want 1", len(pools))
	}
	if len(pools[0].Entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(pools[0].Entries))
	}
	if got := pools[0].Entries[0].Name; got != "proxy-1" {
		t.Fatalf("generated fallback name = %q, want %q", got, "proxy-1")
	}
}

func TestNormalizeProxyPoolsExpandsSingleIPv6BindRange(t *testing.T) {
	pools := NormalizeProxyPools([]ProxyPool{
		{
			Name: "free-egress",
			IPv6BindRanges: []IPv6BindRange{
				{
					Start:  "2602:294:0:eb::113",
					Weight: 5,
				},
			},
		},
	})

	if len(pools) != 1 {
		t.Fatalf("len(pools) = %d, want 1", len(pools))
	}
	entries := pools[0].Entries
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if entries[0].URL != "bind://[2602:294:0:eb::113]" {
		t.Fatalf("entry.URL = %q, want %q", entries[0].URL, "bind://[2602:294:0:eb::113]")
	}
	if entries[0].Name != "bind-v6-1" {
		t.Fatalf("entry.Name = %q, want %q", entries[0].Name, "bind-v6-1")
	}
	if entries[0].Weight != 5 {
		t.Fatalf("entry.Weight = %d, want 5", entries[0].Weight)
	}
}

func TestNormalizeProxyPoolsSkipsOversizedIPv6BindRange(t *testing.T) {
	pools := NormalizeProxyPools([]ProxyPool{
		{
			Name: "free-egress",
			Entries: []ProxyPoolEntry{
				{URL: "http://example.com"},
			},
			IPv6BindRanges: []IPv6BindRange{
				{
					NamePrefix: "free-v6-",
					Start:      "2602:294:0:eb::",
					End:        "2602:294:0:eb::1000",
				},
			},
		},
	})

	if len(pools) != 1 {
		t.Fatalf("len(pools) = %d, want 1", len(pools))
	}
	if len(pools[0].Entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(pools[0].Entries))
	}
	if got := pools[0].Entries[0].Name; got != "proxy-1" {
		t.Fatalf("generated fallback name = %q, want %q", got, "proxy-1")
	}
	if got := pools[0].Entries[0].URL; got != "http://example.com" {
		t.Fatalf("explicit entry URL = %q, want %q", got, "http://example.com")
	}
}

func TestNormalizeProxyPoolsKeepsIPv6BindLeaseOnlyPool(t *testing.T) {
	pools := NormalizeProxyPools([]ProxyPool{
		{
			Name: "dedicated-v6",
			IPv6BindLeaseRanges: []IPv6BindLeaseRange{
				{
					NamePrefix: "acct-v6-",
					CIDR:       "2602:294:0:eb::/64",
				},
			},
		},
	})

	if len(pools) != 1 {
		t.Fatalf("len(pools) = %d, want 1", len(pools))
	}
	if len(pools[0].Entries) != 0 {
		t.Fatalf("len(entries) = %d, want 0", len(pools[0].Entries))
	}
	if len(pools[0].IPv6BindLeaseRanges) != 1 {
		t.Fatalf("len(lease ranges) = %d, want 1", len(pools[0].IPv6BindLeaseRanges))
	}
	if pools[0].IPv6BindLeaseRanges[0].CIDR != "2602:294:0:eb::/64" {
		t.Fatalf("lease CIDR = %q, want %q", pools[0].IPv6BindLeaseRanges[0].CIDR, "2602:294:0:eb::/64")
	}
}

func TestAllocateIPv6BindLeaseUsesCIDRWithoutExpansion(t *testing.T) {
	pool := &ProxyPool{
		Name: "dedicated-v6",
		IPv6BindLeaseRanges: []IPv6BindLeaseRange{
			{
				NamePrefix: "acct-v6-",
				CIDR:       "2602:294:0:eb::/126",
			},
		},
	}
	occupied := map[string]struct{}{
		"2602:294:0:eb::":  {},
		"2602:294:0:eb::1": {},
		"2602:294:0:eb::2": {},
	}

	lease, ok := AllocateIPv6BindLease(pool, "auth-c", occupied)
	if !ok {
		t.Fatal("expected lease allocation to succeed")
	}
	if lease.IP != "2602:294:0:eb::3" {
		t.Fatalf("lease.IP = %q, want %q", lease.IP, "2602:294:0:eb::3")
	}
	if lease.URL != "bind://[2602:294:0:eb::3]" {
		t.Fatalf("lease.URL = %q, want bind URL", lease.URL)
	}
	if lease.EntryName == "" {
		t.Fatal("expected generated lease entry name")
	}
}

func TestIPv6BindLeasePoolContainsConfiguredCIDR(t *testing.T) {
	pool := &ProxyPool{
		Name: "dedicated-v6",
		IPv6BindLeaseRanges: []IPv6BindLeaseRange{
			{CIDR: "2602:294:0:eb::/64"},
		},
	}

	if !IPv6BindLeasePoolContains(pool, "2602:294:0:eb::abcd") {
		t.Fatal("expected address inside /64 to be allowed")
	}
	if IPv6BindLeasePoolContains(pool, "2602:294:0:ec::abcd") {
		t.Fatal("expected address outside /64 to be rejected")
	}
}
