package auth

import (
	"context"
	"testing"
	"time"
)

func TestRefreshCodexAuths_RecoversCoolingAndUpgrades(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(nil, nil, nil)

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
	// Register the executor AFTER Register so the fire-and-forget probe inside
	// Register (which fires only when an executor already exists) does not race
	// the explicit refresh and clear the cooling state before we snapshot it.
	exec := &codexQuotaRefreshExecutor{updated: &Auth{Metadata: map[string]any{
		MetadataCodexFiveHourQuotaRemainingRatioKey: 0.9,
	}}}
	mgr.RegisterExecutor(exec)

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

func TestStartAndGetRefreshCodexJob(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(nil, nil, nil)

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
	// Register the executor AFTER Register so the fire-and-forget probe inside
	// Register does not race the explicit refresh.
	mgr.RegisterExecutor(&codexQuotaRefreshExecutor{updated: &Auth{Metadata: map[string]any{
		MetadataCodexFiveHourQuotaRemainingRatioKey: 0.9,
	}}})

	jobID, started := mgr.StartRefreshCodexAuths(true)
	if !started {
		t.Fatalf("expected started=true on first start")
	}
	if jobID == "" {
		t.Fatalf("expected non-empty job id")
	}

	var view RefreshCodexJobView
	done := false
	for i := 0; i < 50; i++ {
		v, ok := mgr.GetRefreshCodexJob(jobID)
		if !ok {
			t.Fatalf("job %s not found", jobID)
		}
		view = v
		if v.Status == RefreshJobDone {
			done = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !done {
		t.Fatalf("job did not complete; last view=%+v", view)
	}
	if view.Total != 1 {
		t.Fatalf("total = %d, want 1", view.Total)
	}
	if view.Done != 1 {
		t.Fatalf("done = %d, want 1", view.Done)
	}
	if view.Recovered != 1 {
		t.Fatalf("recovered = %d, want 1; results=%+v", view.Recovered, view.Results)
	}

	// After completion a second start creates a NEW job (the prior one is done,
	// so the single-job slot is free).
	jobID2, started2 := mgr.StartRefreshCodexAuths(true)
	if !started2 {
		t.Fatalf("expected started=true after prior job done")
	}
	if jobID2 == jobID {
		t.Fatalf("expected a new job id after completion, got same id %s", jobID2)
	}
}

func TestRefreshJobIsStale(t *testing.T) {
	// Recent running job: not stale.
	recent := &RefreshCodexJob{Status: RefreshJobRunning, StartedAt: time.Now()}
	if refreshJobIsStale(recent) {
		t.Fatalf("recent running job should not be stale")
	}

	// Running job older than timeout + 1min grace: stale.
	old := &RefreshCodexJob{Status: RefreshJobRunning, StartedAt: time.Now().Add(-(refreshCodexJobTimeout + 2*time.Minute))}
	if !refreshJobIsStale(old) {
		t.Fatalf("running job older than timeout+grace should be stale")
	}

	// Just under the timeout + grace threshold: not stale.
	justUnder := &RefreshCodexJob{Status: RefreshJobRunning, StartedAt: time.Now().Add(-(refreshCodexJobTimeout + 30*time.Second))}
	if refreshJobIsStale(justUnder) {
		t.Fatalf("running job under timeout+grace should not be stale")
	}

	// Done job is never stale, even if very old.
	done := &RefreshCodexJob{Status: RefreshJobDone, StartedAt: time.Now().Add(-(refreshCodexJobTimeout + time.Hour))}
	if refreshJobIsStale(done) {
		t.Fatalf("done job should never be stale")
	}

	// Zero StartedAt is not stale.
	zero := &RefreshCodexJob{Status: RefreshJobRunning}
	if refreshJobIsStale(zero) {
		t.Fatalf("zero-StartedAt job should not be stale")
	}

	// Nil job is not stale.
	if refreshJobIsStale(nil) {
		t.Fatalf("nil job should not be stale")
	}
}

func TestStartRefreshCodexAuths_StaleJobAllowsNewStart(t *testing.T) {
	// Shrink the timeout so the staleness threshold is tiny; restore after.
	saved := refreshCodexJobTimeout
	refreshCodexJobTimeout = time.Millisecond
	defer func() { refreshCodexJobTimeout = saved }()

	mgr := NewManager(nil, nil, nil)

	// Install a wedged "running" job whose StartedAt is far in the past. With the
	// shrunken timeout it is comfortably stale, so it must NOT block a new start.
	mgr.refreshJob = &RefreshCodexJob{
		ID:        "old",
		Status:    RefreshJobRunning,
		StartedAt: time.Now().Add(-time.Hour),
	}

	jobID, started := mgr.StartRefreshCodexAuths(true)
	if !started {
		t.Fatalf("expected started=true when prior job is stale")
	}
	if jobID == "old" {
		t.Fatalf("expected a NEW job id, got the stale job id %q", jobID)
	}
	if jobID == "" {
		t.Fatalf("expected a non-empty new job id")
	}
}
