package auth

import (
	"strings"

	internalcodex "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
)

const (
	metadataPlanTypeKey  = "plan_type"
	metadataCodexIDToken = "id_token"
)

func codexPlanType(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if len(auth.Attributes) > 0 {
		if v := strings.TrimSpace(auth.Attributes[metadataPlanTypeKey]); v != "" {
			return v
		}
	}
	if len(auth.Metadata) == 0 {
		return ""
	}
	if raw, ok := auth.Metadata[metadataPlanTypeKey]; ok {
		if v, ok := raw.(string); ok {
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		}
	}
	// Compatibility: allow operators to inject chatgpt_plan_type at the top-level auth JSON.
	if raw, ok := auth.Metadata["chatgpt_plan_type"]; ok {
		if v, ok := raw.(string); ok {
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// hydrateCodexPlanType populates auth.Metadata["plan_type"] from the JWT id_token when missing.
// It must only be called in contexts where it is safe to mutate auth.Metadata (e.g. during manager
// load/register/update while holding the manager lock).
func hydrateCodexPlanType(auth *Auth) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return
	}
	// Operators may pin plan_type via immutable attributes; do not override.
	if len(auth.Attributes) > 0 {
		if v := strings.TrimSpace(auth.Attributes[metadataPlanTypeKey]); v != "" {
			return
		}
	}

	currentPlan := ""
	if raw, ok := auth.Metadata[metadataPlanTypeKey]; ok {
		if v, ok := raw.(string); ok {
			currentPlan = strings.TrimSpace(v)
		}
	}
	if currentPlan != "" && strings.EqualFold(currentPlan, "unknown") {
		currentPlan = ""
	}

	// If we already have a plan type but no ID token, there's nothing to reconcile.
	_, hasToken := auth.Metadata[metadataCodexIDToken].(string)
	if currentPlan != "" && !hasToken {
		return
	}

	rawToken, ok := auth.Metadata[metadataCodexIDToken].(string)
	if !ok {
		return
	}
	idToken := strings.TrimSpace(rawToken)
	if idToken == "" {
		return
	}
	claims, err := internalcodex.ParseJWTToken(idToken)
	if err != nil || claims == nil {
		return
	}
	planType := strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType)
	if planType == "" {
		return
	}
	if currentPlan == "" || !strings.EqualFold(currentPlan, planType) {
		auth.Metadata[metadataPlanTypeKey] = planType
	}
}

func codexPlanIsPaid(planType string) bool {
	pt := strings.ToLower(strings.TrimSpace(planType))
	switch pt {
	case "", "free", "none", "unknown":
		return false
	default:
		return true
	}
}
