package auth

import (
	"fmt"
	"strings"
	"time"
)

// downgradeDetectedPrefix is the stable StatusMessage prefix we use when
// disabling an auth because submitted==paid but upstream reports free. The
// re-enable path tests this exact prefix to avoid clobbering other unrelated
// disable reasons (e.g. "revoked: refresh_token_reused"). Rename requires
// updating every producer AND consumer — guarded by a test.
const downgradeDetectedPrefix = "codex_downgrade_detected: "

const (
	// MetadataSubmittedPlanTypeKey pins the plan_type observed at first
	// successful refresh, used to distinguish "this account was submitted as
	// a paid plan" from the live probed state. Never overwritten once set.
	MetadataSubmittedPlanTypeKey = "cliproxy_submitted_plan_type"

	// MetadataProbedPlanTypeKey stores the most recent plan_type returned by
	// the /wham/usage probe. Overwritten on each probe. Exported so auth-file
	// hydrate paths (filestore, watcher synthesizer, management handler) can
	// prefer this value over the stale JWT chatgpt_plan_type claim when
	// rebuilding Auth.Attributes["plan_type"] from persisted metadata.
	MetadataProbedPlanTypeKey = "cliproxy_codex_probed_plan_type"

	// metadataDowngradeDetectedAtKey records when we first disabled an auth
	// because submitted==paid but probed==free. Cleared on re-enable. Used
	// to enforce the 2h grace window before permanent deletion.
	metadataDowngradeDetectedAtKey = "cliproxy_codex_downgrade_detected_at"

	// Unexported aliases to preserve existing internal references.
	metadataSubmittedPlanTypeKey = MetadataSubmittedPlanTypeKey
	metadataProbedPlanTypeKey    = MetadataProbedPlanTypeKey
)

func isPaidPlan(planType string) bool {
	switch strings.ToLower(strings.TrimSpace(planType)) {
	case "plus", "pro", "team", "enterprise":
		return true
	}
	return false
}

func isFreePlan(planType string) bool {
	switch strings.ToLower(strings.TrimSpace(planType)) {
	case "", "free", "none", "unknown":
		return true
	}
	return false
}

func submittedPlanType(auth *Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	v, _ := auth.Metadata[metadataSubmittedPlanTypeKey].(string)
	return v
}

// setSubmittedPlanType writes the submitted plan_type pin. Pin semantics:
// once a non-empty value is written it is never overwritten. Empty values
// are ignored.
func setSubmittedPlanType(auth *Auth, value string) {
	if auth == nil {
		return
	}
	v := strings.TrimSpace(value)
	if v == "" {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	if existing, _ := auth.Metadata[metadataSubmittedPlanTypeKey].(string); existing != "" {
		return
	}
	auth.Metadata[metadataSubmittedPlanTypeKey] = v
}

func probedPlanType(auth *Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	v, _ := auth.Metadata[metadataProbedPlanTypeKey].(string)
	return v
}

// setProbedPlanType overwrites the live probed plan_type.
func setProbedPlanType(auth *Auth, value string) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[metadataProbedPlanTypeKey] = strings.TrimSpace(value)
}

func downgradeDetectedAt(auth *Auth) (time.Time, bool) {
	if auth == nil || auth.Metadata == nil {
		return time.Time{}, false
	}
	raw, _ := auth.Metadata[metadataDowngradeDetectedAtKey].(string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		// Fall back to RFC3339 (no fractional seconds) for tolerance.
		ts, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, false
		}
	}
	return ts.UTC(), true
}

func setDowngradeDetectedAt(auth *Auth, t time.Time) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[metadataDowngradeDetectedAtKey] = t.UTC().Format(time.RFC3339Nano)
}

func clearDowngradeDetectedAt(auth *Auth) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	delete(auth.Metadata, metadataDowngradeDetectedAtKey)
}

// applyPlanTypeRefreshDecision updates auth based on a fresh refresh-token
// outcome: jwtPlan is the plan_type claim extracted from the freshly-refreshed
// id_token (may be ""), realPlan is what /wham/usage returned right after the
// token refresh (only consulted when probeSucceeded is true), and now is the
// wall clock used for the downgrade-detected timestamp.
//
// Mutations are applied in this order:
//  1. If jwtPlan is non-empty, pin it as the submitted_plan_type (no-op when
//     already pinned — this is the ingestion fingerprint).
//  2. If probe failed, return. We never mutate plan_type/Disabled state
//     without an authoritative /wham/usage reading.
//  3. Write probed_plan_type and Attributes[plan_type] = realPlan.
//  4. 4-state decision on (submitted, real):
//     - paid + free:  take ownership of disable iff not already disabled for
//       another reason; record downgrade timestamp if first detection.
//     - paid + paid:  clear downgrade timestamp; re-enable only if WE own the
//       disable (our StatusMessage prefix matches).
//     - free + *:     no Disabled mutation (free-submitted accounts are out
//       of the downgrade-protection scope).
//     - paid + <something unrecognized>: treat like free (conservative).
//
// The function mutates auth in place and is a pure function of its inputs
// plus the auth's existing state — all side effects land on *auth.
func ApplyPlanTypeRefreshDecision(auth *Auth, jwtPlan, realPlan string, probeSucceeded bool, now time.Time) {
	if auth == nil {
		return
	}

	if strings.TrimSpace(jwtPlan) != "" {
		setSubmittedPlanType(auth, jwtPlan)
	}

	if !probeSucceeded {
		return
	}

	realNorm := strings.ToLower(strings.TrimSpace(realPlan))
	setProbedPlanType(auth, realNorm)

	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["plan_type"] = realNorm

	submitted := submittedPlanType(auth)

	switch {
	case isPaidPlan(submitted) && isFreePlan(realNorm):
		// Downgrade suspected. Ownership of this disable is tracked by the
		// downgrade_detected_at metadata (persisted to disk, survives reloads).
		// StatusMessage is set for operator visibility but is a best-effort
		// signal — it is NOT the authoritative owner flag (filestore does not
		// persist it, so it would reset to empty on every file reload).
		_, ownedByUs := downgradeDetectedAt(auth)
		if !auth.Disabled {
			auth.Disabled = true
			auth.Status = StatusDisabled
			auth.StatusMessage = fmt.Sprintf("%ssubmitted=%s upstream=%s",
				downgradeDetectedPrefix, submitted, realNorm)
			setDowngradeDetectedAt(auth, now.UTC())
		} else if ownedByUs {
			// We already own this disable — preserve original timestamp.
			// Refresh the StatusMessage for UI/log clarity (safe because we
			// verified ownership via the persisted timestamp).
			auth.StatusMessage = fmt.Sprintf("%ssubmitted=%s upstream=%s",
				downgradeDetectedPrefix, submitted, realNorm)
		}
		// If disabled for another reason (no downgrade timestamp), leave the
		// foreign disable alone.

	case isPaidPlan(submitted) && isPaidPlan(realNorm):
		// Confirmed paid. Re-enable only if WE disabled it (have a downgrade
		// timestamp — the authoritative, persisted ownership marker).
		if auth.Disabled {
			if _, ownedByUs := downgradeDetectedAt(auth); ownedByUs {
				auth.Disabled = false
				auth.Status = StatusActive
				auth.StatusMessage = ""
			}
		}
		clearDowngradeDetectedAt(auth)
	}
}
