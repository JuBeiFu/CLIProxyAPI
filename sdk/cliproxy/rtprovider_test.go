package cliproxy

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestRoundTripperForDirectBypassesProxy(t *testing.T) {
	t.Parallel()

	provider := newDefaultRoundTripperProvider(nil)
	rt := provider.RoundTripperFor(&coreauth.Auth{ProxyURL: "direct"})
	transport, ok := rt.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", rt)
	}
	if transport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestRoundTripperForProxyPoolUsesResolvedProxy(t *testing.T) {
	t.Parallel()

	provider := newDefaultRoundTripperProvider(&config.Config{
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
	})
	rt := provider.RoundTripperFor(&coreauth.Auth{ID: "auth-1"})
	transport, ok := rt.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", rt)
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
