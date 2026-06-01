package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

func mi(id string) *ModelInfo { return &ModelInfo{ID: id, Object: "model", OwnedBy: "openai", Type: "openai"} }

func ids(models []*registry.ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		if m != nil {
			out = append(out, m.ID)
		}
	}
	return out
}

func TestIsFreeAllowedModelID(t *testing.T) {
	allow := []string{"gpt-5.5"}
	cases := map[string]bool{
		"gpt-5.5":             true,
		"gpt-5.5(high)":       true,
		"GPT-5.5":             true,
		"gpt-5.5-mini":        false,
		"gpt-5.5-codex":       false,
		"gpt-5.5-codex-spark": false,
		"gpt-5.4":             false,
		"gpt-image-2":         false,
	}
	for id, want := range cases {
		if got := isFreeAllowedModelID(id, allow); got != want {
			t.Errorf("isFreeAllowedModelID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestFilterCodexModelsForFreePlan(t *testing.T) {
	allow := []string{"gpt-5.5"}
	models := []*registry.ModelInfo{mi("gpt-5.5"), mi("gpt-5.5-codex"), mi("gpt-5.4"), mi("gpt-image-2")}

	// free + gate on => only gpt-5.5
	got := filterCodexModelsForFreePlan(models, "free", true, allow)
	if g := ids(got); len(g) != 1 || g[0] != "gpt-5.5" {
		t.Fatalf("free gate => %v, want [gpt-5.5]", ids(got))
	}
	// plus => unchanged
	if g := ids(filterCodexModelsForFreePlan(models, "plus", true, allow)); len(g) != 4 {
		t.Fatalf("plus => %v, want all 4", g)
	}
	// empty/unknown plan => unchanged (not-yet-probed)
	if g := ids(filterCodexModelsForFreePlan(models, "", true, allow)); len(g) != 4 {
		t.Fatalf("empty plan => %v, want all 4", g)
	}
	// gate disabled => unchanged even for free
	if g := ids(filterCodexModelsForFreePlan(models, "free", false, allow)); len(g) != 4 {
		t.Fatalf("gate off => %v, want all 4", g)
	}
}

func TestFreePlanModelGateHookWiring(t *testing.T) {
	allow := []string{"gpt-5.5"}
	// emulate the closure body
	check := func(model string, enabled bool) bool {
		if !enabled {
			return true
		}
		return isFreeAllowedModelIDForAuthPkg(model, allow)
	}
	if !check("gpt-5.5(high)", true) {
		t.Fatal("gpt-5.5 variant must be allowed")
	}
	if check("gpt-5.4", true) {
		t.Fatal("gpt-5.4 must be disallowed for free")
	}
	if !check("gpt-5.4", false) {
		t.Fatal("disabled gate must allow everything")
	}
}
