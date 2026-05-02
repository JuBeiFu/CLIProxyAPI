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

	// MetadataBoundProxyEntryKey stores the pool-entry name this auth has
	// been bound to after the multi-path plan probe. Empirically the
	// /wham/usage plan_type differs across OpenAI edge nodes (different
	// egress IPs hit different regions whose plan_type caches are out of
	// sync), so we probe each pool entry until one reports a paid plan and
	// pin the auth to that entry. Both the probe and the real dispatch go
	// through the bound entry so OpenAI sees the same node for both, which
	// makes the cached plan_type decision trustworthy. Special value
	// BoundProxyEntryDirect means direct egress (all pool entries reported
	// free, direct reported paid). Empty string means unbound — either
	// never probed, or all paths returned free so the auth is genuinely
	// free and eligible for the 5min forced-refresh retry cycle.
	MetadataBoundProxyEntryKey = "cliproxy_bound_proxy_entry"

	// IPv6 bind lease metadata pins one auth to one dedicated host IPv6.
	// The manager owns assignment/release; request routing only reads it.
	MetadataIPv6BindLeasePoolKey  = "cliproxy_ipv6_bind_lease_pool"
	MetadataIPv6BindLeaseEntryKey = "cliproxy_ipv6_bind_lease_entry"
	MetadataIPv6BindLeaseIPKey    = "cliproxy_ipv6_bind_lease_ip"
	MetadataIPv6BindLeaseURLKey   = "cliproxy_ipv6_bind_lease_url"

	// MetadataCodexFiveHourQuotaRemainingRatioKey stores the remaining quota
	// ratio for Codex's rolling 5-hour window as returned by /wham/usage.
	MetadataCodexFiveHourQuotaRemainingRatioKey = "cliproxy_codex_5h_remaining_ratio"

	// MetadataCodexFiveHourQuotaResetAtKey stores the reset time for the same
	// rolling 5-hour window when /wham/usage returns it.
	MetadataCodexFiveHourQuotaResetAtKey = "cliproxy_codex_5h_reset_at"

	MetadataCodexFiveHourQuotaLimitKey     = "cliproxy_codex_5h_limit"
	MetadataCodexFiveHourQuotaRemainingKey = "cliproxy_codex_5h_remaining"
	MetadataCodexFiveHourQuotaUpdatedAtKey = "cliproxy_codex_5h_updated_at"

	MetadataCodexForceTokenRefreshKey = "cliproxy_codex_force_token_refresh"

	// BoundProxyEntryDirect is the sentinel value stored in
	// MetadataBoundProxyEntryKey when the auth should use direct egress
	// (no proxy) because every pool entry reported a free plan but direct
	// reported a paid plan.
	BoundProxyEntryDirect = "__direct__"

	// metadataDowngradeDetectedAtKey records when we first disabled an auth
	// because submitted==paid but probed==free. Cleared on re-enable. Used
	// to enforce the 2h grace window before permanent deletion.
	metadataDowngradeDetectedAtKey = "cliproxy_codex_downgrade_detected_at"

	// Unexported aliases to preserve existing internal references.
	metadataSubmittedPlanTypeKey = MetadataSubmittedPlanTypeKey
	metadataProbedPlanTypeKey    = MetadataProbedPlanTypeKey
	metadataBoundProxyEntryKey   = MetadataBoundProxyEntryKey
)

// IPv6BindLeaseInfo is the persisted per-auth IPv6 bind lease.
type IPv6BindLeaseInfo struct {
	Pool      string
	EntryName string
	IP        string
	URL       string
}

// BoundProxyEntry returns the pool entry name this auth is bound to, or
// empty string if unbound. BoundProxyEntryDirect is returned for auths
// pinned to direct egress.
func BoundProxyEntry(auth *Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	v, _ := auth.Metadata[metadataBoundProxyEntryKey].(string)
	return strings.TrimSpace(v)
}

// SetBoundProxyEntry overwrites the bound pool-entry name. Empty string
// clears the binding.
func SetBoundProxyEntry(auth *Auth, name string) {
	if auth == nil {
		return
	}
	v := strings.TrimSpace(name)
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	if v == "" {
		delete(auth.Metadata, metadataBoundProxyEntryKey)
		return
	}
	auth.Metadata[metadataBoundProxyEntryKey] = v
}

// IPv6BindLease returns the persisted IPv6 bind lease, if present.
func IPv6BindLease(auth *Auth) IPv6BindLeaseInfo {
	if auth == nil || auth.Metadata == nil {
		return IPv6BindLeaseInfo{}
	}
	pool, _ := auth.Metadata[MetadataIPv6BindLeasePoolKey].(string)
	entry, _ := auth.Metadata[MetadataIPv6BindLeaseEntryKey].(string)
	ip, _ := auth.Metadata[MetadataIPv6BindLeaseIPKey].(string)
	url, _ := auth.Metadata[MetadataIPv6BindLeaseURLKey].(string)
	return IPv6BindLeaseInfo{
		Pool:      strings.TrimSpace(pool),
		EntryName: strings.TrimSpace(entry),
		IP:        strings.TrimSpace(ip),
		URL:       strings.TrimSpace(url),
	}
}

// SetIPv6BindLease overwrites the persisted IPv6 bind lease. Empty IP or URL
// clears the lease.
func SetIPv6BindLease(auth *Auth, lease IPv6BindLeaseInfo) {
	if auth == nil {
		return
	}
	lease.Pool = strings.TrimSpace(lease.Pool)
	lease.EntryName = strings.TrimSpace(lease.EntryName)
	lease.IP = strings.TrimSpace(lease.IP)
	lease.URL = strings.TrimSpace(lease.URL)
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	if lease.IP == "" || lease.URL == "" {
		ClearIPv6BindLease(auth)
		return
	}
	auth.Metadata[MetadataIPv6BindLeasePoolKey] = lease.Pool
	auth.Metadata[MetadataIPv6BindLeaseEntryKey] = lease.EntryName
	auth.Metadata[MetadataIPv6BindLeaseIPKey] = lease.IP
	auth.Metadata[MetadataIPv6BindLeaseURLKey] = lease.URL
}

// ClearIPv6BindLease removes any persisted IPv6 bind lease metadata.
func ClearIPv6BindLease(auth *Auth) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	delete(auth.Metadata, MetadataIPv6BindLeasePoolKey)
	delete(auth.Metadata, MetadataIPv6BindLeaseEntryKey)
	delete(auth.Metadata, MetadataIPv6BindLeaseIPKey)
	delete(auth.Metadata, MetadataIPv6BindLeaseURLKey)
}

func isPaidPlan(planType string) bool {
	switch normalizedPlanTypeKey(planType) {
	case "plus", "pro", "prolite", "team", "enterprise":
		return true
	}
	return false
}

func isFreePlan(planType string) bool {
	switch normalizedPlanTypeKey(planType) {
	case "", "free", "none", "unknown":
		return true
	}
	return false
}

func normalizedPlanTypeKey(planType string) string {
	key := strings.ToLower(strings.TrimSpace(planType))
	key = strings.ReplaceAll(key, "_", "")
	key = strings.ReplaceAll(key, "-", "")
	key = strings.ReplaceAll(key, " ", "")
	return key
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
//     another reason; record downgrade timestamp if first detection.
//     - paid + paid:  clear downgrade timestamp; re-enable only if WE own the
//     disable (our StatusMessage prefix matches).
//     - free + *:     no Disabled mutation (free-submitted accounts are out
//     of the downgrade-protection scope).
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
