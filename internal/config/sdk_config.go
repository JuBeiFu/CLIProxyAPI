// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

// SDKConfig represents the application's configuration, loaded from a YAML file.
type SDKConfig struct {
	// ProxyURL is the URL of an optional proxy server to use for outbound requests.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// DefaultProxyPool selects the proxy pool used by credentials that do not
	// specify a per-auth or per-key proxy-url/proxy-pool override.
	DefaultProxyPool string `yaml:"default-proxy-pool,omitempty" json:"default-proxy-pool,omitempty"`

	// ProxyPools defines named proxy pools that can be assigned to credentials.
	ProxyPools []ProxyPool `yaml:"proxy-pools,omitempty" json:"proxy-pools,omitempty"`

	// EnableGeminiCLIEndpoint controls whether Gemini CLI internal endpoints (/v1internal:*) are enabled.
	// Default is false for safety; when false, /v1internal:* requests are rejected.
	EnableGeminiCLIEndpoint bool `yaml:"enable-gemini-cli-endpoint" json:"enable-gemini-cli-endpoint"`

	// ForceModelPrefix requires explicit model prefixes (e.g., "teamA/gemini-3-pro-preview")
	// to target prefixed credentials. When false, unprefixed model requests may use prefixed
	// credentials as well.
	ForceModelPrefix bool `yaml:"force-model-prefix" json:"force-model-prefix"`

	// RequestLog enables or disables detailed request logging functionality.
	RequestLog bool `yaml:"request-log" json:"request-log"`

	// APIKeys is a list of keys for authenticating clients to this proxy server.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`

	// PassthroughHeaders controls whether upstream response headers are forwarded to downstream clients.
	// Default is false (disabled).
	PassthroughHeaders bool `yaml:"passthrough-headers" json:"passthrough-headers"`

	// Streaming configures server-side streaming behavior (keep-alives and safe bootstrap retries).
	Streaming StreamingConfig `yaml:"streaming" json:"streaming"`

	// NonStreamKeepAliveInterval controls how often blank lines are emitted for non-streaming responses.
	// <= 0 disables keep-alives. Value is in seconds.
	NonStreamKeepAliveInterval int `yaml:"nonstream-keepalive-interval,omitempty" json:"nonstream-keepalive-interval,omitempty"`
}

// StreamingConfig holds server streaming behavior configuration.
type StreamingConfig struct {
	// KeepAliveSeconds controls how often the server emits SSE heartbeats (": keep-alive\n\n").
	// <= 0 disables keep-alives. Default is 0.
	KeepAliveSeconds int `yaml:"keepalive-seconds,omitempty" json:"keepalive-seconds,omitempty"`

	// BootstrapRetries controls how many times the server may retry a streaming request before any bytes are sent,
	// to allow auth rotation / transient recovery.
	// <= 0 disables bootstrap retries. Default is 0.
	BootstrapRetries int `yaml:"bootstrap-retries,omitempty" json:"bootstrap-retries,omitempty"`
}

// ProxyPool defines a named collection of outbound proxies.
type ProxyPool struct {
	Name string `yaml:"name" json:"name"`

	// FallbackToDirect allows runtime transport helpers to retry the request
	// without a proxy when the selected pool proxy fails to connect.
	FallbackToDirect bool `yaml:"fallback-to-direct,omitempty" json:"fallback-to-direct,omitempty"`

	// HealthCheckURL is the URL used by management APIs to probe each proxy.
	HealthCheckURL string `yaml:"health-check-url,omitempty" json:"health-check-url,omitempty"`

	// HealthCheckMethod is the HTTP method used for management proxy checks.
	HealthCheckMethod string `yaml:"health-check-method,omitempty" json:"health-check-method,omitempty"`

	// HealthCheckIntervalSeconds is advisory metadata for UI/automation.
	HealthCheckIntervalSeconds int `yaml:"health-check-interval-seconds,omitempty" json:"health-check-interval-seconds,omitempty"`

	// HealthCheckTimeoutSeconds controls management-side probe timeout.
	HealthCheckTimeoutSeconds int `yaml:"health-check-timeout-seconds,omitempty" json:"health-check-timeout-seconds,omitempty"`

	// ExpectedStatusCodes marks which upstream statuses count as healthy.
	ExpectedStatusCodes []int `yaml:"health-check-expected-status-codes,omitempty" json:"health-check-expected-status-codes,omitempty"`

	Entries []ProxyPoolEntry `yaml:"entries" json:"entries"`
}

// ProxyPoolEntry is a single outbound proxy candidate within a pool.
type ProxyPoolEntry struct {
	Name     string `yaml:"name,omitempty" json:"name,omitempty"`
	URL      string `yaml:"url" json:"url"`
	Weight   int    `yaml:"weight,omitempty" json:"weight,omitempty"`
	Disabled bool   `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}
