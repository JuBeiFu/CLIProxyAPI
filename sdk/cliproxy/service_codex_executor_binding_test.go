package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestEnsureExecutorsForAuth_CodexDoesNotReplaceInNormalMode(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "codex-auth-1",
		Provider: "codex",
		Status:   coreauth.StatusActive,
	}

	service.ensureExecutorsForAuth(auth)
	firstExecutor, okFirst := service.coreManager.Executor("codex")
	if !okFirst || firstExecutor == nil {
		t.Fatal("expected codex executor after first bind")
	}

	service.ensureExecutorsForAuth(auth)
	secondExecutor, okSecond := service.coreManager.Executor("codex")
	if !okSecond || secondExecutor == nil {
		t.Fatal("expected codex executor after second bind")
	}

	if firstExecutor != secondExecutor {
		t.Fatal("expected codex executor to stay unchanged in normal mode")
	}
}

func TestEnsureExecutorsForAuthWithMode_CodexForceReplace(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "codex-auth-2",
		Provider: "codex",
		Status:   coreauth.StatusActive,
	}

	service.ensureExecutorsForAuth(auth)
	firstExecutor, okFirst := service.coreManager.Executor("codex")
	if !okFirst || firstExecutor == nil {
		t.Fatal("expected codex executor after first bind")
	}

	service.ensureExecutorsForAuthWithMode(auth, true)
	secondExecutor, okSecond := service.coreManager.Executor("codex")
	if !okSecond || secondExecutor == nil {
		t.Fatal("expected codex executor after forced rebind")
	}

	if firstExecutor == secondExecutor {
		t.Fatal("expected codex executor replacement in force mode")
	}
}

func TestRegisterModelsForAuth_CodexSupportedModelsOverridePlanModels(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "codex-supported-models-auth",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"plan_type":                "pro",
			"supported_models":         "gpt-5.4,gpt-5.5",
			"supported_models_source":  "codex_entitlements",
			"supported_models_updated": "2026-04-24T00:00:00Z",
		},
	}
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(auth)
	models := registry.GetGlobalRegistry().GetModelsForClient(auth.ID)
	ids := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model != nil {
			ids[model.ID] = struct{}{}
		}
	}
	if _, ok := ids["gpt-5.4"]; !ok {
		t.Fatalf("registered models missing gpt-5.4: %+v", ids)
	}
	if _, ok := ids["gpt-5.5"]; !ok {
		t.Fatalf("registered models missing gpt-5.5: %+v", ids)
	}
	if len(ids) != 2 {
		t.Fatalf("registered model count = %d, want 2: %+v", len(ids), ids)
	}
}

func TestRegisterModelsForAuth_CodexEmptySupportedModelsDoesNotFallbackToPlanModels(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "codex-empty-supported-models-auth",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"plan_type":                "pro",
			"supported_models_source":  "codex_entitlements",
			"supported_models_updated": "2026-04-25T00:00:00Z",
		},
	}
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(auth)
	models := registry.GetGlobalRegistry().GetModelsForClient(auth.ID)
	for _, model := range models {
		if model != nil && model.ID == "gpt-5.5" {
			t.Fatalf("registered stale gpt-5.5 model from plan fallback: %+v", models)
		}
	}
	if len(models) != 0 {
		t.Fatalf("registered models = %+v, want none for empty entitlements", models)
	}
}
