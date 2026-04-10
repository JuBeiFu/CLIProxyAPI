package util

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestSetProxy_EmptyProxyConfigForcesDirectTransport(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://env-proxy.example.com:8080")
	t.Setenv("HTTPS_PROXY", "http://env-proxy.example.com:8080")
	t.Setenv("ALL_PROXY", "socks5://env-proxy.example.com:1080")

	client := SetProxy(&config.SDKConfig{ProxyURL: ""}, &http.Client{})
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected *http.Transport, got %T", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("expected empty proxy config to force direct transport and bypass environment proxy")
	}
}

func TestSetProxy_ExplicitProxyConfigUsesConfiguredTransport(t *testing.T) {
	client := SetProxy(&config.SDKConfig{ProxyURL: "http://proxy.example.com:8080"}, &http.Client{})
	if client.Transport == nil {
		t.Fatal("expected explicit proxy config to install transport")
	}
	if _, ok := client.Transport.(*http.Transport); ok {
		t.Fatal("expected explicit proxy config to use proxy-aware transport rather than direct transport")
	}
}
