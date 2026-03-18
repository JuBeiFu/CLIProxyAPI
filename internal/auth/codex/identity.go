package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// CredentialAccount returns the preferred stable account identifier for a Codex credential.
// It prefers the explicit account field and falls back to email.
func CredentialAccount(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range []string{"account", "email"} {
		raw, ok := metadata[key]
		if !ok {
			continue
		}
		if value, ok := raw.(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// CredentialAccountID returns the account id when present in credential metadata.
func CredentialAccountID(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range []string{"account_id", "chatgpt_account_id"} {
		raw, ok := metadata[key]
		if !ok {
			continue
		}
		if value, ok := raw.(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// CredentialHashAccountID returns the short stable hash used in team-plan filenames.
func CredentialHashAccountID(metadata map[string]any) string {
	accountID := CredentialAccountID(metadata)
	if accountID == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(accountID))
	return hex.EncodeToString(digest[:])[:8]
}

// CredentialFileNameFromMetadata derives the canonical Codex credential filename from metadata.
func CredentialFileNameFromMetadata(metadata map[string]any, includeProviderPrefix bool) string {
	account := CredentialAccount(metadata)
	if account == "" {
		return ""
	}
	planType := ""
	if raw, ok := metadata["plan_type"].(string); ok {
		planType = strings.TrimSpace(raw)
	}
	hashAccountID := CredentialHashAccountID(metadata)
	if normalizePlanTypeForFilename(planType) == "team" && hashAccountID == "" {
		parts := make([]string, 0, 3)
		if includeProviderPrefix {
			parts = append(parts, "codex")
		}
		parts = append(parts, account, "team")
		return strings.Join(parts, "-") + ".json"
	}
	return CredentialFileName(account, planType, hashAccountID, includeProviderPrefix)
}

// CredentialID returns the stable manager ID used for Codex credentials when account info exists.
func CredentialID(metadata map[string]any) string {
	account := strings.ToLower(strings.TrimSpace(CredentialAccount(metadata)))
	if account == "" {
		return ""
	}
	return "codex:" + account
}
