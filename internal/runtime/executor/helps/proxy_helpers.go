package helps

import (
	"context"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/proxyrouting"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/proxystats"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// NewProxyAwareHTTPClient creates an HTTP client with auth-aware proxy routing.
// It prefers explicit auth proxy settings and routing rules, then falls back to
// the RoundTripper injected into the execution context.
//
// Parameters:
//   - ctx: The context containing optional RoundTripper
//   - cfg: The application configuration
//   - auth: The authentication information
//   - timeout: The client timeout (0 means no timeout)
//
// Returns:
//   - *http.Client: An HTTP client with configured proxy or transport
func NewProxyAwareHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
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
		return httpClient
	}

	// When no explicit proxy is selected, prefer direct connections over
	// inheriting process-wide HTTP(S)_PROXY settings such as warp-lb.
	httpClient.Transport = util.NewProxyPoolTransport("direct")

	return httpClient
}

// buildProxyTransport creates an HTTP transport configured for the given proxy URL.
// It supports SOCKS5, HTTP, and HTTPS proxy protocols.
//
// Parameters:
//   - proxyURL: The proxy URL string (e.g., "socks5://user:pass@host:port", "http://host:port")
//
// Returns:
//   - *http.Transport: A configured transport, or nil if the proxy URL is invalid
func buildProxyTransport(proxyURL string) *http.Transport {
	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
	if errBuild != nil {
		log.Errorf("%v", errBuild)
		return nil
	}
	return transport
}
