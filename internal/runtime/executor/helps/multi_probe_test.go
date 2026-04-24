package helps

import (
	"context"
	"fmt"
	"testing"

	codexauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

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

	plan, bound, _, ok := ProbeCodexPlanAcrossPool(context.Background(), cfg, auth, "token")
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

	plan, bound, _, ok := ProbeCodexPlanAcrossPool(context.Background(), cfg, auth, "token")
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

	plan, _, models, ok := ProbeCodexPlanAcrossPool(context.Background(), &config.Config{}, &cliproxyauth.Auth{Provider: "codex"}, "token")
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
