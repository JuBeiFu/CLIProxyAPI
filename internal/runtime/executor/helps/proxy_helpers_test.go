package helps

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/proxypool"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestNewProxyAwareHTTPClientDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	client := NewProxyAwareHTTPClient(
		context.Background(),
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
		0,
	)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestNewProxyAwareHTTPClientUsesDefaultProxyPool(t *testing.T) {
	t.Parallel()

	client := NewProxyAwareHTTPClient(
		context.Background(),
		&config.Config{
			SDKConfig: sdkconfig.SDKConfig{
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
		},
		&cliproxyauth.Auth{ID: "auth-1"},
		0,
	)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	proxyURL, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("transport.Proxy returned error: %v", err)
	}
	if proxyURL == nil || proxyURL.String() != "http://proxy-a.local:8080" {
		t.Fatalf("proxy URL = %v, want http://proxy-a.local:8080", proxyURL)
	}
}

func TestNewProxyAwareHTTPClientWithResolutionUsesDefaultProxyPool(t *testing.T) {
	t.Parallel()

	client, resolution := NewProxyAwareHTTPClientWithResolution(
		context.Background(),
		&config.Config{
			SDKConfig: sdkconfig.SDKConfig{
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
		},
		&cliproxyauth.Auth{ID: "auth-1"},
		0,
	)

	if resolution.Source != "proxy-pool" {
		t.Fatalf("resolution.Source = %q, want %q", resolution.Source, "proxy-pool")
	}
	if resolution.ProxyPool != "shared-egress" {
		t.Fatalf("resolution.ProxyPool = %q, want %q", resolution.ProxyPool, "shared-egress")
	}
	if resolution.ProxyName != "proxy-a" {
		t.Fatalf("resolution.ProxyName = %q, want %q", resolution.ProxyName, "proxy-a")
	}
	if resolution.ProxyURL != "http://proxy-a.local:8080" {
		t.Fatalf("resolution.ProxyURL = %q, want %q", resolution.ProxyURL, "http://proxy-a.local:8080")
	}

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	proxyURL, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("transport.Proxy returned error: %v", err)
	}
	if proxyURL == nil || proxyURL.String() != "http://proxy-a.local:8080" {
		t.Fatalf("proxy URL = %v, want http://proxy-a.local:8080", proxyURL)
	}
}

func TestNewProxyAwareHTTPClientWithResolutionHonorsRequestRouteOverride(t *testing.T) {
	t.Parallel()

	ctx := proxypool.WithRequestRoute(context.Background(), proxypool.RequestRoute{
		Pool:  "shared-egress",
		Entry: "proxy-b",
	})

	client, resolution := NewProxyAwareHTTPClientWithResolution(
		ctx,
		&config.Config{
			SDKConfig: sdkconfig.SDKConfig{
				DefaultProxyPool: "shared-egress",
				ProxyPools: []config.ProxyPool{
					{
						Name: "shared-egress",
						Entries: []config.ProxyPoolEntry{
							{Name: "proxy-a", URL: "http://proxy-a.local:8080"},
							{Name: "proxy-b", URL: "http://proxy-b.local:8080"},
						},
					},
				},
			},
		},
		&cliproxyauth.Auth{ID: "auth-override", Provider: "codex", ProxyPool: "shared-egress"},
		0,
	)

	if resolution.Source != "request-route" {
		t.Fatalf("resolution.Source = %q, want %q", resolution.Source, "request-route")
	}
	if resolution.ProxyName != "proxy-b" {
		t.Fatalf("resolution.ProxyName = %q, want %q", resolution.ProxyName, "proxy-b")
	}

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	proxyURL, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("transport.Proxy returned error: %v", err)
	}
	if proxyURL == nil || proxyURL.String() != "http://proxy-b.local:8080" {
		t.Fatalf("proxy URL = %v, want http://proxy-b.local:8080", proxyURL)
	}
}
