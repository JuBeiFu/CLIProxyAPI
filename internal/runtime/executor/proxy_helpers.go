package executor

import (
	"context"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/proxyrouting"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/proxystats"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// newProxyAwareHTTPClient creates an HTTP client with auth-aware proxy routing.
// It prefers explicit auth proxy settings and routing rules, then falls back to
// the RoundTripper injected into the execution context.
func newProxyAwareHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	httpClient := &http.Client{}
	if timeout > 0 {
		httpClient.Timeout = timeout
	}

	selection := proxyrouting.Resolve(cfg, auth)
	if selection.HasProxy() {
		transport := util.NewProxyPoolTransport(selection.ProxyURL)
		if transport != nil {
			httpClient.Transport = proxystats.AttachRoundTripperMetadata(transport, selection.StatsMetadata())
			return httpClient
		}
		log.Debug("failed to setup proxy transport from configured proxy pool, falling back to context transport")
	}

	if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
		httpClient.Transport = rt
	}

	return httpClient
}
