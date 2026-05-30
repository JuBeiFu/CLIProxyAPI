package helps

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	codexauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/proxypool"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type probeStatusError struct {
	status  int
	message string
}

func (e probeStatusError) Error() string { return e.message }

func (e probeStatusError) StatusCode() int { return e.status }

func TestProbeCodexPlanAcrossPoolPrefersExistingBoundEntryWhenStillPaid(t *testing.T) {
	orig := fetchUsageInfoWithProxy
	defer func() { fetchUsageInfoWithProxy = orig }()

	var requests []string
	fetchUsageInfoWithProxy = func(ctx context.Context, proxyURL, accessToken string) (codexauth.WhamUsageInfo, error) {
		requests = append(requests, proxyURL)
		switch proxyURL {
		case "http://bound-node":
			return codexauth.WhamUsageInfo{PlanType: "plus"}, nil
		case "http://other-node":
			return codexauth.WhamUsageInfo{PlanType: "free"}, nil
		case "":
			return codexauth.WhamUsageInfo{PlanType: "free"}, nil
		default:
			return codexauth.WhamUsageInfo{}, fmt.Errorf("unexpected proxyURL %q", proxyURL)
		}
	}

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			DefaultProxyPool: "free-egress",
			ProxyPools: []config.ProxyPool{
				{
					Name: "free-egress",
					Entries: []config.ProxyPoolEntry{
						{Name: "bound-node", URL: "http://bound-node"},
						{Name: "other-node", URL: "http://other-node"},
					},
				},
			},
		},
	}
	auth := &cliproxyauth.Auth{
		ID:        "auth-1",
		Provider:  "codex",
		ProxyPool: "free-egress",
		Metadata:  map[string]any{},
	}
	cliproxyauth.SetBoundProxyEntry(auth, "bound-node")

	plan, bound, _, _, _, ok, err := ProbeCodexPlanAcrossPool(context.Background(), cfg, auth, "token")
	if err != nil {
		t.Fatalf("ProbeCodexPlanAcrossPool error = %v", err)
	}
	if !ok {
		t.Fatal("probeOK = false, want true")
	}
	if plan != "plus" {
		t.Fatalf("plan = %q, want %q", plan, "plus")
	}
	if bound != "bound-node" {
		t.Fatalf("bound = %q, want %q", bound, "bound-node")
	}
	if len(requests) != 1 || requests[0] != "http://bound-node" {
		t.Fatalf("requests = %v, want only bound probe", requests)
	}
}

func TestProbeCodexPlanAcrossPoolFallsBackWhenExistingBoundEntryIsFree(t *testing.T) {
	orig := fetchUsageInfoWithProxy
	defer func() { fetchUsageInfoWithProxy = orig }()

	var requests []string
	fetchUsageInfoWithProxy = func(ctx context.Context, proxyURL, accessToken string) (codexauth.WhamUsageInfo, error) {
		requests = append(requests, proxyURL)
		switch proxyURL {
		case "http://bound-node":
			return codexauth.WhamUsageInfo{PlanType: "free"}, nil
		case "http://other-node":
			return codexauth.WhamUsageInfo{PlanType: "plus"}, nil
		case "":
			return codexauth.WhamUsageInfo{PlanType: "free"}, nil
		default:
			return codexauth.WhamUsageInfo{}, fmt.Errorf("unexpected proxyURL %q", proxyURL)
		}
	}

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			DefaultProxyPool: "free-egress",
			ProxyPools: []config.ProxyPool{
				{
					Name: "free-egress",
					Entries: []config.ProxyPoolEntry{
						{Name: "bound-node", URL: "http://bound-node"},
						{Name: "other-node", URL: "http://other-node"},
					},
				},
			},
		},
	}
	auth := &cliproxyauth.Auth{
		ID:        "auth-1",
		Provider:  "codex",
		ProxyPool: "free-egress",
		Metadata:  map[string]any{},
	}
	cliproxyauth.SetBoundProxyEntry(auth, "bound-node")

	plan, bound, _, _, _, ok, err := ProbeCodexPlanAcrossPool(context.Background(), cfg, auth, "token")
	if err != nil {
		t.Fatalf("ProbeCodexPlanAcrossPool error = %v", err)
	}
	if !ok {
		t.Fatal("probeOK = false, want true")
	}
	if plan != "plus" {
		t.Fatalf("plan = %q, want %q", plan, "plus")
	}
	if bound != "other-node" {
		t.Fatalf("bound = %q, want %q", bound, "other-node")
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %v, want 2 probes", requests)
	}
	if requests[0] != "http://bound-node" {
		t.Fatalf("first request = %q, want %q", requests[0], "http://bound-node")
	}
}

func TestProbeCodexPlanAcrossPoolReturnsSupportedModelsFromPaidPath(t *testing.T) {
	orig := fetchUsageInfoWithProxy
	defer func() { fetchUsageInfoWithProxy = orig }()

	fetchUsageInfoWithProxy = func(ctx context.Context, proxyURL, accessToken string) (codexauth.WhamUsageInfo, error) {
		return codexauth.WhamUsageInfo{
			PlanType:        "plus",
			SupportedModels: []string{"gpt-5.4", "gpt-5.5"},
		}, nil
	}

	plan, _, models, _, _, ok, err := ProbeCodexPlanAcrossPool(context.Background(), &config.Config{}, &cliproxyauth.Auth{Provider: "codex"}, "token")
	if err != nil {
		t.Fatalf("ProbeCodexPlanAcrossPool error = %v", err)
	}
	if !ok {
		t.Fatal("probeOK = false, want true")
	}
	if plan != "plus" {
		t.Fatalf("plan = %q, want plus", plan)
	}
	if len(models) != 2 || models[0] != "gpt-5.4" || models[1] != "gpt-5.5" {
		t.Fatalf("models = %v, want [gpt-5.4 gpt-5.5]", models)
	}
}

func TestProbeCodexPlanAcrossPoolReturnsWeeklyQuotaFromPaidPath(t *testing.T) {
	orig := fetchUsageInfoWithProxy
	defer func() { fetchUsageInfoWithProxy = orig }()

	weeklyQuota := &codexauth.WhamQuotaWindow{RemainingRatio: 0}
	fetchUsageInfoWithProxy = func(ctx context.Context, proxyURL, accessToken string) (codexauth.WhamUsageInfo, error) {
		return codexauth.WhamUsageInfo{
			PlanType:    "plus",
			WeeklyQuota: weeklyQuota,
		}, nil
	}

	plan, _, _, _, gotWeeklyQuota, ok, err := ProbeCodexPlanAcrossPool(context.Background(), &config.Config{}, &cliproxyauth.Auth{Provider: "codex"}, "token")
	if err != nil {
		t.Fatalf("ProbeCodexPlanAcrossPool error = %v", err)
	}
	if !ok {
		t.Fatal("probeOK = false, want true")
	}
	if plan != "plus" {
		t.Fatalf("plan = %q, want plus", plan)
	}
	if gotWeeklyQuota != weeklyQuota {
		t.Fatalf("weekly quota = %+v, want %+v", gotWeeklyQuota, weeklyQuota)
	}
}

func TestProbeCodexPlanAcrossPoolReturnsUsageFromFreePath(t *testing.T) {
	orig := fetchUsageInfoWithProxy
	defer func() { fetchUsageInfoWithProxy = orig }()

	weeklyQuota := &codexauth.WhamQuotaWindow{RemainingRatio: 0.75}
	fetchUsageInfoWithProxy = func(ctx context.Context, proxyURL, accessToken string) (codexauth.WhamUsageInfo, error) {
		return codexauth.WhamUsageInfo{
			PlanType:        "free",
			SupportedModels: []string{"gpt-5.4-mini"},
			WeeklyQuota:     weeklyQuota,
		}, nil
	}

	plan, bound, models, fiveHourQuota, gotWeeklyQuota, ok, err := ProbeCodexPlanAcrossPool(context.Background(), &config.Config{}, &cliproxyauth.Auth{Provider: "codex"}, "token")
	if err != nil {
		t.Fatalf("ProbeCodexPlanAcrossPool error = %v", err)
	}
	if !ok {
		t.Fatal("probeOK = false, want true")
	}
	if plan != "free" || bound != "" {
		t.Fatalf("plan/bound = %q/%q, want free/empty", plan, bound)
	}
	if len(models) != 1 || models[0] != "gpt-5.4-mini" {
		t.Fatalf("models = %v, want [gpt-5.4-mini]", models)
	}
	if fiveHourQuota != nil {
		t.Fatalf("five-hour quota = %+v, want nil", fiveHourQuota)
	}
	if gotWeeklyQuota != weeklyQuota {
		t.Fatalf("weekly quota = %+v, want %+v", gotWeeklyQuota, weeklyQuota)
	}
}

func TestProbeCodexPlanAcrossPoolReturnsTerminalAuthErrorWhenAllPathsUnauthorized(t *testing.T) {
	orig := fetchUsageInfoWithProxy
	defer func() { fetchUsageInfoWithProxy = orig }()

	fetchUsageInfoWithProxy = func(ctx context.Context, proxyURL, accessToken string) (codexauth.WhamUsageInfo, error) {
		return codexauth.WhamUsageInfo{}, probeStatusError{
			status:  http.StatusUnauthorized,
			message: `codex: /wham/usage status 401: {"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","code":"token_invalidated"}}`,
		}
	}

	_, _, _, _, _, ok, err := ProbeCodexPlanAcrossPool(context.Background(), &config.Config{}, &cliproxyauth.Auth{Provider: "codex"}, "token")
	if ok {
		t.Fatalf("probeOK = true, want false")
	}
	if err == nil {
		t.Fatalf("expected terminal auth error")
	}
	statusProvider, okStatus := err.(interface{ StatusCode() int })
	if !okStatus {
		t.Fatalf("expected returned error to expose StatusCode")
	}
	if got := statusProvider.StatusCode(); got != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", got, http.StatusUnauthorized)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "token_invalidated") {
		t.Fatalf("error = %q, want token_invalidated", err.Error())
	}
}

func TestProbeCodexPlanAcrossPoolReturnsTerminalAuthErrorWithMixedProbeFailures(t *testing.T) {
	orig := fetchUsageInfoWithProxy
	defer func() { fetchUsageInfoWithProxy = orig }()

	fetchUsageInfoWithProxy = func(ctx context.Context, proxyURL, accessToken string) (codexauth.WhamUsageInfo, error) {
		if proxyURL == "http://slow-node" {
			return codexauth.WhamUsageInfo{}, fmt.Errorf("proxy timeout")
		}
		return codexauth.WhamUsageInfo{}, probeStatusError{
			status:  http.StatusUnauthorized,
			message: `codex: /wham/usage status 401: {"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","code":"token_invalidated"}}`,
		}
	}

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			DefaultProxyPool: "free-egress",
			ProxyPools: []config.ProxyPool{
				{
					Name: "free-egress",
					Entries: []config.ProxyPoolEntry{
						{Name: "slow-node", URL: "http://slow-node"},
					},
				},
			},
		},
	}

	_, _, _, _, _, ok, err := ProbeCodexPlanAcrossPool(context.Background(), cfg, &cliproxyauth.Auth{Provider: "codex", ProxyPool: "free-egress"}, "token")
	if ok {
		t.Fatalf("probeOK = true, want false")
	}
	if err == nil {
		t.Fatalf("expected terminal auth error")
	}
	statusProvider, okStatus := err.(interface{ StatusCode() int })
	if !okStatus || statusProvider.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("expected status 401 terminal error, got %T %v", err, err)
	}
}

func TestProbeCodexPlanAcrossPoolStartsWithStablePreferredEntryWhenUnbound(t *testing.T) {
	orig := fetchUsageInfoWithProxy
	defer func() { fetchUsageInfoWithProxy = orig }()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			DefaultProxyPool: "free-egress",
			ProxyPools: []config.ProxyPool{
				{
					Name: "free-egress",
					Entries: []config.ProxyPoolEntry{
						{Name: "proxy-a", URL: "http://proxy-a"},
						{Name: "proxy-b", URL: "http://proxy-b"},
						{Name: "proxy-c", URL: "http://proxy-c"},
					},
				},
			},
		},
	}
	auth := &cliproxyauth.Auth{
		ID:       "auth-stable",
		Provider: "codex",
		Metadata: map[string]any{},
	}
	pool := cfg.ProxyPoolByName("free-egress")
	expected, ok := proxypool.SelectPoolEntryWithHealth(pool, auth, proxypool.DefaultHealthManager())
	if !ok {
		t.Fatal("expected stable proxy-pool selection")
	}

	var requests []string
	fetchUsageInfoWithProxy = func(ctx context.Context, proxyURL, accessToken string) (codexauth.WhamUsageInfo, error) {
		requests = append(requests, proxyURL)
		if proxyURL == expected.URL {
			return codexauth.WhamUsageInfo{PlanType: "plus"}, nil
		}
		return codexauth.WhamUsageInfo{PlanType: "free"}, nil
	}

	plan, bound, _, _, _, ok, err := ProbeCodexPlanAcrossPool(context.Background(), cfg, auth, "token")
	if err != nil {
		t.Fatalf("ProbeCodexPlanAcrossPool error = %v", err)
	}
	if !ok {
		t.Fatal("probeOK = false, want true")
	}
	if plan != "plus" {
		t.Fatalf("plan = %q, want %q", plan, "plus")
	}
	if bound != expected.Name {
		t.Fatalf("bound = %q, want %q", bound, expected.Name)
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %v, want exactly one probe", requests)
	}
	if requests[0] != expected.URL {
		t.Fatalf("first request = %q, want %q", requests[0], expected.URL)
	}
}

func TestProbeCodexPlanAcrossPoolStartsWithIPv6BindLease(t *testing.T) {
	orig := fetchUsageInfoWithProxy
	defer func() { fetchUsageInfoWithProxy = orig }()

	var requests []string
	fetchUsageInfoWithProxy = func(ctx context.Context, proxyURL, accessToken string) (codexauth.WhamUsageInfo, error) {
		requests = append(requests, proxyURL)
		if proxyURL == "bind://[2602:294:0:eb::42]" {
			return codexauth.WhamUsageInfo{PlanType: "plus"}, nil
		}
		return codexauth.WhamUsageInfo{PlanType: "free"}, nil
	}

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
						{Name: "proxy-a", URL: "http://proxy-a"},
					},
				},
			},
		},
	}
	auth := &cliproxyauth.Auth{
		ID:       "auth-ipv6-lease",
		Provider: "codex",
		Metadata: map[string]any{},
	}
	cliproxyauth.SetIPv6BindLease(auth, cliproxyauth.IPv6BindLeaseInfo{
		Pool:      "dedicated-v6",
		EntryName: "acct-v6-auth",
		IP:        "2602:294:0:eb::42",
		URL:       "bind://[2602:294:0:eb::42]",
	})

	plan, bound, _, _, _, ok, err := ProbeCodexPlanAcrossPool(context.Background(), cfg, auth, "token")
	if err != nil {
		t.Fatalf("ProbeCodexPlanAcrossPool error = %v", err)
	}
	if !ok {
		t.Fatal("probeOK = false, want true")
	}
	if plan != "plus" {
		t.Fatalf("plan = %q, want plus", plan)
	}
	if bound != "" {
		t.Fatalf("bound = %q, want empty legacy bound entry for IPv6 lease", bound)
	}
	if len(requests) != 1 || requests[0] != "bind://[2602:294:0:eb::42]" {
		t.Fatalf("requests = %v, want only IPv6 lease probe", requests)
	}
}

func TestProbeCodexPlanAcrossPoolWithIPv6BindLeaseFallsBackToPoolEntries(t *testing.T) {
	orig := fetchUsageInfoWithProxy
	defer func() { fetchUsageInfoWithProxy = orig }()

	var requests []string
	fetchUsageInfoWithProxy = func(ctx context.Context, proxyURL, accessToken string) (codexauth.WhamUsageInfo, error) {
		requests = append(requests, proxyURL)
		if proxyURL == "bind://[2602:294:0:eb::42]" {
			return codexauth.WhamUsageInfo{PlanType: "free"}, nil
		}
		return codexauth.WhamUsageInfo{PlanType: "plus"}, nil
	}

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
						{Name: "proxy-a", URL: "http://proxy-a"},
					},
				},
			},
		},
	}
	auth := &cliproxyauth.Auth{
		ID:       "auth-ipv6-lease-free",
		Provider: "codex",
		Metadata: map[string]any{},
	}
	cliproxyauth.SetIPv6BindLease(auth, cliproxyauth.IPv6BindLeaseInfo{
		Pool:      "dedicated-v6",
		EntryName: "acct-v6-auth",
		IP:        "2602:294:0:eb::42",
		URL:       "bind://[2602:294:0:eb::42]",
	})

	plan, bound, _, _, _, ok, err := ProbeCodexPlanAcrossPool(context.Background(), cfg, auth, "token")
	if err != nil {
		t.Fatalf("ProbeCodexPlanAcrossPool error = %v", err)
	}
	if !ok {
		t.Fatal("probeOK = false, want true")
	}
	if plan != "plus" {
		t.Fatalf("plan = %q, want plus", plan)
	}
	if bound != "proxy-a" {
		t.Fatalf("bound = %q, want proxy-a", bound)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %v, want lease then pool probe", requests)
	}
	if requests[0] != "bind://[2602:294:0:eb::42]" || requests[1] != "http://proxy-a" {
		t.Fatalf("requests = %v, want [lease pool-entry]", requests)
	}
}

func TestProbeCodexPlanAcrossPoolWithBrokenIPv6BindLeaseFallsBackToDirect(t *testing.T) {
	orig := fetchUsageInfoWithProxy
	defer func() { fetchUsageInfoWithProxy = orig }()

	var requests []string
	fetchUsageInfoWithProxy = func(ctx context.Context, proxyURL, accessToken string) (codexauth.WhamUsageInfo, error) {
		requests = append(requests, proxyURL)
		switch proxyURL {
		case "bind://[2602:294:0:eb::42]":
			return codexauth.WhamUsageInfo{}, fmt.Errorf("network is unreachable")
		case "http://proxy-a":
			return codexauth.WhamUsageInfo{}, fmt.Errorf("proxy timeout")
		case "":
			return codexauth.WhamUsageInfo{PlanType: "plus"}, nil
		default:
			return codexauth.WhamUsageInfo{}, fmt.Errorf("unexpected proxyURL %q", proxyURL)
		}
	}

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
						{Name: "proxy-a", URL: "http://proxy-a"},
					},
				},
			},
		},
	}
	auth := &cliproxyauth.Auth{
		ID:       "auth-ipv6-lease-broken",
		Provider: "codex",
		Metadata: map[string]any{},
	}
	cliproxyauth.SetIPv6BindLease(auth, cliproxyauth.IPv6BindLeaseInfo{
		Pool:      "dedicated-v6",
		EntryName: "acct-v6-auth",
		IP:        "2602:294:0:eb::42",
		URL:       "bind://[2602:294:0:eb::42]",
	})

	plan, bound, _, _, _, ok, err := ProbeCodexPlanAcrossPool(context.Background(), cfg, auth, "token")
	if err != nil {
		t.Fatalf("ProbeCodexPlanAcrossPool error = %v", err)
	}
	if !ok {
		t.Fatal("probeOK = false, want true")
	}
	if plan != "plus" {
		t.Fatalf("plan = %q, want plus", plan)
	}
	if bound != cliproxyauth.BoundProxyEntryDirect {
		t.Fatalf("bound = %q, want %q", bound, cliproxyauth.BoundProxyEntryDirect)
	}
	if len(requests) != 3 {
		t.Fatalf("requests = %v, want lease then pool then direct", requests)
	}
	if requests[0] != "bind://[2602:294:0:eb::42]" || requests[1] != "http://proxy-a" || requests[2] != "" {
		t.Fatalf("requests = %v, want [lease pool-entry direct]", requests)
	}
}
