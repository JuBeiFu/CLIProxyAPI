package auth

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestFreePlanModelBlocked(t *testing.T) {
	// no gate registered => never blocks
	FreePlanModelGate = nil
	if freePlanModelBlocked(mustAuth("codex", "free", "free", false), "gpt-5.4") {
		t.Fatal("nil gate must not block")
	}

	// gate allows only gpt-5.5
	FreePlanModelGate = func(model string) bool { return canonicalModelKey(model) == "gpt-5.5" }
	defer func() { FreePlanModelGate = nil }()

	free := mustAuth("codex", "free", "free", false)
	if !freePlanModelBlocked(free, "gpt-5.4") {
		t.Fatal("free auth must be blocked for gpt-5.4")
	}
	if freePlanModelBlocked(free, "gpt-5.5(high)") {
		t.Fatal("free auth must NOT be blocked for gpt-5.5 thinking variant")
	}

	// paid auth never blocked
	plus := mustAuth("codex", "plus", "plus", false)
	if freePlanModelBlocked(plus, "gpt-5.4") {
		t.Fatal("plus auth must not be blocked")
	}

	// empty/unknown plan (not yet probed) never blocked
	unknown := mustAuth("codex", "", "", false)
	if freePlanModelBlocked(unknown, "gpt-5.4") {
		t.Fatal("unprobed auth must not be blocked")
	}

	// empty model route never blocked
	if freePlanModelBlocked(free, "") {
		t.Fatal("empty model must not be blocked")
	}
}

// TestSchedulerPick_FreePlanModelGateGuardsDefaultPath proves the scheduler's
// default selection predicate (pickSingle) actually consults the free-plan
// model gate, not just the legacy conductor fallback loops. We register a
// SINGLE free-plan codex auth whose registry model set is intentionally
// over-broad (BOTH a blocked model "gpt-5.5" and an allowed model "gpt-5.4"),
// simulating the stale window where a refresh has flipped the auth to
// plan=free but the registry/shard has not yet been re-filtered. With the gate
// active, the scheduler must REFUSE to serve the blocked model from this free
// auth while still serving the allowed model.
func TestSchedulerPick_FreePlanModelGateGuardsDefaultPath(t *testing.T) {
	const authID = "free-gate-overbroad"
	const blockedModel = "gpt-5.5"
	const allowedModel = "gpt-5.4"

	reg := registry.GetGlobalRegistry()
	// Over-broad / stale registration: the free auth is registered for BOTH
	// the blocked and the allowed model.
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{
		{ID: blockedModel},
		{ID: allowedModel},
	})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	// An explicitly free-plan codex auth (plan_type=free).
	freeAuth := &Auth{ID: authID, Provider: "codex", Metadata: map[string]any{}}
	setProbedPlanType(freeAuth, "free")
	if normalizedPlanTypeKey(codexPlanTypeForAuth(freeAuth)) != "free" {
		t.Fatalf("test setup: expected free plan, got %q", codexPlanTypeForAuth(freeAuth))
	}

	scheduler := newSchedulerForTest(&FillFirstSelector{}, freeAuth)

	// Gate allows only the allowed model for free plans.
	FreePlanModelGate = func(m string) bool { return canonicalModelKey(m) == allowedModel }
	defer func() { FreePlanModelGate = nil }()

	// Blocked model: the scheduler predicate must reject the free auth, so
	// selection yields NO auth (auth-unavailable error).
	got, errPick := scheduler.pickSingle(context.Background(), "codex", blockedModel, cliproxyexecutor.Options{}, nil)
	if got != nil {
		t.Fatalf("pickSingle(%s) selected %q, want none (free auth must be gated out)", blockedModel, got.ID)
	}
	if errPick == nil {
		t.Fatalf("pickSingle(%s) error = nil, want auth-unavailable error", blockedModel)
	}

	// Allowed model: the same free auth is fine, so selection succeeds.
	got, errPick = scheduler.pickSingle(context.Background(), "codex", allowedModel, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle(%s) error = %v, want success", allowedModel, errPick)
	}
	if got == nil || got.ID != authID {
		t.Fatalf("pickSingle(%s) auth = %v, want %s", allowedModel, got, authID)
	}

	// Sanity: with the gate disabled, the blocked model is served again (proves
	// the gate, not some unrelated registry filter, is what excluded it).
	FreePlanModelGate = nil
	got, errPick = scheduler.pickSingle(context.Background(), "codex", blockedModel, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle(%s) gate-off error = %v, want success", blockedModel, errPick)
	}
	if got == nil || got.ID != authID {
		t.Fatalf("pickSingle(%s) gate-off auth = %v, want %s", blockedModel, got, authID)
	}
}
