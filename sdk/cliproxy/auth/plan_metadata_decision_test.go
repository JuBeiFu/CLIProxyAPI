package auth

import (
	"strings"
	"testing"
	"time"
)

func newTestAuthMeta() *Auth {
	return &Auth{
		ID:         "test-auth",
		Provider:   "codex",
		Metadata:   map[string]any{},
		Attributes: map[string]string{},
	}
}

// Case 1: submitted=paid + probed=free (first detection) → disable with
// downgrade marker, timestamp set, Attributes reflect real.
func TestApplyPlanTypeRefreshDecision_DowngradeFirstDetection(t *testing.T) {
	t.Parallel()
	a := newTestAuthMeta()
	setSubmittedPlanType(a, "plus")
	now := time.Date(2026, 4, 19, 9, 0, 0, 0, time.UTC)

	ApplyPlanTypeRefreshDecision(a, "plus", "free", true, now)

	if !a.Disabled {
		t.Fatalf("expected Disabled=true after downgrade detection")
	}
	if a.Status != StatusDisabled {
		t.Fatalf("Status = %v, want StatusDisabled", a.Status)
	}
	if !strings.HasPrefix(a.StatusMessage, downgradeDetectedPrefix) {
		t.Fatalf("StatusMessage should start with %q, got %q", downgradeDetectedPrefix, a.StatusMessage)
	}
	ts, ok := downgradeDetectedAt(a)
	if !ok || !ts.Equal(now) {
		t.Fatalf("downgrade timestamp mismatch: got %v ok=%v, want %v", ts, ok, now)
	}
	if a.Attributes["plan_type"] != "free" {
		t.Fatalf("Attributes[plan_type] = %q, want free", a.Attributes["plan_type"])
	}
	if probedPlanType(a) != "free" {
		t.Fatalf("probed = %q, want free", probedPlanType(a))
	}
}

// Case 2: paid+paid AFTER being disabled-by-downgrade → re-enable, clear timer.
func TestApplyPlanTypeRefreshDecision_ReEnableAfterUpgrade(t *testing.T) {
	t.Parallel()
	a := newTestAuthMeta()
	setSubmittedPlanType(a, "plus")
	// Simulate previously-detected downgrade state
	a.Disabled = true
	a.Status = StatusDisabled
	a.StatusMessage = downgradeDetectedPrefix + "submitted=plus upstream=free"
	setDowngradeDetectedAt(a, time.Date(2026, 4, 19, 7, 0, 0, 0, time.UTC))
	now := time.Date(2026, 4, 19, 9, 0, 0, 0, time.UTC)

	ApplyPlanTypeRefreshDecision(a, "plus", "plus", true, now)

	if a.Disabled {
		t.Fatalf("expected re-enabled (Disabled=false)")
	}
	if a.Status != StatusActive {
		t.Fatalf("Status = %v, want StatusActive", a.Status)
	}
	if a.StatusMessage != "" {
		t.Fatalf("StatusMessage should be cleared, got %q", a.StatusMessage)
	}
	if _, ok := downgradeDetectedAt(a); ok {
		t.Fatalf("downgrade timestamp should be cleared")
	}
	if a.Attributes["plan_type"] != "plus" {
		t.Fatalf("Attributes[plan_type] = %q, want plus", a.Attributes["plan_type"])
	}
}

// Case 3: paid+paid but disabled for a DIFFERENT reason (e.g. 401) → DO NOT
// re-enable; our logic must not touch unrelated disables.
func TestApplyPlanTypeRefreshDecision_DoNotTouchForeignDisable(t *testing.T) {
	t.Parallel()
	a := newTestAuthMeta()
	setSubmittedPlanType(a, "plus")
	a.Disabled = true
	a.Status = StatusDisabled
	a.StatusMessage = "revoked: refresh_token_reused"
	now := time.Date(2026, 4, 19, 9, 0, 0, 0, time.UTC)

	ApplyPlanTypeRefreshDecision(a, "plus", "plus", true, now)

	if !a.Disabled {
		t.Fatalf("foreign-disabled auth must remain disabled")
	}
	if a.StatusMessage != "revoked: refresh_token_reused" {
		t.Fatalf("foreign StatusMessage got overwritten: %q", a.StatusMessage)
	}
}

// Case 4: submitted=free + probed=free (genuine free account) → no disable,
// attributes updated.
func TestApplyPlanTypeRefreshDecision_SubmittedFreeNoAction(t *testing.T) {
	t.Parallel()
	a := newTestAuthMeta()
	setSubmittedPlanType(a, "free")
	now := time.Date(2026, 4, 19, 9, 0, 0, 0, time.UTC)

	ApplyPlanTypeRefreshDecision(a, "free", "free", true, now)

	if a.Disabled {
		t.Fatalf("submitted=free should never auto-disable")
	}
	if a.Attributes["plan_type"] != "free" {
		t.Fatalf("Attributes[plan_type] = %q, want free", a.Attributes["plan_type"])
	}
	if _, ok := downgradeDetectedAt(a); ok {
		t.Fatalf("no downgrade timer for submitted=free")
	}
}

// Case 5: probe failed → don't touch Attributes, Disabled, probed, timestamp.
// Only JWT-based backfill of submitted pin is allowed.
func TestApplyPlanTypeRefreshDecision_ProbeFailedNoMutation(t *testing.T) {
	t.Parallel()
	a := newTestAuthMeta()
	a.Attributes["plan_type"] = "plus"
	setSubmittedPlanType(a, "plus")
	setProbedPlanType(a, "plus")

	ApplyPlanTypeRefreshDecision(a, "plus", "", false, time.Now().UTC())

	if a.Disabled {
		t.Fatalf("probe-failed must not disable")
	}
	if a.Attributes["plan_type"] != "plus" {
		t.Fatalf("probe-failed must not overwrite Attributes[plan_type], got %q", a.Attributes["plan_type"])
	}
	if probedPlanType(a) != "plus" {
		t.Fatalf("probe-failed must not overwrite probed, got %q", probedPlanType(a))
	}
}

// Case 6: auth with empty submitted pin + JWT says plus + probe says free →
// backfill pin FROM JWT first, THEN apply downgrade logic using the newly
// pinned submitted value.
func TestApplyPlanTypeRefreshDecision_FirstBackfillThenDowngrade(t *testing.T) {
	t.Parallel()
	a := newTestAuthMeta()
	// No submitted pin yet
	if submittedPlanType(a) != "" {
		t.Fatalf("precondition: submitted must be empty")
	}
	now := time.Date(2026, 4, 19, 9, 0, 0, 0, time.UTC)

	ApplyPlanTypeRefreshDecision(a, "plus", "free", true, now)

	if submittedPlanType(a) != "plus" {
		t.Fatalf("submitted should have been backfilled to plus, got %q", submittedPlanType(a))
	}
	if !a.Disabled {
		t.Fatalf("expected disabled after backfill + downgrade")
	}
	if !strings.HasPrefix(a.StatusMessage, downgradeDetectedPrefix) {
		t.Fatalf("expected downgrade prefix in status, got %q", a.StatusMessage)
	}
}

// Case 7: paid-submitted + downgrade detected, then RE-CALLED while still
// probed=free — timestamp MUST NOT advance (we preserve first-detection time
// for the 2h grace window).
func TestApplyPlanTypeRefreshDecision_TimestampStable(t *testing.T) {
	t.Parallel()
	a := newTestAuthMeta()
	setSubmittedPlanType(a, "plus")
	firstDetection := time.Date(2026, 4, 19, 9, 0, 0, 0, time.UTC)
	ApplyPlanTypeRefreshDecision(a, "plus", "free", true, firstDetection)
	ts1, ok := downgradeDetectedAt(a)
	if !ok {
		t.Fatalf("precondition: expected timestamp after first detection")
	}

	// Second call 30 minutes later, still free
	secondCall := firstDetection.Add(30 * time.Minute)
	ApplyPlanTypeRefreshDecision(a, "plus", "free", true, secondCall)

	ts2, ok := downgradeDetectedAt(a)
	if !ok {
		t.Fatalf("expected timestamp preserved")
	}
	if !ts2.Equal(ts1) {
		t.Fatalf("timestamp advanced: first=%v second=%v (must be stable)", ts1, ts2)
	}
}

// Case 8: paid-submitted, flipped free→plus→free — timestamp must RESET on
// each flip (re-enable clears, re-disable sets new timer).
func TestApplyPlanTypeRefreshDecision_TimestampResetsOnFlip(t *testing.T) {
	t.Parallel()
	a := newTestAuthMeta()
	setSubmittedPlanType(a, "plus")

	t1 := time.Date(2026, 4, 19, 9, 0, 0, 0, time.UTC)
	ApplyPlanTypeRefreshDecision(a, "plus", "free", true, t1) // disable
	ts1, _ := downgradeDetectedAt(a)

	t2 := t1.Add(10 * time.Minute)
	ApplyPlanTypeRefreshDecision(a, "plus", "plus", true, t2) // re-enable
	if _, ok := downgradeDetectedAt(a); ok {
		t.Fatalf("timestamp should be cleared after re-enable")
	}

	t3 := t2.Add(5 * time.Minute)
	ApplyPlanTypeRefreshDecision(a, "plus", "free", true, t3) // re-disable
	ts3, ok := downgradeDetectedAt(a)
	if !ok {
		t.Fatalf("expected fresh timestamp after re-disable")
	}
	if !ts3.Equal(t3) {
		t.Fatalf("re-disable timestamp = %v, want %v", ts3, t3)
	}
	if ts3.Equal(ts1) {
		t.Fatalf("timestamps must differ across flip, both = %v", ts1)
	}
}

func TestDowngradeDetectedPrefix_IsStable(t *testing.T) {
	t.Parallel()
	// Guard against accidental rename — re-enable logic depends on this exact prefix.
	if downgradeDetectedPrefix != "codex_downgrade_detected: " {
		t.Fatalf("downgradeDetectedPrefix changed unexpectedly: %q", downgradeDetectedPrefix)
	}
}
