package codex

import (
	"strings"
)

const (
	ProviderName        = "codex"
	SessionProviderName = "codex_session"
	probedPlanTypeKey   = "cliproxy_codex_probed_plan_type"
)

func NormalizeProviderName(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case ProviderName, SessionProviderName:
		return ProviderName
	default:
		return strings.ToLower(strings.TrimSpace(provider))
	}
}

func IsProviderName(provider string) bool {
	return NormalizeProviderName(provider) == ProviderName
}

func ResolveMetadataPlanType(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	if probed, ok := metadata[probedPlanTypeKey].(string); ok {
		if trimmed := strings.ToLower(strings.TrimSpace(probed)); trimmed != "" {
			return trimmed
		}
	}
	if raw, ok := metadata["plan_type"].(string); ok {
		if trimmed := strings.ToLower(strings.TrimSpace(raw)); trimmed != "" {
			return trimmed
		}
	}
	if idTokenRaw, ok := metadata["id_token"].(string); ok {
		if claims, err := ParseJWTToken(idTokenRaw); err == nil && claims != nil {
			if trimmed := strings.ToLower(strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType)); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func ResolveMetadataAccountID(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range []string{"account_id", "chatgpt_account_id", "accountId", "chatgptAccountId"} {
		if raw, ok := metadata[key].(string); ok {
			if trimmed := strings.TrimSpace(raw); trimmed != "" {
				return trimmed
			}
		}
	}
	if idTokenRaw, ok := metadata["id_token"].(string); ok {
		if claims, err := ParseJWTToken(idTokenRaw); err == nil && claims != nil {
			if trimmed := strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func ApplyMetadataAliases(metadata map[string]any) {
	if len(metadata) == 0 {
		return
	}
	if accountID := ResolveMetadataAccountID(metadata); accountID != "" {
		metadata["account_id"] = accountID
		metadata["chatgpt_account_id"] = accountID
	}
	if planType := ResolveMetadataPlanType(metadata); planType != "" {
		metadata["plan_type"] = planType
	}
}
