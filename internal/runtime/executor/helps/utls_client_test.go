package helps

import (
	"context"
	"net/http"
	"testing"

	tls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// codexClientHelloID is the macOS-Safari default + profile mapping the codex
// utls feature depends on. A mis-mapped case would ship a wrong fingerprint
// silently, so lock every branch including the empty/whitespace/unknown -> Safari
// fallback and nil cfg.
func TestCodexClientHelloID(t *testing.T) {
	cases := []struct {
		profile string
		want    tls.ClientHelloID
	}{
		{"", tls.HelloSafari_Auto},
		{"safari", tls.HelloSafari_Auto},
		{"  Safari ", tls.HelloSafari_Auto},
		{"SAFARI", tls.HelloSafari_Auto},
		{"chrome", tls.HelloChrome_Auto},
		{"  chrome", tls.HelloChrome_Auto},
		{"firefox", tls.HelloFirefox_Auto},
		{"ios", tls.HelloIOS_Auto},
		{"bogus", tls.HelloSafari_Auto},
	}
	for _, c := range cases {
		cfg := &config.Config{}
		cfg.CodexHeaderDefaults.UTLSProfile = c.profile
		if got := codexClientHelloID(cfg); got != c.want {
			t.Errorf("codexClientHelloID(%q) = %v, want %v", c.profile, got, c.want)
		}
	}
	if got := codexClientHelloID(nil); got != tls.HelloSafari_Auto {
		t.Errorf("codexClientHelloID(nil) = %v, want Safari", got)
	}
}

type recordingRT struct {
	label string
	hit   *string
}

func (r recordingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	*r.hit = r.label
	return &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header), Request: req}, nil
}

// fallbackRoundTripper must route only https chatgpt.com to utls and everything
// else (other hosts, plain http) to the fallback — the exact mechanism the
// feature exists to provide. A host/scheme/case bug would silently leak codex
// traffic onto Go-stdlib TLS.
func TestFallbackRoundTripperRouting(t *testing.T) {
	var hit string
	frt := &fallbackRoundTripper{
		utls:     recordingRT{label: "utls", hit: &hit},
		fallback: recordingRT{label: "fallback", hit: &hit},
		hosts:    codexHosts,
	}
	cases := []struct {
		url  string
		want string
	}{
		{"https://chatgpt.com/backend-api/codex/responses", "utls"},
		{"https://CHATGPT.COM/backend-api/codex", "utls"}, // case-insensitive
		{"https://api.openai.com/v1/x", "fallback"},
		{"https://auth.openai.com/oauth/token", "fallback"},
		{"http://chatgpt.com/backend-api/codex", "fallback"}, // scheme gate
	}
	for _, c := range cases {
		hit = ""
		req, err := http.NewRequest(http.MethodGet, c.url, nil)
		if err != nil {
			t.Fatalf("new request %q: %v", c.url, err)
		}
		if _, err := frt.RoundTrip(req); err != nil {
			t.Fatalf("RoundTrip %q: %v", c.url, err)
		}
		if hit != c.want {
			t.Errorf("RoundTrip %q routed to %q, want %q", c.url, hit, c.want)
		}
	}
}

// NewCodexHTTPClientWithResolution must be a true no-op when UpstreamUTLS is off
// (off-path returns a non-codex-utls transport), and when on must produce the
// codex utls fallback transport pinned to HTTP/1.1 by default / HTTP/2 when
// UTLSHTTP2 is set.
func TestNewCodexHTTPClientGating(t *testing.T) {
	asCodexUtls := func(c *http.Client) (interface{ CodexUtlsHTTP1() bool }, bool) {
		v, ok := c.Transport.(interface{ CodexUtlsHTTP1() bool })
		return v, ok
	}

	// Flag OFF -> not the codex utls transport.
	off := &config.Config{}
	off.CodexHeaderDefaults.UpstreamUTLS = false
	client, _ := NewCodexHTTPClientWithResolution(context.Background(), off, nil, 0)
	if _, ok := asCodexUtls(client); ok {
		t.Fatalf("UpstreamUTLS=false should not yield a codex utls transport")
	}

	// Flag ON, default -> codex utls transport, HTTP/1.1 pinned.
	on := &config.Config{}
	on.CodexHeaderDefaults.UpstreamUTLS = true
	client, _ = NewCodexHTTPClientWithResolution(context.Background(), on, nil, 0)
	v, ok := asCodexUtls(client)
	if !ok {
		t.Fatalf("UpstreamUTLS=true should yield a codex utls transport")
	}
	if !v.CodexUtlsHTTP1() {
		t.Errorf("default codex utls transport should be HTTP/1.1-pinned")
	}
	if frt, okT := client.Transport.(*fallbackRoundTripper); !okT {
		t.Fatalf("transport type = %T, want *fallbackRoundTripper", client.Transport)
	} else if _, okHost := frt.hosts["chatgpt.com"]; !okHost {
		t.Errorf("codex utls transport hosts = %v, want chatgpt.com", frt.hosts)
	}

	// Flag ON + UTLSHTTP2 -> codex utls transport, HTTP/2 (not h1-pinned).
	h2 := &config.Config{}
	h2.CodexHeaderDefaults.UpstreamUTLS = true
	h2.CodexHeaderDefaults.UTLSHTTP2 = true
	client, _ = NewCodexHTTPClientWithResolution(context.Background(), h2, nil, 0)
	v, ok = asCodexUtls(client)
	if !ok {
		t.Fatalf("UpstreamUTLS=true (h2) should still yield a codex utls transport")
	}
	if v.CodexUtlsHTTP1() {
		t.Errorf("UTLSHTTP2=true should NOT be HTTP/1.1-pinned")
	}
}

// forceHTTP1ALPN must pin the ClientHello's ALPN to http/1.1 only, so the
// impersonated socket never negotiates h2 (which would re-expose a Go-net h2
// fingerprint).
func TestForceHTTP1ALPN(t *testing.T) {
	spec, err := tls.UTLSIdToSpec(tls.HelloChrome_Auto)
	if err != nil {
		t.Fatalf("UTLSIdToSpec: %v", err)
	}
	forceHTTP1ALPN(&spec)
	found := false
	for _, ext := range spec.Extensions {
		if alpn, ok := ext.(*tls.ALPNExtension); ok {
			found = true
			if len(alpn.AlpnProtocols) != 1 || alpn.AlpnProtocols[0] != "http/1.1" {
				t.Errorf("ALPN = %v, want [http/1.1]", alpn.AlpnProtocols)
			}
		}
	}
	if !found {
		t.Fatalf("Chrome ClientHello spec had no ALPN extension to pin")
	}
}

// The native-Go re-login client must present a real-browser TLS fingerprint for
// the OpenAI login-surface hosts (chatgpt.com / auth.openai.com / sentinel) to
// clear Cloudflare, while the Microsoft Graph mailbox (email-OTP flow) and every
// other host stay on the standard transport.
func TestOpenAILoginHostsRouteUtls(t *testing.T) {
	var hit string
	frt := &fallbackRoundTripper{
		utls:     recordingRT{label: "utls", hit: &hit},
		fallback: recordingRT{label: "fallback", hit: &hit},
		hosts:    openaiLoginHosts,
	}
	cases := []struct {
		url  string
		want string
	}{
		{"https://chatgpt.com/api/auth/session", "utls"},
		{"https://AUTH.OPENAI.COM/api/accounts/password/verify", "utls"}, // case-insensitive
		{"https://sentinel.openai.com/api/sentinel", "utls"},
		{"https://graph.microsoft.com/v1.0/me/messages", "fallback"},
		{"https://example.com/", "fallback"},
		{"http://chatgpt.com/api/auth/session", "fallback"}, // scheme gate
	}
	for _, c := range cases {
		hit = ""
		req, err := http.NewRequest(http.MethodGet, c.url, nil)
		if err != nil {
			t.Fatalf("new request %q: %v", c.url, err)
		}
		if _, err := frt.RoundTrip(req); err != nil {
			t.Fatalf("RoundTrip %q: %v", c.url, err)
		}
		if hit != c.want {
			t.Errorf("RoundTrip %q routed to %q, want %q", c.url, hit, c.want)
		}
	}
}
