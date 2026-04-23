package proxypool

import (
	"net/http"
	"testing"
)

func TestBuildHTTPRoundTripperForResolutionReusesDirectTransport(t *testing.T) {
	t.Parallel()

	resolution := Resolution{
		Source:           "codex-awaiting-rebind",
		FallbackToDirect: true,
	}

	first := BuildHTTPRoundTripperForResolution(resolution)
	second := BuildHTTPRoundTripperForResolution(resolution)
	if first == nil || second == nil {
		t.Fatal("expected non-nil round tripper")
	}
	if first != second {
		t.Fatalf("expected cached direct round tripper reuse, got %p and %p", first, second)
	}
}

func TestBuildHTTPRoundTripperForResolutionReusesProxyTransport(t *testing.T) {
	t.Parallel()

	resolution := Resolution{
		ProxyURL: "http://proxy-a.local:8080",
		Source:   "proxy-pool",
	}

	first := BuildHTTPRoundTripperForResolution(resolution)
	second := BuildHTTPRoundTripperForResolution(resolution)
	if first == nil || second == nil {
		t.Fatal("expected non-nil round tripper")
	}
	if first != second {
		t.Fatalf("expected cached proxy round tripper reuse, got %p and %p", first, second)
	}

	transport, ok := first.(*http.Transport)
	if !ok {
		t.Fatalf("round tripper type = %T, want *http.Transport", first)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	proxyURL, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("transport.Proxy returned error: %v", err)
	}
	if proxyURL == nil || proxyURL.String() != resolution.ProxyURL {
		t.Fatalf("proxy URL = %v, want %s", proxyURL, resolution.ProxyURL)
	}
}
