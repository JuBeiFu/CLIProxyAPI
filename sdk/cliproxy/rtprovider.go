package cliproxy

import (
	"net/http"
	"strings"
	"sync"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/proxyrouting"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/proxystats"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// defaultRoundTripperProvider returns a per-auth HTTP RoundTripper based on
// explicit auth proxy settings and the current proxy routing config.
type defaultRoundTripperProvider struct {
	cfgGetter func() *internalconfig.Config
	mu        sync.RWMutex
	cache     map[string]http.RoundTripper
}

func newDefaultRoundTripperProvider(cfgGetter func() *internalconfig.Config) *defaultRoundTripperProvider {
	return &defaultRoundTripperProvider{cfgGetter: cfgGetter, cache: make(map[string]http.RoundTripper)}
}

// RoundTripperFor implements coreauth.RoundTripperProvider.
func (p *defaultRoundTripperProvider) RoundTripperFor(auth *coreauth.Auth) http.RoundTripper {
	if auth == nil {
		return nil
	}
	selection := proxyrouting.Resolve(p.currentConfig(), auth)
	proxyStr := strings.TrimSpace(selection.ProxyURL)
	if proxyStr == "" {
		return nil
	}

	p.mu.RLock()
	base := p.cache[proxyStr]
	p.mu.RUnlock()
	if base == nil {
		transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyStr)
		if errBuild != nil {
			log.Errorf("%v", errBuild)
		}
		if transport != nil {
			base = transport
		} else {
			// Fallback to proxy pool transport for comma-separated proxy lists.
			base = util.NewProxyPoolTransport(proxyStr)
		}
		if base == nil {
			return nil
		}
		p.mu.Lock()
		if cached := p.cache[proxyStr]; cached != nil {
			base = cached
		} else {
			p.cache[proxyStr] = base
		}
		p.mu.Unlock()
	}

	return proxystats.AttachRoundTripperMetadata(base, selection.StatsMetadata())
}

func (p *defaultRoundTripperProvider) currentConfig() *internalconfig.Config {
	if p == nil || p.cfgGetter == nil {
		return nil
	}
	return p.cfgGetter()
}
