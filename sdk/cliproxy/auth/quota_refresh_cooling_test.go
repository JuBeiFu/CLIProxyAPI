package auth

import (
	"context"
	"testing"
	"time"
)

func TestRefreshCodexAuths_RecoversCoolingAndUpgrades(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(nil, nil, nil)

	// Executor that, on Refresh, marks the auth plus + 5h quota healthy.
	exec := &codexQuotaRefreshExecutor{updated: &Auth{Metadata: map[string]any{
		MetadataCodexFiveHourQuotaRemainingRatioKey: 0.9,
	}}}
	mgr.RegisterExecutor(exec)

	// Cooling free auth.
	cooling := &Auth{
		ID:             "cooling",
		Provider:       "codex",
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: time.Now().Add(time.Hour),
		Quota:          QuotaState{Exceeded: true, Reason: codexFiveHourQuotaLowReason, NextRecoverAt: time.Now().Add(time.Hour)},
		Metadata:       map[string]any{"type": "codex", "access_token": "tok"},
	}
	if _, err := mgr.Register(ctx, cooling); err != nil {
		t.Fatalf("register: %v", err)
	}

	res := mgr.RefreshCodexAuths(ctx, true)
	if res.Total != 1 {
		t.Fatalf("total = %d, want 1", res.Total)
	}
	if res.Recovered != 1 {
		t.Fatalf("recovered = %d, want 1; results=%+v", res.Recovered, res.Results)
	}
	stored, _ := mgr.GetByID("cooling")
	if stored.Unavailable || stored.Quota.Exceeded {
		t.Fatalf("expected cooling cleared, got unavailable=%v quota=%+v", stored.Unavailable, stored.Quota)
	}
}

func TestRefreshCodexAuths_CoolingOnlySkipsHealthy(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(nil, nil, nil)
	mgr.RegisterExecutor(&codexQuotaRefreshExecutor{})

	healthy := &Auth{ID: "healthy", Provider: "codex", Status: StatusActive,
		Metadata: map[string]any{"type": "codex", "access_token": "tok"}}
	if _, err := mgr.Register(ctx, healthy); err != nil {
		t.Fatalf("register: %v", err)
	}

	res := mgr.RefreshCodexAuths(ctx, true) // cooling_only
	if res.Total != 0 {
		t.Fatalf("cooling_only total = %d, want 0 (healthy skipped)", res.Total)
	}

	resAll := mgr.RefreshCodexAuths(ctx, false) // all
	if resAll.Total != 1 {
		t.Fatalf("all total = %d, want 1", resAll.Total)
	}
}
