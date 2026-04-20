package auth

import (
	"testing"
	"time"
)

// shouldForceRefresh returns true for auths within the 5-min short-cycle scope:
// submitted plan_type is paid AND (probed plan_type is empty OR probed is free/none).
// Disabled flag is irrelevant — disabled auths MUST still be probed so they can
// be re-enabled on upgrade activation. Non-codex auths are out of scope.
func TestShouldForceRefresh_Scope(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		auth      *Auth
		expect    bool
		rationale string
	}{
		{
			name:   "paid_submitted_never_probed",
			auth:   mustAuth("codex", "plus", "", false),
			expect: true,
		},
		{
			name:   "paid_submitted_probed_free",
			auth:   mustAuth("codex", "plus", "free", false),
			expect: true,
		},
		{
			name:   "paid_submitted_probed_none",
			auth:   mustAuth("codex", "plus", "none", false),
			expect: true,
		},
		{
			// Confirmed paid WITH binding: settled, out of scope.
			name:   "paid_submitted_probed_plus_bound_out",
			auth:   mustAuthWithBinding("codex", "plus", "plus", false, "free-proxy-1"),
			expect: false,
		},
		{
			name:   "paid_submitted_probed_pro_bound_out",
			auth:   mustAuthWithBinding("codex", "pro", "pro", false, "free-proxy-2"),
			expect: false,
		},
		{
			// Confirmed paid WITHOUT binding: multi-path probe hasn't pinned
			// a node yet. Must re-probe so dispatch has a trustworthy egress.
			name:   "paid_submitted_probed_plus_unbound_in",
			auth:   mustAuth("codex", "plus", "plus", false),
			expect: true,
		},
		{
			// Direct-egress binding counts as settled (multi-path probe
			// picked direct as the paid-reporting path).
			name:   "paid_submitted_probed_plus_bound_direct_out",
			auth:   mustAuthWithBinding("codex", "plus", "plus", false, BoundProxyEntryDirect),
			expect: false,
		},
		{
			// Rule 5: bound paid with FRESH last_refresh (within reprobe
			// interval) is still settled — no stale binding yet.
			name:   "paid_bound_fresh_refresh_out",
			auth:   mustAuthBoundWithRefresh("codex", "plus", "plus", "free-proxy-1", time.Now().Add(-DefaultBoundReprobeInterval/2)),
			expect: false,
		},
		{
			// Rule 5: bound paid whose binding is OLDER than the reprobe
			// interval is back in scope — OpenAI's per-edge cache may have
			// flipped plus→free silently on that entry.
			name:   "paid_bound_stale_refresh_in",
			auth:   mustAuthBoundWithRefresh("codex", "plus", "plus", "free-proxy-1", time.Now().Add(-DefaultBoundReprobeInterval-time.Second)),
			expect: true,
		},
		{
			// Rule 5 must NOT fire when LastRefreshedAt is zero (never
			// recorded) AND Metadata["last_refresh"] is absent. We don't
			// want to spam re-probes on mocked/test auths that lack the
			// timestamp entirely.
			name:   "paid_bound_zero_refresh_out",
			auth:   mustAuthWithBinding("codex", "plus", "plus", false, "free-proxy-1"),
			expect: false,
		},
		{
			// Rule 5 must fire when LastRefreshedAt is zero but
			// Metadata["last_refresh"] says the refresh was long ago. This
			// is the file-loaded-auth case: LastRefreshedAt is only
			// populated in-process on actual refresh; file-hydrated auths
			// carry only the metadata string until their first in-process
			// refresh.
			name: "paid_bound_metadata_only_stale_in",
			auth: func() *Auth {
				a := mustAuthWithBinding("codex", "plus", "plus", false, "free-proxy-1")
				stale := time.Now().Add(-DefaultBoundReprobeInterval - time.Minute).UTC().Format(time.RFC3339)
				a.Metadata["last_refresh"] = stale
				return a
			}(),
			expect: true,
		},
		{
			// Metadata-only fresh: within reprobe window, should stay out.
			name: "paid_bound_metadata_only_fresh_out",
			auth: func() *Auth {
				a := mustAuthWithBinding("codex", "plus", "plus", false, "free-proxy-1")
				fresh := time.Now().Add(-DefaultBoundReprobeInterval / 2).UTC().Format(time.RFC3339)
				a.Metadata["last_refresh"] = fresh
				return a
			}(),
			expect: false,
		},
		{
			// Never-probed: include regardless of submitted so every auth
			// gets its first-ever /wham/usage classification.
			name:   "free_submitted_never_probed_IN_for_first_probe",
			auth:   mustAuth("codex", "free", "", false),
			expect: true,
		},
		{
			// No submitted pin either — still probe once to classify.
			name:   "empty_submitted_never_probed_IN_for_first_probe",
			auth:   mustAuth("codex", "", "", false),
			expect: true,
		},
		{
			// Probed confirmed free AND submitted was free → settled, exit scope.
			name:   "free_submitted_probed_free_out",
			auth:   mustAuth("codex", "free", "free", false),
			expect: false,
		},
		{
			// Probed free but submitted pin missing → settled (no paid contract
			// to watch for). Exit scope.
			name:   "empty_submitted_probed_free_out",
			auth:   mustAuth("codex", "", "free", false),
			expect: false,
		},
		{
			name:   "paid_submitted_probed_free_disabled_still_in",
			auth:   mustAuth("codex", "plus", "free", true),
			expect: true,
		},
		{
			name:   "non_codex_provider_out",
			auth:   mustAuth("gemini", "plus", "free", false),
			expect: false,
		},
		{
			name:   "nil_auth_safe",
			auth:   nil,
			expect: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldForceRefresh(c.auth); got != c.expect {
				t.Errorf("shouldForceRefresh(%s): got %v, want %v", c.name, got, c.expect)
			}
		})
	}
}

// mustAuthWithBinding builds an auth and pins it to the given pool-entry
// name (or BoundProxyEntryDirect). Used to exercise the "probed=paid WITH
// binding" settled branch.
func mustAuthWithBinding(provider, submitted, probed string, disabled bool, entry string) *Auth {
	a := mustAuth(provider, submitted, probed, disabled)
	SetBoundProxyEntry(a, entry)
	return a
}

// mustAuthBoundWithRefresh builds a bound auth AND sets LastRefreshedAt so
// the periodic-reprobe rule can be exercised. Freshness of refreshedAt
// determines whether the stale-binding rule fires.
func mustAuthBoundWithRefresh(provider, submitted, probed, entry string, refreshedAt time.Time) *Auth {
	a := mustAuthWithBinding(provider, submitted, probed, false, entry)
	a.LastRefreshedAt = refreshedAt
	return a
}

func mustAuth(provider, submitted, probed string, disabled bool) *Auth {
	a := &Auth{
		ID:       "a-" + provider + "-" + submitted + "-" + probed,
		Provider: provider,
		Metadata: map[string]any{},
		Disabled: disabled,
	}
	if submitted != "" {
		setSubmittedPlanType(a, submitted)
	}
	if probed != "" {
		setProbedPlanType(a, probed)
	}
	return a
}

func TestShouldDeleteDowngradedAuth_GraceWindow(t *testing.T) {
	t.Parallel()
	grace := 2 * time.Hour
	firstDetect := time.Date(2026, 4, 19, 9, 0, 0, 0, time.UTC)

	// Auth previously disabled-by-downgrade, probed still free.
	a := mustAuth("codex", "plus", "free", true)
	a.StatusMessage = downgradeDetectedPrefix + "submitted=plus upstream=free"
	setDowngradeDetectedAt(a, firstDetect)

	// Exactly at the boundary — NOT yet eligible for deletion.
	if shouldDeleteDowngradedAuth(a, firstDetect.Add(grace), grace) {
		t.Fatalf("boundary: should NOT delete at exactly grace elapsed")
	}

	// One nanosecond past the boundary — eligible.
	if !shouldDeleteDowngradedAuth(a, firstDetect.Add(grace).Add(time.Nanosecond), grace) {
		t.Fatalf("past boundary: expected delete eligible")
	}

	// Before boundary — not eligible.
	if shouldDeleteDowngradedAuth(a, firstDetect.Add(grace-time.Minute), grace) {
		t.Fatalf("before boundary: should not delete")
	}
}

func TestShouldDeleteDowngradedAuth_RequiresOurPrefix(t *testing.T) {
	t.Parallel()
	firstDetect := time.Date(2026, 4, 19, 9, 0, 0, 0, time.UTC)
	grace := 2 * time.Hour

	// Disabled auth with foreign reason — must NEVER delete, even if a stale
	// downgrade timestamp somehow leaked in.
	a := mustAuth("codex", "plus", "free", true)
	a.StatusMessage = "revoked: refresh_token_reused"
	setDowngradeDetectedAt(a, firstDetect)

	if shouldDeleteDowngradedAuth(a, firstDetect.Add(3*time.Hour), grace) {
		t.Fatalf("foreign-disabled auth must not be deleted by downgrade grace")
	}
}

func TestShouldDeleteDowngradedAuth_RequiresCurrentlyFree(t *testing.T) {
	t.Parallel()
	firstDetect := time.Date(2026, 4, 19, 9, 0, 0, 0, time.UTC)
	grace := 2 * time.Hour

	// Probed flipped to paid — must not delete (decision fn would also have
	// cleared the timestamp, but guard defensively).
	a := mustAuth("codex", "plus", "plus", true)
	a.StatusMessage = downgradeDetectedPrefix + "submitted=plus upstream=free"
	setDowngradeDetectedAt(a, firstDetect)

	if shouldDeleteDowngradedAuth(a, firstDetect.Add(3*time.Hour), grace) {
		t.Fatalf("must not delete when probed flipped to paid")
	}
}

func TestShouldDeleteDowngradedAuth_NoTimestampNoDelete(t *testing.T) {
	t.Parallel()
	grace := 2 * time.Hour

	a := mustAuth("codex", "plus", "free", true)
	a.StatusMessage = downgradeDetectedPrefix + "submitted=plus upstream=free"
	// No downgradeDetectedAt set.

	if shouldDeleteDowngradedAuth(a, time.Now().Add(10*time.Hour), grace) {
		t.Fatalf("no timestamp → must not delete")
	}
}

func TestShouldDeleteDowngradedAuth_NilOrNotDisabled(t *testing.T) {
	t.Parallel()
	grace := 2 * time.Hour
	now := time.Now()

	if shouldDeleteDowngradedAuth(nil, now, grace) {
		t.Fatalf("nil auth")
	}

	a := mustAuth("codex", "plus", "free", false) // NOT disabled
	a.StatusMessage = downgradeDetectedPrefix + "submitted=plus upstream=free"
	setDowngradeDetectedAt(a, now.Add(-3*time.Hour))
	if shouldDeleteDowngradedAuth(a, now, grace) {
		t.Fatalf("not-disabled auth must not be auto-deleted via this path")
	}
}
