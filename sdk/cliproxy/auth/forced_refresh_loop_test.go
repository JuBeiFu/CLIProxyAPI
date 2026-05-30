package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// forcedRefreshTestExecutor records which auth IDs had Refresh called and
// optionally simulates what /wham/usage probe would have returned for each
// by invoking ApplyPlanTypeRefreshDecision with the configured value.
type forcedRefreshTestExecutor struct {
	id           string
	mu           sync.Mutex
	refreshedIDs []string
	// probeResults maps auth ID → simulated probed plan_type. Empty value means
	// "probe failed, don't apply decision".
	probeResults map[string]string
}

func (e *forcedRefreshTestExecutor) Identifier() string { return e.id }
func (e *forcedRefreshTestExecutor) Execute(ctx context.Context, a *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *forcedRefreshTestExecutor) ExecuteStream(ctx context.Context, a *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}
func (e *forcedRefreshTestExecutor) CountTokens(ctx context.Context, a *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *forcedRefreshTestExecutor) HttpRequest(ctx context.Context, a *Auth, r *http.Request) (*http.Response, error) {
	return nil, nil
}
func (e *forcedRefreshTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.mu.Lock()
	e.refreshedIDs = append(e.refreshedIDs, auth.ID)
	probed, ok := e.probeResults[auth.ID]
	e.mu.Unlock()
	if ok && probed != "" {
		ApplyPlanTypeRefreshDecision(auth, submittedPlanType(auth), probed, true, time.Now())
	}
	return auth, nil
}
func (e *forcedRefreshTestExecutor) refreshed() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.refreshedIDs...)
}

// Only scope-matching auths get refreshed.
func TestRunForcedRefreshOnce_RefreshesScopeAuthsOnly(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, nil, nil)
	exec := &forcedRefreshTestExecutor{id: "codex", probeResults: map[string]string{}}
	m.RegisterExecutor(exec)

	m.auths = map[string]*Auth{
		"A_paid_unprobed":          mustAuthKeyed("A_paid_unprobed", "codex", "plus", "", false),
		"B_paid_free":              mustAuthKeyed("B_paid_free", "codex", "plus", "free", false),
		"C_paid_confirmed_bound":   mustAuthBound("C_paid_confirmed_bound", "codex", "plus", "plus", "free-proxy-1"),
		"D_free_unprobed":          mustAuthKeyed("D_free_unprobed", "codex", "free", "", false),
		"E_free_probed_free":       mustAuthKeyed("E_free_probed_free", "codex", "free", "free", false),
		"F_paid_confirmed_unbound": mustAuthKeyed("F_paid_confirmed_unbound", "codex", "plus", "plus", false),
	}

	m.runForcedRefreshOnce(context.Background(), DefaultDowngradeDeletionGrace)

	got := exec.refreshed()
	// Rule 1: never-probed auths are always in scope (including D, which was
	// submitted=free — the first probe is still needed to classify).
	// Rule 2: already-probed paid+free (B) stays in scope.
	// Rule 3: paid+paid WITHOUT a binding (F) is in scope — multi-path
	// probe needs to run so the auth gets pinned to a usable node.
	// Rule 4 (via HealthChecker) not exercised in this test.
	// Confirmed-paid WITH binding (C) and settled free+free (E) stay OUT.
	want := map[string]bool{
		"A_paid_unprobed":          true,
		"B_paid_free":              true,
		"D_free_unprobed":          true,
		"F_paid_confirmed_unbound": true,
	}
	if len(got) != len(want) {
		t.Fatalf("refreshed %v, want keys %v", got, want)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected refresh of %s", id)
		}
	}
}

// Disabled auths in scope ARE refreshed — that's the re-enable path.
func TestRunForcedRefreshOnce_IncludesDisabled(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, nil, nil)
	exec := &forcedRefreshTestExecutor{id: "codex", probeResults: map[string]string{}}
	m.RegisterExecutor(exec)

	m.auths = map[string]*Auth{
		"disabled_paid_free": mustAuthKeyed("disabled_paid_free", "codex", "plus", "free", true),
	}
	// Simulate that ApplyPlanTypeRefreshDecision had set disable + timestamp
	// earlier — but NOT yet past grace window.
	m.auths["disabled_paid_free"].StatusMessage = downgradeDetectedPrefix + "submitted=plus upstream=free"
	setDowngradeDetectedAt(m.auths["disabled_paid_free"], time.Now().Add(-30*time.Minute))

	m.runForcedRefreshOnce(context.Background(), DefaultDowngradeDeletionGrace)

	got := exec.refreshed()
	if len(got) != 1 || got[0] != "disabled_paid_free" {
		t.Fatalf("expected disabled auth refreshed, got %v", got)
	}
	// Still present (not deleted, since grace not yet elapsed)
	if _, present := m.auths["disabled_paid_free"]; !present {
		t.Fatalf("auth should still be in manager (within grace)")
	}
}

// Past the 2h grace window, still free → auth is deleted from the manager.
func TestRunForcedRefreshOnce_DeletesAfterGrace(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, nil, nil)
	exec := &forcedRefreshTestExecutor{id: "codex", probeResults: map[string]string{}}
	m.RegisterExecutor(exec)

	grace := 2 * time.Hour
	a := mustAuthKeyed("stale_downgrade", "codex", "plus", "free", true)
	a.StatusMessage = downgradeDetectedPrefix + "submitted=plus upstream=free"
	setDowngradeDetectedAt(a, time.Now().Add(-3*time.Hour)) // 3h ago > 2h grace
	clearRevokedAuthTombstoneForTest(t, a)
	m.auths = map[string]*Auth{"stale_downgrade": a}

	m.runForcedRefreshOnce(context.Background(), grace)

	if _, present := m.auths["stale_downgrade"]; present {
		t.Fatalf("auth must have been deleted (3h > 2h grace)")
	}
	// Because deletion short-circuits the refresh path, Refresh should NOT
	// have been called for this auth.
	if len(exec.refreshed()) != 0 {
		t.Fatalf("should not refresh after deletion, got %v", exec.refreshed())
	}
}

func TestRunForcedRefreshOnce_DeletesPendingActivationAfterGrace(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, nil, nil)
	exec := &forcedRefreshTestExecutor{id: "codex", probeResults: map[string]string{}}
	m.RegisterExecutor(exec)

	a := mustAuthKeyed("stale_pending_activation", "codex", "free", "free", false)
	a.CreatedAt = time.Now().Add(-DefaultPendingActivationDeletionGrace - time.Minute)
	a.Metadata[MetadataCodexWeeklyQuotaRemainingRatioKey] = 0.75
	clearRevokedAuthTombstoneForTest(t, a)
	m.auths = map[string]*Auth{"stale_pending_activation": a}

	m.runForcedRefreshOnce(context.Background(), DefaultDowngradeDeletionGrace)

	if _, present := m.auths["stale_pending_activation"]; present {
		t.Fatalf("pending activation auth must have been deleted after %s", DefaultPendingActivationDeletionGrace)
	}
	if len(exec.refreshed()) != 0 {
		t.Fatalf("should not refresh after pending activation deletion, got %v", exec.refreshed())
	}
}

// Before grace elapses, auth is kept and refreshed normally.
func TestRunForcedRefreshOnce_KeepsBeforeGrace(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, nil, nil)
	exec := &forcedRefreshTestExecutor{id: "codex", probeResults: map[string]string{}}
	m.RegisterExecutor(exec)

	grace := 2 * time.Hour
	a := mustAuthKeyed("recent_downgrade", "codex", "plus", "free", true)
	a.StatusMessage = downgradeDetectedPrefix + "submitted=plus upstream=free"
	setDowngradeDetectedAt(a, time.Now().Add(-30*time.Minute))
	m.auths = map[string]*Auth{"recent_downgrade": a}

	m.runForcedRefreshOnce(context.Background(), grace)

	if _, present := m.auths["recent_downgrade"]; !present {
		t.Fatalf("auth should be kept within grace")
	}
}

// A paid-confirmed auth (in scope? no!) shouldn't be refreshed by forced loop.
func TestRunForcedRefreshOnce_DoesNotTouchConfirmedPaid(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, nil, nil)
	exec := &forcedRefreshTestExecutor{id: "codex", probeResults: map[string]string{}}
	m.RegisterExecutor(exec)

	m.auths = map[string]*Auth{
		// paid+paid WITH binding: settled, out of scope.
		"confirmed_paid_bound": mustAuthBound("confirmed_paid_bound", "codex", "plus", "plus", "free-proxy-1"),
	}

	m.runForcedRefreshOnce(context.Background(), DefaultDowngradeDeletionGrace)

	if len(exec.refreshed()) != 0 {
		t.Fatalf("confirmed paid with binding must not be touched, got %v", exec.refreshed())
	}
}

func mustAuthKeyed(id, provider, submitted, probed string, disabled bool) *Auth {
	a := &Auth{
		ID:       id,
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

// mustAuthBound is like mustAuthKeyed but also pins the auth to the given
// pool-entry name. Used to exercise the "probed=paid WITH binding"
// exclusion branch of shouldForceRefresh.
func mustAuthBound(id, provider, submitted, probed, entry string) *Auth {
	a := mustAuthKeyed(id, provider, submitted, probed, false)
	SetBoundProxyEntry(a, entry)
	return a
}
