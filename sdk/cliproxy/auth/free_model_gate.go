package auth

import "strings"

// FreePlanModelGate, when non-nil, reports whether a given route model is
// ALLOWED for a free-plan codex auth. It is registered by the sdk/cliproxy
// layer (which owns config) at startup, mirroring BoundProxyHealthChecker, so
// the auth package stays decoupled from config and picks up hot-reloads via the
// closure reading live cfg. Returning false means the model is NOT allowed for
// a free auth, so selection must skip that candidate.
var FreePlanModelGate func(routeModel string) bool

// freePlanModelBlocked returns true iff the gate is active, the auth is an
// EXPLICITLY-free codex auth, the route model is non-empty, and the gate
// disallows that model for free plans. Unprobed (empty/unknown plan) and paid
// auths are never blocked.
func freePlanModelBlocked(auth *Auth, routeModel string) bool {
	if FreePlanModelGate == nil || auth == nil {
		return false
	}
	if strings.TrimSpace(routeModel) == "" {
		return false
	}
	if !isCodexAuth(auth) {
		return false
	}
	if normalizedPlanTypeKey(codexPlanTypeForAuth(auth)) != "free" {
		return false
	}
	return !FreePlanModelGate(routeModel)
}
