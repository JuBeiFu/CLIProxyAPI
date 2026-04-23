package helps

import (
	"context"
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestProbeCodexPlanAcrossPoolPrefersExistingBoundEntryWhenStillPaid(t *testing.T) {
	orig := fetchPlanTypeWithProxy
	defer func() { fetchPlanTypeWithProxy = orig }()

	var requests []string
	fetchPlanTypeWithProxy = func(ctx context.Context, proxyURL, accessToken string) (string, error) {
		requests = append(requests, proxyURL)
		switch proxyURL {
		case "http://bound-node":
			return "plus", nil
		case "http://other-node":
			return "free", nil
		case "":
			return "free", nil
		default:
			return "", fmt.Errorf("unexpected proxyURL %q", proxyURL)
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

	plan, bound, ok := ProbeCodexPlanAcrossPool(context.Background(), cfg, auth, "token")
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
	orig := fetchPlanTypeWithProxy
	defer func() { fetchPlanTypeWithProxy = orig }()

	var requests []string
	fetchPlanTypeWithProxy = func(ctx context.Context, proxyURL, accessToken string) (string, error) {
		requests = append(requests, proxyURL)
		switch proxyURL {
		case "http://bound-node":
			return "free", nil
		case "http://other-node":
			return "plus", nil
		case "":
			return "free", nil
		default:
			return "", fmt.Errorf("unexpected proxyURL %q", proxyURL)
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

	plan, bound, ok := ProbeCodexPlanAcrossPool(context.Background(), cfg, auth, "token")
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
