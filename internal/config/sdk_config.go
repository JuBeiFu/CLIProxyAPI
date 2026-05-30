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

	// CodexResponsesStreamHTTP1 forces Codex /responses streaming upstream requests to HTTP/1.1.
	// It is intended as a gray switch for isolating HTTP/2 stream resets.
	CodexResponsesStreamHTTP1 bool `yaml:"codex-responses-stream-http1" json:"codex-responses-stream-http1"`

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

	// IPv6BindRanges expands inclusive IPv6 address ranges into direct bind://
	// proxy entries during normalization. This allows a pool to materialize
	// host-assigned IPv6 egress addresses without defining each entry manually.
	IPv6BindRanges []IPv6BindRange `yaml:"ipv6-bind-ranges,omitempty" json:"ipv6-bind-ranges,omitempty"`

	// IPv6BindLeaseRanges defines large IPv6 pools that are allocated lazily
	// per auth and persisted on the auth record. Unlike IPv6BindRanges, these
	// ranges are not expanded into ProxyPoolEntry values, so a /64 remains
	// cheap to configure.
	IPv6BindLeaseRanges []IPv6BindLeaseRange `yaml:"ipv6-bind-lease-ranges,omitempty" json:"ipv6-bind-lease-ranges,omitempty"`

	// CodexPreferIPv6Bind, when true, makes a codex auth's own persisted IPv6
	// bind lease its PRIMARY egress (not just a failover target), so every
	// account egresses from a unique /128 instead of sharing the host's
	// direct-v4 IP. Avoids collective IP-correlation risk across accounts.
	// Requires IPv6BindLeaseRanges. Default false (legacy direct-v4 primary +
	// v6 failover).
	CodexPreferIPv6Bind bool `yaml:"codex-prefer-ipv6-bind,omitempty" json:"codex-prefer-ipv6-bind,omitempty"`

	Entries []ProxyPoolEntry `yaml:"entries" json:"entries"`
}

// ProxyPoolEntry is a single outbound proxy candidate within a pool.
type ProxyPoolEntry struct {
	Name     string `yaml:"name,omitempty" json:"name,omitempty"`
	URL      string `yaml:"url" json:"url"`
	Weight   int    `yaml:"weight,omitempty" json:"weight,omitempty"`
	Disabled bool   `yaml:"disabled,omitempty" json:"disabled,omitempty"`

	// runtimeGenerated marks entries synthesized from configuration ranges.
	// They are used at runtime but should not be persisted back into config.yaml.
	runtimeGenerated bool `yaml:"-" json:"-"`
}

// IPv6BindRange describes an inclusive IPv6 address span that should be
// expanded into bind://[ipv6] proxy entries.
type IPv6BindRange struct {
	NamePrefix string `yaml:"name-prefix,omitempty" json:"name-prefix,omitempty"`
	Start      string `yaml:"start,omitempty" json:"start,omitempty"`
	End        string `yaml:"end,omitempty" json:"end,omitempty"`
	Weight     int    `yaml:"weight,omitempty" json:"weight,omitempty"`
	Disabled   bool   `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// IPv6BindLeaseRange describes a CIDR block used for per-auth bind:// IPv6
// leases. It is intentionally CIDR-only so huge host ranges never expand into
// runtime proxy entries.
type IPv6BindLeaseRange struct {
	NamePrefix string `yaml:"name-prefix,omitempty" json:"name-prefix,omitempty"`
	CIDR       string `yaml:"cidr,omitempty" json:"cidr,omitempty"`
	Disabled   bool   `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}
