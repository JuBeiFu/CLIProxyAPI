package auth

import (
	"context"
	"testing"
)

func TestManager_RegisterPlainAuthPreservesIncrementalCaches(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	mgr.routeAwareProviders["codex"] = false
	mgr.apiKeyModelAlias.Store(apiKeyModelAliasTable{
		"sentinel": {"alias": "upstream-model"},
	})

	if _, err := mgr.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "plain-register",
		Provider: "codex",
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.routeAwareMu.RLock()
	_, routeAwareCached := mgr.routeAwareProviders["codex"]
	mgr.routeAwareMu.RUnlock()
	if !routeAwareCached {
		t.Fatal("expected plain auth register to preserve cached non-route-aware provider result")
	}

	table, _ := mgr.apiKeyModelAlias.Load().(apiKeyModelAliasTable)
	if _, ok := table["sentinel"]; !ok {
		t.Fatal("expected plain auth register to avoid rebuilding api-key alias cache")
	}
}

func TestManager_RegisterRouteAwareAuthInvalidatesProviderCache(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	mgr.routeAwareProviders["gemini"] = false

	if _, err := mgr.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "route-aware-register",
		Provider: "gemini",
		Prefix:   "tenant-a",
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.routeAwareMu.RLock()
	_, cached := mgr.routeAwareProviders["gemini"]
	mgr.routeAwareMu.RUnlock()
	if cached {
		t.Fatal("expected route-aware auth register to invalidate cached provider result")
	}
}

func TestManager_UpdatePlainAuthPreservesIncrementalCaches(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	if _, err := mgr.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "plain-update",
		Provider: "codex",
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	mgr.routeAwareProviders["codex"] = false
	mgr.apiKeyModelAlias.Store(apiKeyModelAliasTable{
		"sentinel": {"alias": "upstream-model"},
	})

	if _, err := mgr.Update(WithSkipPersist(context.Background()), &Auth{
		ID:       "plain-update",
		Provider: "codex",
		Disabled: true,
		Status:   StatusDisabled,
	}); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}

	mgr.routeAwareMu.RLock()
	_, routeAwareCached := mgr.routeAwareProviders["codex"]
	mgr.routeAwareMu.RUnlock()
	if !routeAwareCached {
		t.Fatal("expected plain auth update to preserve cached non-route-aware provider result")
	}

	table, _ := mgr.apiKeyModelAlias.Load().(apiKeyModelAliasTable)
	if _, ok := table["sentinel"]; !ok {
		t.Fatal("expected plain auth update to avoid rebuilding api-key alias cache")
	}
}

func TestManager_UpdateRouteAwareAuthInvalidatesProviderCache(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	if _, err := mgr.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "route-aware-update",
		Provider: "gemini",
		Prefix:   "tenant-b",
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	mgr.routeAwareProviders["gemini"] = true

	if _, err := mgr.Update(WithSkipPersist(context.Background()), &Auth{
		ID:       "route-aware-update",
		Provider: "gemini",
		Prefix:   "tenant-b",
		Disabled: true,
		Status:   StatusDisabled,
	}); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}

	mgr.routeAwareMu.RLock()
	_, cached := mgr.routeAwareProviders["gemini"]
	mgr.routeAwareMu.RUnlock()
	if cached {
		t.Fatal("expected route-aware auth update to invalidate cached provider result")
	}
}

func TestManager_RemovePlainAuthPreservesIncrementalCaches(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	if _, err := mgr.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "plain-remove",
		Provider: "codex",
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	mgr.routeAwareProviders["codex"] = false
	mgr.apiKeyModelAlias.Store(apiKeyModelAliasTable{
		"sentinel": {"alias": "upstream-model"},
	})

	if _, err := mgr.Remove(context.Background(), "plain-remove"); err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}

	mgr.routeAwareMu.RLock()
	_, routeAwareCached := mgr.routeAwareProviders["codex"]
	mgr.routeAwareMu.RUnlock()
	if !routeAwareCached {
		t.Fatal("expected plain auth remove to preserve cached non-route-aware provider result")
	}

	table, _ := mgr.apiKeyModelAlias.Load().(apiKeyModelAliasTable)
	if _, ok := table["sentinel"]; !ok {
		t.Fatal("expected plain auth remove to avoid rebuilding api-key alias cache")
	}
}
