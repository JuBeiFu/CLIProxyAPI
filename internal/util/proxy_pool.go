package util

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/proxystats"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

var (
	proxyPoolCounter    atomic.Uint64
	proxyTransportCache sync.Map
)

type proxyTransportEntry struct {
	proxyURL  string
	transport http.RoundTripper
}

type proxyPoolTransport struct {
	entries []proxyTransportEntry
}

type proxyDialerEntry struct {
	proxyURL string
	dialer   proxy.Dialer
}

type proxyPoolDialer struct {
	entries []proxyDialerEntry
}

// SplitProxyURLs parses a proxy list from a comma-separated string.
func SplitProxyURLs(raw string) []string {
	parts := strings.Split(raw, ",")
	if len(parts) == 0 {
		return nil
	}
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for i := range parts {
		proxyURL := strings.TrimSpace(parts[i])
		if proxyURL == "" {
			continue
		}
		if _, ok := seen[proxyURL]; ok {
			continue
		}
		seen[proxyURL] = struct{}{}
		out = append(out, proxyURL)
	}
	return out
}

// OrderedProxyURLs returns proxies in a round-robin starting order so new
// requests spread across the pool while preserving deterministic fallback.
func OrderedProxyURLs(raw string) []string {
	proxies := SplitProxyURLs(raw)
	if len(proxies) <= 1 {
		return proxies
	}
	start := int(proxyPoolCounter.Add(1)-1) % len(proxies)
	ordered := make([]string, 0, len(proxies))
	ordered = append(ordered, proxies[start:]...)
	ordered = append(ordered, proxies[:start]...)
	return ordered
}

// NewProxyPoolTransport creates a RoundTripper that load-balances across the
// configured proxy list and falls back to the next proxy when the current one
// fails at the transport layer.
func NewProxyPoolTransport(raw string) http.RoundTripper {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if cached, ok := proxyTransportCache.Load(raw); ok {
		if rt, okCast := cached.(http.RoundTripper); okCast && rt != nil {
			return rt
		}
	}
	proxies := SplitProxyURLs(raw)
	entries := make([]proxyTransportEntry, 0, len(proxies))
	for i := range proxies {
		transport := BuildProxyTransport(proxies[i])
		if transport == nil {
			continue
		}
		entries = append(entries, proxyTransportEntry{proxyURL: proxies[i], transport: transport})
	}
	if len(entries) == 0 {
		return nil
	}
	rt := http.RoundTripper(&proxyPoolTransport{entries: entries})
	if actual, loaded := proxyTransportCache.LoadOrStore(raw, rt); loaded {
		if cached, okCast := actual.(http.RoundTripper); okCast && cached != nil {
			return cached
		}
	}
	return rt
}

// NewProxyDialer creates a proxy.Dialer that load-balances SOCKS proxy usage
// and falls back to the next proxy when dialing fails.
func NewProxyDialer(raw string, forward proxy.Dialer) proxy.Dialer {
	if forward == nil {
		forward = proxy.Direct
	}
	proxies := SplitProxyURLs(raw)
	entries := make([]proxyDialerEntry, 0, len(proxies))
	for i := range proxies {
		dialer, ok := buildProxyDialer(proxies[i], forward)
		if !ok {
			continue
		}
		entries = append(entries, proxyDialerEntry{proxyURL: proxies[i], dialer: dialer})
	}
	if len(entries) == 0 {
		return forward
	}
	if len(entries) == 1 {
		return entries[0].dialer
	}
	return &proxyPoolDialer{entries: entries}
}

// BuildProxyTransport creates an HTTP transport configured for the given proxy
// URL. It supports SOCKS5, HTTP, and HTTPS proxies.
func BuildProxyTransport(proxyURL string) *http.Transport {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil
	}

	parsedURL, errParse := url.Parse(proxyURL)
	if errParse != nil {
		log.WithError(errParse).Debug("parse proxy URL failed")
		return nil
	}
	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		log.Debug("proxy URL missing scheme/host")
		return nil
	}

	transport := cloneDefaultTransport()

	switch parsedURL.Scheme {
	case "socks5", "socks5h":
		var proxyAuth *proxy.Auth
		if parsedURL.User != nil {
			username := parsedURL.User.Username()
			password, _ := parsedURL.User.Password()
			proxyAuth = &proxy.Auth{User: username, Password: password}
		}
		dialer, errSOCKS5 := proxy.SOCKS5("tcp", parsedURL.Host, proxyAuth, proxy.Direct)
		if errSOCKS5 != nil {
			log.WithError(errSOCKS5).Debug("create SOCKS5 dialer failed")
			return nil
		}
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}
	case "http", "https":
		transport.Proxy = http.ProxyURL(parsedURL)
	default:
		log.Debugf("unsupported proxy scheme: %s", parsedURL.Scheme)
		return nil
	}

	return transport
}

func (p *proxyPoolTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("proxy pool transport: nil request")
	}
	var lastErr error
	start := 0
	if len(p.entries) > 1 {
		start = int(proxyPoolCounter.Add(1)-1) % len(p.entries)
	}
	for attempt := 0; attempt < len(p.entries); attempt++ {
		idx := (start + attempt) % len(p.entries)
		retryReq, errClone := cloneRequestForRetry(req)
		if errClone != nil {
			return nil, errClone
		}
		attemptStarted := time.Now()
		resp, err := p.entries[idx].transport.RoundTrip(retryReq)
		if err == nil {
			return proxystats.WrapResponse(req.Context(), p.entries[idx].proxyURL, attemptStarted, resp), nil
		}
		proxystats.RecordTransportFailure(req.Context(), p.entries[idx].proxyURL, attemptStarted, err)
		lastErr = err
		if attempt+1 >= len(p.entries) || !isRetryableProxyTransportError(err) {
			return nil, err
		}
		log.WithError(err).Debug("proxy transport failed, retrying next proxy")
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("proxy pool transport: no usable proxy transport")
}

func (p *proxyPoolDialer) Dial(network, addr string) (net.Conn, error) {
	var lastErr error
	start := 0
	if len(p.entries) > 1 {
		start = int(proxyPoolCounter.Add(1)-1) % len(p.entries)
	}
	for attempt := 0; attempt < len(p.entries); attempt++ {
		idx := (start + attempt) % len(p.entries)
		conn, err := p.entries[idx].dialer.Dial(network, addr)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if attempt+1 >= len(p.entries) {
			break
		}
		log.WithError(err).Debug("proxy dialer failed, retrying next proxy")
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("proxy pool dialer: no usable proxy dialer")
}

func buildProxyDialer(proxyURL string, forward proxy.Dialer) (proxy.Dialer, bool) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil, false
	}
	parsedURL, errParse := url.Parse(proxyURL)
	if errParse != nil {
		log.WithError(errParse).Debug("parse proxy URL failed")
		return nil, false
	}
	dialer, errDialer := proxy.FromURL(parsedURL, forward)
	if errDialer != nil {
		log.WithError(errDialer).Debug("create proxy dialer failed")
		return nil, false
	}
	return dialer, true
}

func cloneDefaultTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok || base == nil {
		return &http.Transport{}
	}
	return base.Clone()
}

func cloneRequestForRetry(req *http.Request) (*http.Request, error) {
	cloned := req.Clone(req.Context())
	if req.Body == nil || req.Body == http.NoBody {
		return cloned, nil
	}
	if req.GetBody != nil {
		body, errGet := req.GetBody()
		if errGet != nil {
			return nil, errGet
		}
		cloned.Body = body
		return cloned, nil
	}
	bodyBytes, errRead := io.ReadAll(req.Body)
	if errRead != nil {
		return nil, errRead
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(bodyBytes)), nil
	}
	cloned.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	return cloned, nil
}

func isRetryableProxyTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr != nil && urlErr.Err != nil {
		return isRetryableProxyTransportError(urlErr.Err)
	}
	return true
}
