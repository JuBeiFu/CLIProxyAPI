package auth

import "testing"

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
