package auth

import "strings"

const metadataProxyProfileKey = "proxy_profile"

// PlanType returns the normalized subscription/plan type attached to an auth.
// It checks immutable attributes first, then mutable metadata fields.
func (a *Auth) PlanType() string {
	if a == nil {
		return ""
	}
	if len(a.Attributes) > 0 {
		if v := strings.TrimSpace(a.Attributes[metadataPlanTypeKey]); v != "" {
			return v
		}
	}
	if len(a.Metadata) == 0 {
		return ""
	}
	return firstMetadataString(a.Metadata, metadataPlanTypeKey, "chatgpt_plan_type", "planType")
}

// ProxyProfile returns the named proxy profile pinned to an auth, when configured.
func (a *Auth) ProxyProfile() string {
	if a == nil {
		return ""
	}
	if len(a.Attributes) > 0 {
		if v := strings.TrimSpace(a.Attributes[metadataProxyProfileKey]); v != "" {
			return v
		}
	}
	if len(a.Metadata) == 0 {
		return ""
	}
	return firstMetadataString(a.Metadata, metadataProxyProfileKey, "proxy-profile", "proxyProfile")
}

func firstMetadataString(meta map[string]any, keys ...string) string {
	if len(meta) == 0 {
		return ""
	}
	for _, key := range keys {
		raw, ok := meta[key]
		if !ok {
			continue
		}
		switch value := raw.(type) {
		case string:
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		case []byte:
			if trimmed := strings.TrimSpace(string(value)); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}
