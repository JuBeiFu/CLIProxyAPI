package helps

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	tls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/proxypool"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// utlsRoundTripper implements http.RoundTripper using utls with Chrome fingerprint
// to bypass Cloudflare's TLS fingerprinting on Anthropic domains.
type utlsRoundTripper struct {
	mu          sync.Mutex
	connections map[string]*http2.ClientConn
	pending     map[string]*sync.Cond
	dialer      proxy.Dialer
	helloID     tls.ClientHelloID
}

func newUtlsRoundTripper(proxyURL string) *utlsRoundTripper {
	var dialer proxy.Dialer = proxy.Direct
	if proxyURL != "" {
		proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL)
		if errBuild != nil {
			log.Errorf("utls: failed to configure proxy dialer for %q: %v", proxyURL, errBuild)
		} else if mode != proxyutil.ModeInherit && proxyDialer != nil {
			dialer = proxyDialer
		}
	}
	return &utlsRoundTripper{
		connections: make(map[string]*http2.ClientConn),
		pending:     make(map[string]*sync.Cond),
		dialer:      dialer,
		helloID:     tls.HelloChrome_Auto, // default; codex overrides per config
	}
}

// codexClientHelloID maps the configured codex utls profile to a utls
// ClientHelloID. Default Safari: the real Codex CLI uses reqwest's native-tls
// and its canonical user is macOS (SecureTransport ≈ Safari). Operators whose
// served UA is Windows/Linux should pick chrome/firefox.
func codexClientHelloID(cfg *config.Config) tls.ClientHelloID {
	profile := ""
	if cfg != nil {
		profile = strings.ToLower(strings.TrimSpace(cfg.CodexHeaderDefaults.UTLSProfile))
	}
	switch profile {
	case "chrome":
		return tls.HelloChrome_Auto
	case "firefox":
		return tls.HelloFirefox_Auto
	case "ios":
		return tls.HelloIOS_Auto
	case "safari", "":
		return tls.HelloSafari_Auto
	default:
		return tls.HelloSafari_Auto
	}
}

func (t *utlsRoundTripper) getOrCreateConnection(host, addr string) (*http2.ClientConn, error) {
	t.mu.Lock()

	if h2Conn, ok := t.connections[host]; ok && h2Conn.CanTakeNewRequest() {
		t.mu.Unlock()
		return h2Conn, nil
	}

	if cond, ok := t.pending[host]; ok {
		cond.Wait()
		if h2Conn, ok := t.connections[host]; ok && h2Conn.CanTakeNewRequest() {
			t.mu.Unlock()
			return h2Conn, nil
		}
	}

	cond := sync.NewCond(&t.mu)
	t.pending[host] = cond
	t.mu.Unlock()

	h2Conn, err := t.createConnection(host, addr)

	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.pending, host)
	cond.Broadcast()

	if err != nil {
		return nil, err
	}

	t.connections[host] = h2Conn
	return h2Conn, nil
}

func (t *utlsRoundTripper) createConnection(host, addr string) (*http2.ClientConn, error) {
	conn, err := t.dialer.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{ServerName: host}
	tlsConn := tls.UClient(conn, tlsConfig, t.helloID)

	if err := tlsConn.Handshake(); err != nil {
		conn.Close()
		return nil, err
	}

	tr := &http2.Transport{}
	h2Conn, err := tr.NewClientConn(tlsConn)
	if err != nil {
		tlsConn.Close()
		return nil, err
	}

	return h2Conn, nil
}

func (t *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	hostname := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(hostname, port)

	h2Conn, err := t.getOrCreateConnection(hostname, addr)
	if err != nil {
		return nil, err
	}

	resp, err := h2Conn.RoundTrip(req)
	if err != nil {
		t.mu.Lock()
		if cached, ok := t.connections[hostname]; ok && cached == h2Conn {
			delete(t.connections, hostname)
		}
		t.mu.Unlock()
		return nil, err
	}

	return resp, nil
}

// anthropicHosts contains the hosts that should use utls Chrome TLS fingerprint.
var anthropicHosts = map[string]struct{}{
	"api.anthropic.com": {},
}

// codexHosts are the chatgpt.com hosts whose codex traffic should present a real
// browser TLS fingerprint (utls) instead of Go's stdlib ClientHello, which
// Cloudflare/OpenAI trivially separate from a real client (the cause of served
// codex accounts dying).
var codexHosts = map[string]struct{}{
	"chatgpt.com": {},
}

// fallbackRoundTripper uses utls for the configured HTTPS host set and falls
// back to standard transport for all other requests (non-HTTPS or non-listed).
type fallbackRoundTripper struct {
	utls     http.RoundTripper
	fallback http.RoundTripper
	hosts    map[string]struct{}
	// codexUtlsHTTP1 is true when the utls leg already speaks HTTP/1.1 (the
	// default codex profile). It lets the codex executor report the stream
	// transport correctly and skip its (now redundant) http/1.1 downgrade,
	// instead of mis-typing the client as "http1.1_unsupported".
	codexUtlsHTTP1 bool
}

func (f *fallbackRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "https" {
		if _, ok := f.hosts[strings.ToLower(req.URL.Hostname())]; ok {
			return f.utls.RoundTrip(req)
		}
	}
	return f.fallback.RoundTrip(req)
}

// CodexUtlsHTTP1 reports whether the codex utls leg already negotiates HTTP/1.1
// over a real-browser TLS ClientHello (so there is no Go-net HTTP/2 SETTINGS
// fingerprint to downgrade). Consumed via an interface assertion from the codex
// executor, which lives in a different package and cannot see this type.
func (f *fallbackRoundTripper) CodexUtlsHTTP1() bool { return f.codexUtlsHTTP1 }

// NewUtlsHTTPClient creates an HTTP client using utls Chrome TLS fingerprint.
// Use this for Claude API requests to match real Claude Code's TLS behavior.
// Falls back to standard transport for non-HTTPS requests.
func NewUtlsHTTPClient(cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	resolution := proxypool.Resolve(cfg, auth)
	proxyURL := strings.TrimSpace(resolution.ProxyURL)

	utlsRT := newUtlsRoundTripper(proxyURL)

	var standardTransport http.RoundTripper
	if transport := proxypool.BuildHTTPRoundTripperForResolution(resolution); transport != nil {
		standardTransport = transport
	} else {
		standardTransport = &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		}
	}

	client := &http.Client{
		Transport: &fallbackRoundTripper{
			utls:     utlsRT,
			fallback: standardTransport,
			hosts:    anthropicHosts,
		},
	}
	if timeout > 0 {
		client.Timeout = timeout
	}
	return client
}

// NewCodexUtlsHTTPClient builds a utls-backed client for codex (chatgpt.com)
// requests and returns the proxy resolution alongside, matching the codex
// executor's NewProxyAwareHTTPClientWithResolution call sites.
//
// It takes ctx so the egress is resolved via ResolveWithContext, honoring the
// codex request-route / Assisted / ipv6-bind-lease selection the conductor
// injects into ctx (a context-less Resolve would silently pick a different
// proxy/egress IP and emit wrong resolution telemetry).
//
// chatgpt.com gets a real-browser ClientHello (Safari by default, per config);
// every other host falls back to the standard transport. By default the codex
// utls socket is pinned to HTTP/1.1 (UTLSHTTP2=false): wrapping a browser-TLS
// ClientHello in Go-net's HTTP/2 transport would emit Go's HTTP/2 SETTINGS
// frame, which no real browser/hyper client sends and is itself a strong
// fingerprint contradiction. Forcing HTTP/1.1 removes the HTTP/2 surface
// entirely, mirroring sub2api's tlsfingerprint dialer. Set UTLSHTTP2=true to
// keep HTTP/2 (ALPN h2 matches the real codex client) at the cost of that tell.
func NewCodexUtlsHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) (*http.Client, proxypool.Resolution) {
	resolution := proxypool.ResolveWithContext(ctx, cfg, auth, proxypool.DefaultHealthManager())
	helloID := codexClientHelloID(cfg)

	var standardTransport http.RoundTripper
	if transport := proxypool.BuildHTTPRoundTripperForResolution(resolution); transport != nil {
		standardTransport = transport
	} else {
		standardTransport = proxyutil.NewDirectTransport()
	}

	frt := &fallbackRoundTripper{
		fallback: standardTransport,
		hosts:    codexHosts,
	}
	if cfg != nil && cfg.CodexHeaderDefaults.UTLSHTTP2 {
		utlsRT := newUtlsRoundTripper(strings.TrimSpace(resolution.ProxyURL))
		utlsRT.helloID = helloID
		frt.utls = utlsRT
	} else {
		frt.utls = newCodexUtlsHTTP1Transport(codexEgressDialer(resolution), helloID)
		frt.codexUtlsHTTP1 = true
	}

	client := &http.Client{Transport: frt}
	if timeout > 0 {
		client.Timeout = timeout
	}
	return client, resolution
}

// codexEgressDialer builds a proxy-aware raw dialer for the resolved egress,
// honoring socks5/http proxies and bind:// (ipv6 local-addr) leases — the same
// egress decision the standard transport would use. Empty/inherit -> direct.
func codexEgressDialer(resolution proxypool.Resolution) proxy.Dialer {
	proxyURL := strings.TrimSpace(resolution.ProxyURL)
	if proxyURL == "" {
		return proxy.Direct
	}
	dialer, mode, err := proxyutil.BuildDialer(proxyURL)
	if err != nil || dialer == nil || mode == proxyutil.ModeInherit {
		return proxy.Direct
	}
	return dialer
}

// newCodexUtlsHTTP1Transport returns an *http.Transport that performs the utls
// browser handshake via DialTLSContext and is pinned to HTTP/1.1
// (ForceAttemptHTTP2=false + http/1.1-only ALPN), so it never emits Go-net's
// HTTP/2 SETTINGS frame. Because DialTLSContext returns a *utls.UConn (not a
// crypto/tls.Conn), net/http cannot read a negotiated h2 and speaks HTTP/1.1.
func newCodexUtlsHTTP1Transport(dialer proxy.Dialer, helloID tls.ClientHelloID) *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     false,
		DialTLSContext:        codexUtlsDialTLSContext(dialer, helloID),
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// codexUtlsDialTLSContext dials raw TCP via the proxy-aware dialer, then runs a
// utls handshake presenting the codex browser ClientHello with ALPN pinned to
// http/1.1. Mirrors sub2api's tlsfingerprint dialer.
func codexUtlsDialTLSContext(dialer proxy.Dialer, helloID tls.ClientHelloID) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, errSplit := net.SplitHostPort(addr)
		if errSplit != nil {
			host = addr
		}
		rawConn, err := dialViaContext(ctx, dialer, network, addr)
		if err != nil {
			return nil, err
		}
		spec, errSpec := tls.UTLSIdToSpec(helloID)
		if errSpec != nil {
			_ = rawConn.Close()
			return nil, errSpec
		}
		forceHTTP1ALPN(&spec)
		uconn := tls.UClient(rawConn, &tls.Config{ServerName: host}, tls.HelloCustom)
		if errPreset := uconn.ApplyPreset(&spec); errPreset != nil {
			_ = rawConn.Close()
			return nil, errPreset
		}
		if errHS := uconn.HandshakeContext(ctx); errHS != nil {
			_ = rawConn.Close()
			return nil, errHS
		}
		return uconn, nil
	}
}

// dialViaContext prefers the dialer's context-aware Dial when available so
// per-request deadlines/cancellation are honored on the raw TCP dial.
func dialViaContext(ctx context.Context, dialer proxy.Dialer, network, addr string) (net.Conn, error) {
	if cd, ok := dialer.(proxy.ContextDialer); ok {
		return cd.DialContext(ctx, network, addr)
	}
	return dialer.Dial(network, addr)
}

// forceHTTP1ALPN rewrites the ClientHelloSpec's ALPN extension to advertise
// http/1.1 only, so the server never negotiates h2 on the impersonated socket.
// UTLSIdToSpec is called fresh per dial, so this mutation is connection-local.
func forceHTTP1ALPN(spec *tls.ClientHelloSpec) {
	for _, ext := range spec.Extensions {
		if alpn, ok := ext.(*tls.ALPNExtension); ok {
			alpn.AlpnProtocols = []string{"http/1.1"}
		}
	}
}

// NewCodexHTTPClientWithResolution returns a codex HTTP client honoring the
// cfg.CodexHeaderDefaults.UpstreamUTLS toggle: utls (real-browser TLS) when on,
// the standard proxy-aware client when off. Drop-in for the codex executor's
// previous NewProxyAwareHTTPClientWithResolution call sites.
func NewCodexHTTPClientWithResolution(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) (*http.Client, proxypool.Resolution) {
	if cfg != nil && cfg.CodexHeaderDefaults.UpstreamUTLS {
		return NewCodexUtlsHTTPClient(ctx, cfg, auth, timeout)
	}
	return NewProxyAwareHTTPClientWithResolution(ctx, cfg, auth, timeout)
}
