// Package util provides utility functions for the CLI Proxy API server.
// It includes helper functions for proxy configuration, HTTP client setup,
// log level management, and other common operations used across the application.
package util

import (
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// SetProxy configures the provided HTTP client with proxy settings from the configuration.
// It supports SOCKS5, HTTP, and HTTPS proxies. The function modifies the client's transport
// to route requests through the configured proxy server.
func SetProxy(cfg *config.SDKConfig, httpClient *http.Client) *http.Client {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	if cfg == nil {
		return httpClient
	}
	proxyURL := cfg.ProxyURL

	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
	if errBuild != nil {
		log.Errorf("%v", errBuild)
	}
	if transport != nil {
		httpClient.Transport = transport
		return httpClient
	}
	// Fallback to proxy pool transport for comma-separated proxy lists.
	if poolTransport := NewProxyPoolTransport(proxyURL); poolTransport != nil {
		httpClient.Transport = poolTransport
		return httpClient
	}
	if !HasUsableProxyConfig(proxyURL) {
		httpClient.Transport = NewProxyPoolTransport("direct")
	}
	return httpClient
}
