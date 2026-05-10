package cliproxy

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestServiceInitializeLoadedAuthsRegistersModelsForLoadedAuths(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	authID := "startup-loaded-auth"
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(authID)
		registry.GetGlobalRegistry().UnregisterClient(authID)
	})

	service := &Service{
		cfg:         &config.Config{},
		coreManager: manager,
	}

	service.initializeLoadedAuths(context.Background())

	if models := registry.GetGlobalRegistry().GetModelsForClient(authID); len(models) == 0 {
		t.Fatal("expected loaded auth to have models registered during startup initialization")
	}
}
