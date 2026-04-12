package proxypool

import (
	"bytes"
	"io"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// BuildHTTPRoundTripper resolves the effective proxy selection and returns a
// round tripper that optionally falls back to direct mode.
func BuildHTTPRoundTripper(cfg *config.Config, auth *coreauth.Auth) (http.RoundTripper, Resolution) {
	resolution := Resolve(cfg, auth)
	return BuildHTTPRoundTripperForResolution(resolution), resolution
}

// BuildHTTPRoundTripperForResolution builds a round tripper for an already
// resolved proxy decision.
func BuildHTTPRoundTripperForResolution(resolution Resolution) http.RoundTripper {
	proxyURL := resolution.ProxyURL
	if proxyURL == "" {
		if resolution.FallbackToDirect {
			return proxyutil.NewDirectTransport()
		}
		return nil
	}

	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
	if errBuild != nil {
		log.WithError(errBuild).Debug("build proxy transport failed")
		if resolution.FallbackToDirect {
			return proxyutil.NewDirectTransport()
		}
		return nil
	}
	if transport == nil {
		if resolution.FallbackToDirect {
			return proxyutil.NewDirectTransport()
		}
		return nil
	}
	if !resolution.FallbackToDirect {
		return transport
	}
	return &directFallbackRoundTripper{
		primary: transport,
		direct:  proxyutil.NewDirectTransport(),
	}
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
