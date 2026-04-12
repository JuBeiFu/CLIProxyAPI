package cliproxy

import (
	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/proxypool"
	"net/http"
	"sync"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// defaultRoundTripperProvider returns a per-auth HTTP RoundTripper based on
// the Auth.ProxyURL value. It caches transports per proxy URL string.
type defaultRoundTripperProvider struct {
	mu    sync.RWMutex
	cache map[string]http.RoundTripper
	cfg   *internalconfig.Config
}

func newDefaultRoundTripperProvider(cfg *internalconfig.Config) *defaultRoundTripperProvider {
	return &defaultRoundTripperProvider{
		cache: make(map[string]http.RoundTripper),
		cfg:   cfg,
	}
}

// RoundTripperFor implements coreauth.RoundTripperProvider.
func (p *defaultRoundTripperProvider) RoundTripperFor(auth *coreauth.Auth) http.RoundTripper {
	if auth == nil {
		return nil
	}
	resolution := proxypool.Resolve(p.cfg, auth)
	cacheKey := resolution.ProxyURL
	if resolution.FallbackToDirect {
		cacheKey += "|fallback=direct"
	}
	if cacheKey == "" {
		return nil
	}
	p.mu.RLock()
	rt := p.cache[cacheKey]
	p.mu.RUnlock()
	if rt != nil {
		return rt
	}
	transport := proxypool.BuildHTTPRoundTripperForResolution(resolution)
	if transport == nil {
		return nil
	}
	p.mu.Lock()
	p.cache[cacheKey] = transport
	p.mu.Unlock()
	return transport
}
