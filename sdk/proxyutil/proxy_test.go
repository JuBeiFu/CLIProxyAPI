package proxyutil

import (
	"net"
	"net/http"
	"runtime"
	"testing"
	"time"
)

func mustDefaultTransport(t *testing.T) *http.Transport {
	t.Helper()

	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatal("http.DefaultTransport is not an *http.Transport")
	}
	return transport
}

func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    Mode
		wantErr bool
	}{
		{name: "inherit", input: "", want: ModeInherit},
		{name: "direct", input: "direct", want: ModeDirect},
		{name: "none", input: "none", want: ModeDirect},
		{name: "http", input: "http://proxy.example.com:8080", want: ModeProxy},
		{name: "https", input: "https://proxy.example.com:8443", want: ModeProxy},
		{name: "socks5", input: "socks5://proxy.example.com:1080", want: ModeProxy},
		{name: "socks5h", input: "socks5h://proxy.example.com:1080", want: ModeProxy},
		{name: "bind ipv6", input: "bind://[2602:294:0:eb::100]", want: ModeBind},
		{name: "bind ipv6 port", input: "bind://[2602:294:0:eb::100]:0", want: ModeBind},
		{name: "invalid", input: "bad-value", want: ModeInvalid, wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			setting, errParse := Parse(tt.input)
			if tt.wantErr && errParse == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && errParse != nil {
				t.Fatalf("unexpected error: %v", errParse)
			}
			if setting.Mode != tt.want {
				t.Fatalf("mode = %d, want %d", setting.Mode, tt.want)
			}
		})
	}
}

func TestBuildHTTPTransportDirectBypassesProxy(t *testing.T) {
	t.Parallel()

	transport, mode, errBuild := BuildHTTPTransport("direct")
	if errBuild != nil {
		t.Fatalf("BuildHTTPTransport returned error: %v", errBuild)
	}
	if mode != ModeDirect {
		t.Fatalf("mode = %d, want %d", mode, ModeDirect)
	}
	if transport == nil {
		t.Fatal("expected transport, got nil")
	}
	if transport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
	if transport.MaxIdleConns < 4096 {
		t.Fatalf("MaxIdleConns = %d, want at least 4096", transport.MaxIdleConns)
	}
	if transport.MaxIdleConnsPerHost < 1024 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want at least 1024", transport.MaxIdleConnsPerHost)
	}
}

func TestBuildHTTPTransportHTTPProxy(t *testing.T) {
	t.Parallel()

	transport, mode, errBuild := BuildHTTPTransport("http://proxy.example.com:8080")
	if errBuild != nil {
		t.Fatalf("BuildHTTPTransport returned error: %v", errBuild)
	}
	if mode != ModeProxy {
		t.Fatalf("mode = %d, want %d", mode, ModeProxy)
	}
	if transport == nil {
		t.Fatal("expected transport, got nil")
	}

	req, errRequest := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if errRequest != nil {
		t.Fatalf("http.NewRequest returned error: %v", errRequest)
	}

	proxyURL, errProxy := transport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("transport.Proxy returned error: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://proxy.example.com:8080" {
		t.Fatalf("proxy URL = %v, want http://proxy.example.com:8080", proxyURL)
	}

	defaultTransport := mustDefaultTransport(t)
	if transport.ForceAttemptHTTP2 != defaultTransport.ForceAttemptHTTP2 {
		t.Fatalf("ForceAttemptHTTP2 = %v, want %v", transport.ForceAttemptHTTP2, defaultTransport.ForceAttemptHTTP2)
	}
	if transport.IdleConnTimeout != defaultTransport.IdleConnTimeout {
		t.Fatalf("IdleConnTimeout = %v, want %v", transport.IdleConnTimeout, defaultTransport.IdleConnTimeout)
	}
	if transport.TLSHandshakeTimeout != defaultTransport.TLSHandshakeTimeout {
		t.Fatalf("TLSHandshakeTimeout = %v, want %v", transport.TLSHandshakeTimeout, defaultTransport.TLSHandshakeTimeout)
	}
	if transport.MaxIdleConns < 4096 {
		t.Fatalf("MaxIdleConns = %d, want at least 4096", transport.MaxIdleConns)
	}
	if transport.MaxIdleConnsPerHost < 1024 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want at least 1024", transport.MaxIdleConnsPerHost)
	}
}

func TestBuildHTTPTransportSOCKS5ProxyInheritsDefaultTransportSettings(t *testing.T) {
	t.Parallel()

	transport, mode, errBuild := BuildHTTPTransport("socks5://proxy.example.com:1080")
	if errBuild != nil {
		t.Fatalf("BuildHTTPTransport returned error: %v", errBuild)
	}
	if mode != ModeProxy {
		t.Fatalf("mode = %d, want %d", mode, ModeProxy)
	}
	if transport == nil {
		t.Fatal("expected transport, got nil")
	}
	if transport.Proxy != nil {
		t.Fatal("expected SOCKS5 transport to bypass http proxy function")
	}

	defaultTransport := mustDefaultTransport(t)
	if transport.ForceAttemptHTTP2 != defaultTransport.ForceAttemptHTTP2 {
		t.Fatalf("ForceAttemptHTTP2 = %v, want %v", transport.ForceAttemptHTTP2, defaultTransport.ForceAttemptHTTP2)
	}
	if transport.IdleConnTimeout != defaultTransport.IdleConnTimeout {
		t.Fatalf("IdleConnTimeout = %v, want %v", transport.IdleConnTimeout, defaultTransport.IdleConnTimeout)
	}
	if transport.TLSHandshakeTimeout != defaultTransport.TLSHandshakeTimeout {
		t.Fatalf("TLSHandshakeTimeout = %v, want %v", transport.TLSHandshakeTimeout, defaultTransport.TLSHandshakeTimeout)
	}
	if transport.MaxIdleConns < 4096 {
		t.Fatalf("MaxIdleConns = %d, want at least 4096", transport.MaxIdleConns)
	}
	if transport.MaxIdleConnsPerHost < 1024 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want at least 1024", transport.MaxIdleConnsPerHost)
	}
}

func TestBuildHTTPTransportSOCKS5HProxy(t *testing.T) {
	t.Parallel()

	transport, mode, errBuild := BuildHTTPTransport("socks5h://proxy.example.com:1080")
	if errBuild != nil {
		t.Fatalf("BuildHTTPTransport returned error: %v", errBuild)
	}
	if mode != ModeProxy {
		t.Fatalf("mode = %d, want %d", mode, ModeProxy)
	}
	if transport == nil {
		t.Fatal("expected transport, got nil")
	}
	if transport.Proxy != nil {
		t.Fatal("expected SOCKS5H transport to bypass http proxy function")
	}
	if transport.DialContext == nil {
		t.Fatal("expected SOCKS5H transport to have custom DialContext")
	}
}

func TestBuildHTTPTransportBindIPv6UsesLocalAddr(t *testing.T) {
	t.Parallel()

	transport, mode, errBuild := BuildHTTPTransport("bind://[2602:294:0:eb::100]")
	if errBuild != nil {
		t.Fatalf("BuildHTTPTransport returned error: %v", errBuild)
	}
	if mode != ModeBind {
		t.Fatalf("mode = %d, want %d", mode, ModeBind)
	}
	if transport == nil {
		t.Fatal("expected transport, got nil")
	}
	if transport.Proxy != nil {
		t.Fatal("expected bind transport to bypass http proxy function")
	}
	if transport.DialContext == nil {
		t.Fatal("expected bind transport to have custom DialContext")
	}
	defaultTransport := mustDefaultTransport(t)
	if transport.ForceAttemptHTTP2 != defaultTransport.ForceAttemptHTTP2 {
		t.Fatalf("ForceAttemptHTTP2 = %v, want %v", transport.ForceAttemptHTTP2, defaultTransport.ForceAttemptHTTP2)
	}
}

func TestBuildDialerBindIPv6UsesLocalAddr(t *testing.T) {
	t.Parallel()

	dialer, mode, errBuild := BuildDialer("bind://[2602:294:0:eb::100]:12345")
	if errBuild != nil {
		t.Fatalf("BuildDialer returned error: %v", errBuild)
	}
	if mode != ModeBind {
		t.Fatalf("mode = %d, want %d", mode, ModeBind)
	}
	netDialer, ok := dialer.(*net.Dialer)
	if !ok {
		t.Fatalf("dialer type = %T, want *net.Dialer", dialer)
	}
	localAddr, ok := netDialer.LocalAddr.(*net.TCPAddr)
	if !ok {
		t.Fatalf("LocalAddr type = %T, want *net.TCPAddr", netDialer.LocalAddr)
	}
	if got := localAddr.IP.String(); got != "2602:294:0:eb::100" {
		t.Fatalf("LocalAddr IP = %q, want 2602:294:0:eb::100", got)
	}
	if localAddr.Port != 12345 {
		t.Fatalf("LocalAddr port = %d, want 12345", localAddr.Port)
	}
	if netDialer.Timeout != 30*time.Second {
		t.Fatalf("Timeout = %v, want 30s", netDialer.Timeout)
	}
	if netDialer.KeepAlive != 30*time.Second {
		t.Fatalf("KeepAlive = %v, want 30s", netDialer.KeepAlive)
	}
	if runtime.GOOS == "linux" && netDialer.Control == nil {
		t.Fatal("expected bind dialer to install socket control")
	}
}
