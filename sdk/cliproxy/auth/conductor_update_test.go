package auth

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestManager_Update_PreservesModelStates(t *testing.T) {
	m := NewManager(nil, nil, nil)

	model := "test-model"
	backoffLevel := 7

	if _, errRegister := m.Register(context.Background(), &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{"k": "v"},
		ModelStates: map[string]*ModelState{
			model: {
				Quota: QuotaState{BackoffLevel: backoffLevel},
			},
		},
	}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	if _, errUpdate := m.Update(context.Background(), &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{"k": "v2"},
	}); errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}

	updated, ok := m.GetByID("auth-1")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if len(updated.ModelStates) == 0 {
		t.Fatalf("expected ModelStates to be preserved")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if state.Quota.BackoffLevel != backoffLevel {
		t.Fatalf("expected BackoffLevel to be %d, got %d", backoffLevel, state.Quota.BackoffLevel)
	}
}

func TestManager_Update_DisabledExistingDoesNotInheritModelStates(t *testing.T) {
	m := NewManager(nil, nil, nil)

	// Register a disabled auth with existing ModelStates.
	if _, err := m.Register(context.Background(), &Auth{
		ID:       "auth-disabled",
		Provider: "claude",
		Disabled: true,
		Status:   StatusDisabled,
		ModelStates: map[string]*ModelState{
			"stale-model": {
				Quota: QuotaState{BackoffLevel: 5},
			},
		},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	// Update with empty ModelStates — should NOT inherit stale states.
	if _, err := m.Update(context.Background(), &Auth{
		ID:       "auth-disabled",
		Provider: "claude",
		Disabled: true,
		Status:   StatusDisabled,
	}); err != nil {
		t.Fatalf("update auth: %v", err)
	}

	updated, ok := m.GetByID("auth-disabled")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if len(updated.ModelStates) != 0 {
		t.Fatalf("expected disabled auth NOT to inherit ModelStates, got %d entries", len(updated.ModelStates))
	}
}

func TestManager_Update_ActiveToDisabledDoesNotInheritModelStates(t *testing.T) {
	m := NewManager(nil, nil, nil)

	// Register an active auth with ModelStates (simulates existing live auth).
	if _, err := m.Register(context.Background(), &Auth{
		ID:       "auth-a2d",
		Provider: "claude",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			"stale-model": {
				Quota: QuotaState{BackoffLevel: 9},
			},
		},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	// File watcher deletes config → synthesizes Disabled=true auth → Update.
	// Even though existing is active, incoming auth is disabled → skip inheritance.
	if _, err := m.Update(context.Background(), &Auth{
		ID:       "auth-a2d",
		Provider: "claude",
		Disabled: true,
		Status:   StatusDisabled,
	}); err != nil {
		t.Fatalf("update auth: %v", err)
	}

	updated, ok := m.GetByID("auth-a2d")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if len(updated.ModelStates) != 0 {
		t.Fatalf("expected active→disabled transition NOT to inherit ModelStates, got %d entries", len(updated.ModelStates))
	}
}

func TestManager_Update_DisabledToActiveDoesNotInheritStaleModelStates(t *testing.T) {
	m := NewManager(nil, nil, nil)

	// Register a disabled auth with stale ModelStates.
	if _, err := m.Register(context.Background(), &Auth{
		ID:       "auth-d2a",
		Provider: "claude",
		Disabled: true,
		Status:   StatusDisabled,
		ModelStates: map[string]*ModelState{
			"stale-model": {
				Quota: QuotaState{BackoffLevel: 4},
			},
		},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	// Re-enable: incoming auth is active, existing is disabled → skip inheritance.
	if _, err := m.Update(context.Background(), &Auth{
		ID:       "auth-d2a",
		Provider: "claude",
		Status:   StatusActive,
	}); err != nil {
		t.Fatalf("update auth: %v", err)
	}

	updated, ok := m.GetByID("auth-d2a")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if len(updated.ModelStates) != 0 {
		t.Fatalf("expected disabled→active transition NOT to inherit stale ModelStates, got %d entries", len(updated.ModelStates))
	}
}

func TestManager_Update_ActiveInheritsModelStates(t *testing.T) {
	m := NewManager(nil, nil, nil)

	model := "active-model"
	backoffLevel := 3

	// Register an active auth with ModelStates.
	if _, err := m.Register(context.Background(), &Auth{
		ID:       "auth-active",
		Provider: "claude",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			model: {
				Quota: QuotaState{BackoffLevel: backoffLevel},
			},
		},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	// Update with empty ModelStates — both sides active → SHOULD inherit.
	if _, err := m.Update(context.Background(), &Auth{
		ID:       "auth-active",
		Provider: "claude",
		Status:   StatusActive,
	}); err != nil {
		t.Fatalf("update auth: %v", err)
	}

	updated, ok := m.GetByID("auth-active")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if len(updated.ModelStates) == 0 {
		t.Fatalf("expected active auth to inherit ModelStates")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if state.Quota.BackoffLevel != backoffLevel {
		t.Fatalf("expected BackoffLevel to be %d, got %d", backoffLevel, state.Quota.BackoffLevel)
	}
}

func TestManager_Update_PreservesNewerRuntimeAvailabilityState(t *testing.T) {
	m := NewManager(nil, nil, nil)

	model := "newer-runtime-state-model"
	if _, err := m.Register(context.Background(), &Auth{
		ID:        "auth-newer-runtime",
		Provider:  "codex",
		Status:    StatusActive,
		UpdatedAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	m.MarkResult(context.Background(), Result{
		AuthID:   "auth-newer-runtime",
		Provider: "codex",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
	})

	staleRefresh := &Auth{
		ID:              "auth-newer-runtime",
		Provider:        "codex",
		Status:          StatusActive,
		LastRefreshedAt: time.Now(),
		UpdatedAt:       time.Now().Add(-time.Hour),
	}
	if _, err := m.Update(context.Background(), staleRefresh); err != nil {
		t.Fatalf("update auth: %v", err)
	}

	updated, ok := m.GetByID("auth-newer-runtime")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected newer model state to be preserved")
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatalf("expected newer model cooldown to be preserved")
	}
	if updated.NextRetryAfter.IsZero() {
		t.Fatalf("expected newer auth cooldown to be preserved")
	}
}

func TestManager_Update_PreservesRuntimeUsageLimitWhenRefreshHasNoQuotaProof(t *testing.T) {
	m := NewManager(nil, nil, nil)

	resetAt := time.Now().Add(time.Hour)
	if _, err := m.Register(context.Background(), &Auth{
		ID:       "auth-usage-limit-runtime",
		Provider: "codex",
		Status:   StatusActive,
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	limited, ok := m.GetByID("auth-usage-limit-runtime")
	if !ok || limited == nil {
		t.Fatalf("expected auth to be present")
	}
	limited.Status = StatusError
	limited.StatusMessage = "quota exhausted"
	limited.Unavailable = true
	limited.NextRetryAfter = resetAt
	limited.Quota = QuotaState{
		Exceeded:      true,
		Reason:        "usage_limit",
		NextRecoverAt: resetAt,
		UpdatedAt:     time.Now(),
	}
	limited.UpdatedAt = time.Now()
	if _, err := m.Update(context.Background(), limited); err != nil {
		t.Fatalf("update limited auth: %v", err)
	}

	refreshWithoutQuota := &Auth{
		ID:              "auth-usage-limit-runtime",
		Provider:        "codex",
		Status:          StatusActive,
		LastRefreshedAt: time.Now(),
		UpdatedAt:       time.Now().Add(time.Second),
		Metadata:        map[string]any{"type": "codex"},
	}
	if _, err := m.Update(context.Background(), refreshWithoutQuota); err != nil {
		t.Fatalf("update refreshed auth: %v", err)
	}

	updated, ok := m.GetByID("auth-usage-limit-runtime")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if !updated.Unavailable || !updated.Quota.Exceeded || updated.Quota.Reason != "usage_limit" {
		t.Fatalf("expected runtime usage_limit cooldown to be preserved, unavailable=%v quota=%+v", updated.Unavailable, updated.Quota)
	}
}
