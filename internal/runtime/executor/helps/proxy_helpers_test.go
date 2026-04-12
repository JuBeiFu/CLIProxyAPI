package helps

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
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
