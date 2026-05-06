package proxypool

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

var httpRoundTripperCache sync.Map

// BuildHTTPRoundTripper resolves the effective proxy selection and returns a
// round tripper that optionally falls back to direct mode.
func BuildHTTPRoundTripper(cfg *config.Config, auth *coreauth.Auth) (http.RoundTripper, Resolution) {
	return BuildHTTPRoundTripperWithContext(context.Background(), cfg, auth)
}

func BuildHTTPRoundTripperWithContext(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (http.RoundTripper, Resolution) {
	resolution := ResolveWithContext(ctx, cfg, auth, DefaultHealthManager())
	return BuildHTTPRoundTripperForResolution(resolution), resolution
}

// BuildHTTPRoundTripperForResolution builds a round tripper for an already
// resolved proxy decision.
func BuildHTTPRoundTripperForResolution(resolution Resolution) http.RoundTripper {
	cacheKey := httpRoundTripperCacheKey(resolution)
	if cacheKey != "" {
		if cached, ok := httpRoundTripperCache.Load(cacheKey); ok {
			if rt, okRT := cached.(http.RoundTripper); okRT && rt != nil {
				return rt
			}
		}
	}

	var built http.RoundTripper
	proxyURL := resolution.ProxyURL
	if proxyURL == "" {
		if resolution.FallbackToDirect {
			built = proxyutil.NewDirectTransport()
			return storeHTTPRoundTripperCache(cacheKey, built)
		}
		return nil
	}

	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
	if errBuild != nil {
		log.WithError(errBuild).Debug("build proxy transport failed")
		if resolution.FallbackToDirect {
			built = proxyutil.NewDirectTransport()
			return storeHTTPRoundTripperCache(cacheKey, built)
		}
		return nil
	}
	if transport == nil {
		if resolution.FallbackToDirect {
			built = proxyutil.NewDirectTransport()
			return storeHTTPRoundTripperCache(cacheKey, built)
		}
		return nil
	}
	if !resolution.FallbackToDirect {
		built = transport
		return storeHTTPRoundTripperCache(cacheKey, built)
	}
	built = &directFallbackRoundTripper{
		primary: transport,
		direct:  proxyutil.NewDirectTransport(),
	}
	return storeHTTPRoundTripperCache(cacheKey, built)
}

type directFallbackRoundTripper struct {
	primary http.RoundTripper
	direct  http.RoundTripper
}

func (d *directFallbackRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := d.primary.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	retryReq, errClone := cloneRequestForRetry(req)
	if errClone != nil {
		return nil, err
	}
	return d.direct.RoundTrip(retryReq)
}

func cloneRequestForRetry(req *http.Request) (*http.Request, error) {
	if req == nil {
		return nil, nil
	}
	cloned := req.Clone(req.Context())
	if req.Body == nil {
		return cloned, nil
	}
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		cloned.Body = body
		return cloned, nil
	}

	data, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(data))
	req.ContentLength = int64(len(data))
	cloned.Body = io.NopCloser(bytes.NewReader(data))
	cloned.ContentLength = int64(len(data))
	return cloned, nil
}

func httpRoundTripperCacheKey(resolution Resolution) string {
	if resolution.ProxyURL == "" && !resolution.FallbackToDirect {
		return ""
	}
	return resolution.Source + "|" + resolution.ProxyPool + "|" + resolution.ProxyName + "|" + resolution.ProxyURL + "|" + boolKey(resolution.FallbackToDirect)
}

func storeHTTPRoundTripperCache(cacheKey string, built http.RoundTripper) http.RoundTripper {
	if cacheKey == "" || built == nil {
		return built
	}
	actual, _ := httpRoundTripperCache.LoadOrStore(cacheKey, built)
	if rt, ok := actual.(http.RoundTripper); ok && rt != nil {
		return rt
	}
	return built
}

func boolKey(v bool) string {
	if v {
		return "1"
	}
	return "0"
}
