package proxypool

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestRequestRouteContextRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := WithRequestRoute(context.Background(), RequestRoute{
		Pool:  "pool-a",
		Entry: "proxy-2",
	})

	route, ok := RequestRouteFromContext(ctx)
	if !ok {
		t.Fatal("RequestRouteFromContext returned ok=false")
	}
	if route.Pool != "pool-a" || route.Entry != "proxy-2" {
		t.Fatalf("route = %+v, want pool-a/proxy-2", route)
	}
}

func TestRequestRouteContextCarriesAssistedIntent(t *testing.T) {
	t.Parallel()

	ctx := WithRequestRoute(context.Background(), RequestRoute{
		Pool:     "pool-a",
		Entry:    "proxy-2",
		Assisted: true,
	})

	route, ok := RequestRouteFromContext(ctx)
	if !ok {
		t.Fatal("RequestRouteFromContext returned ok=false")
	}
	if !route.Assisted {
		t.Fatal("expected Assisted=true on round trip")
	}
}

func TestResolveHonorsRequestLocalRouteOverride(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			DefaultProxyPool: "pool-a",
			ProxyPools: []config.ProxyPool{{
				Name: "pool-a",
				Entries: []config.ProxyPoolEntry{
					{Name: "proxy-1", URL: "socks5://127.0.0.1:10001"},
					{Name: "proxy-2", URL: "socks5://127.0.0.1:10002"},
				},
			}},
		},
	}
	auth := &coreauth.Auth{Provider: "codex", ProxyPool: "pool-a"}
	ctx := WithRequestRoute(context.Background(), RequestRoute{
		Pool:  "pool-a",
		Entry: "proxy-2",
	})

	resolution := ResolveWithContext(ctx, cfg, auth, nil)
	if resolution.ProxyName != "proxy-2" {
		t.Fatalf("ProxyName = %q, want %q", resolution.ProxyName, "proxy-2")
	}
	if resolution.Source != "request-route" {
		t.Fatalf("Source = %q, want %q", resolution.Source, "request-route")
	}
}
