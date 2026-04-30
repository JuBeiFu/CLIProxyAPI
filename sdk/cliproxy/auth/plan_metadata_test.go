package auth

import (
	"testing"
	"time"
)

func TestIsPaidPlan(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"plus", true},
		{"pro", true},
		{"prolite", true},
		{"pro_lite", true},
		{"pro-lite", true},
		{"team", true},
		{"enterprise", true},
		{"Plus", true},   // case-insensitive
		{"  pro ", true}, // trimmed
		{"free", false},
		{"none", false},
		{"unknown", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isPaidPlan(c.in); got != c.want {
			t.Errorf("isPaidPlan(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestIsFreePlan(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"free", true},
		{"none", true},
		{"unknown", true},
		{"", true},
		{"Free", true},
		{"  none ", true},
		{"plus", false},
		{"pro", false},
		{"team", false},
		{"enterprise", false},
	}
	for _, c := range cases {
		if got := isFreePlan(c.in); got != c.want {
			t.Errorf("isFreePlan(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSubmittedPlanType_PinSemantics(t *testing.T) {
	t.Parallel()
	a := &Auth{Metadata: map[string]any{}}

	// Empty by default
	if got := submittedPlanType(a); got != "" {
		t.Fatalf("empty auth: got %q, want empty", got)
	}

	// First write succeeds
	setSubmittedPlanType(a, "plus")
	if got := submittedPlanType(a); got != "plus" {
		t.Fatalf("after first set: got %q, want plus", got)
	}

	// Second write must NOT overwrite (pin)
	setSubmittedPlanType(a, "free")
	if got := submittedPlanType(a); got != "plus" {
		t.Fatalf("after attempted overwrite: got %q, want plus (pinned)", got)
	}

	// Empty string does not pin
	b := &Auth{Metadata: map[string]any{}}
	setSubmittedPlanType(b, "")
	if got := submittedPlanType(b); got != "" {
		t.Fatalf("empty value should not pin: got %q", got)
	}
	// Now a real value pins
	setSubmittedPlanType(b, "pro")
	if got := submittedPlanType(b); got != "pro" {
		t.Fatalf("after pinning real value: got %q, want pro", got)
	}
}

func TestProbedPlanType_OverwriteSemantics(t *testing.T) {
	t.Parallel()
	a := &Auth{Metadata: map[string]any{}}

	if got := probedPlanType(a); got != "" {
		t.Fatalf("empty: got %q", got)
	}

	setProbedPlanType(a, "free")
	if got := probedPlanType(a); got != "free" {
		t.Fatalf("after first: got %q", got)
	}

	// Second write MUST overwrite (not a pin — it's live state)
	setProbedPlanType(a, "plus")
	if got := probedPlanType(a); got != "plus" {
		t.Fatalf("after overwrite: got %q, want plus", got)
	}
}

func TestDowngradeDetectedAt_SetReadClear(t *testing.T) {
	t.Parallel()
	a := &Auth{Metadata: map[string]any{}}

	if _, ok := downgradeDetectedAt(a); ok {
		t.Fatalf("empty auth should have no downgrade timestamp")
	}

	ts := time.Date(2026, 4, 19, 8, 0, 0, 0, time.UTC)
	setDowngradeDetectedAt(a, ts)
	got, ok := downgradeDetectedAt(a)
	if !ok {
		t.Fatalf("expected timestamp after set")
	}
	if !got.Equal(ts) {
		t.Fatalf("timestamp mismatch: got %v want %v", got, ts)
	}

	clearDowngradeDetectedAt(a)
	if _, ok := downgradeDetectedAt(a); ok {
		t.Fatalf("expected no timestamp after clear")
	}
}

func TestDowngradeDetectedAt_NilMetadataSafe(t *testing.T) {
	t.Parallel()
	a := &Auth{}
	if _, ok := downgradeDetectedAt(a); ok {
		t.Fatalf("nil metadata should return no timestamp")
	}
	// setDowngradeDetectedAt must handle nil metadata by allocating it
	setDowngradeDetectedAt(a, time.Now())
	if _, ok := downgradeDetectedAt(a); !ok {
		t.Fatalf("expected timestamp after set on nil-metadata auth")
	}
}
