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
			name:   "paid_submitted_probed_plus_out",
			auth:   mustAuth("codex", "plus", "plus", false),
			expect: false,
		},
		{
			name:   "paid_submitted_probed_pro_out",
			auth:   mustAuth("codex", "pro", "pro", false),
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
