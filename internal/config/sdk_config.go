// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

// SDKConfig represents the application's configuration, loaded from a YAML file.
type SDKConfig struct {
	// ProxyURL is the URL of an optional proxy server to use for outbound requests.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// ProxyProfiles defines named outbound proxy pools that routing rules can reference.
	ProxyProfiles []ProxyProfile `yaml:"proxy-profiles,omitempty" json:"proxy-profiles,omitempty"`

	// ProxyRouting defines auth-aware outbound proxy selection rules.
	ProxyRouting ProxyRoutingConfig `yaml:"proxy-routing,omitempty" json:"proxy-routing,omitempty"`

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

	// ModelProviderMappings supplies fallback routing rules when a model is not present
	// in the dynamic model registry. Rules are evaluated in order; the first match wins.
	//
	// This is useful when upstream introduces new model IDs (for example, "gpt-*") before
	// the local static model list is updated.
	ModelProviderMappings []ModelProviderMapping `yaml:"model-provider-mappings,omitempty" json:"model-provider-mappings,omitempty"`
}

// ProxyProfile defines a reusable outbound proxy target or pool.
type ProxyProfile struct {
	Name        string `yaml:"name" json:"name"`
	ProxyURL    string `yaml:"proxy-url" json:"proxy-url"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

// ProxyRoutingConfig defines per-auth proxy selection rules.
type ProxyRoutingConfig struct {
	Rules []ProxyRoutingRule `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// ProxyRoutingRule matches auth metadata and routes requests to a proxy profile or URL.
type ProxyRoutingRule struct {
	Name         string   `yaml:"name,omitempty" json:"name,omitempty"`
	Providers    []string `yaml:"providers,omitempty" json:"providers,omitempty"`
	PlanTypes    []string `yaml:"plan-types,omitempty" json:"plan-types,omitempty"`
	AuthKinds    []string `yaml:"auth-kinds,omitempty" json:"auth-kinds,omitempty"`
	ProxyProfile string   `yaml:"proxy-profile,omitempty" json:"proxy-profile,omitempty"`
	ProxyURL     string   `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`
	Disabled     bool     `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// ModelProviderMapping defines a fallback mapping from model name patterns to provider identifiers.
// Providers must match executor identifiers (for example: "codex", "claude", "gemini").
type ModelProviderMapping struct {
	// Pattern matches model IDs. When Regex=false, supports simple glob wildcards ("*" and "?")
	// and falls back to case-insensitive exact matching.
	Pattern string `yaml:"pattern" json:"pattern"`
	// Providers is the ordered list of providers to try for the matching model.
	Providers []string `yaml:"providers" json:"providers"`
	// Regex toggles interpreting Pattern as a regular expression.
	Regex bool `yaml:"regex,omitempty" json:"regex,omitempty"`
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

	// BootstrapTimeoutSeconds caps how long a stream may wait for the initial upstream payload.
	// <= 0 falls back to the built-in default (30 seconds).
	BootstrapTimeoutSeconds int `yaml:"bootstrap-timeout-seconds,omitempty" json:"bootstrap-timeout-seconds,omitempty"`
}
