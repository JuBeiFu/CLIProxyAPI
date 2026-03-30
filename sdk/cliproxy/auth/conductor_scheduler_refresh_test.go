package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type schedulerProviderTestExecutor struct {
	provider string
}

func (e schedulerProviderTestExecutor) Identifier() string { return e.provider }

func (e schedulerProviderTestExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e schedulerProviderTestExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e schedulerProviderTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e schedulerProviderTestExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e schedulerProviderTestExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestManager_RefreshSchedulerEntry_RebuildsSupportedModelSetAfterModelRegistration(t *testing.T) {
	ctx := context.Background()

	testCases := []struct {
		name  string
		prime func(*Manager, *Auth) error
	}{
		{
			name: "register",
			prime: func(manager *Manager, auth *Auth) error {
				_, errRegister := manager.Register(ctx, auth)
				return errRegister
			},
		},
		{
			name: "update",
			prime: func(manager *Manager, auth *Auth) error {
				_, errRegister := manager.Register(ctx, auth)
				if errRegister != nil {
					return errRegister
				}
				updated := auth.Clone()
				updated.Metadata = map[string]any{"updated": true}
				_, errUpdate := manager.Update(ctx, updated)
				return errUpdate
			},
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			auth := &Auth{
				ID:       "refresh-entry-" + testCase.name,
				Provider: "gemini",
			}
			if errPrime := testCase.prime(manager, auth); errPrime != nil {
				t.Fatalf("prime auth %s: %v", testCase.name, errPrime)
			}

			registerSchedulerModels(t, "gemini", "scheduler-refresh-model", auth.ID)

			got, errPick := manager.scheduler.pickSingle(ctx, "gemini", "scheduler-refresh-model", cliproxyexecutor.Options{}, nil)
			var authErr *Error
			if !errors.As(errPick, &authErr) || authErr == nil {
				t.Fatalf("pickSingle() before refresh error = %v, want auth_not_found", errPick)
			}
			if authErr.Code != "auth_not_found" {
				t.Fatalf("pickSingle() before refresh code = %q, want %q", authErr.Code, "auth_not_found")
			}
			if got != nil {
				t.Fatalf("pickSingle() before refresh auth = %v, want nil", got)
			}

			manager.RefreshSchedulerEntry(auth.ID)

			got, errPick = manager.scheduler.pickSingle(ctx, "gemini", "scheduler-refresh-model", cliproxyexecutor.Options{}, nil)
			if errPick != nil {
				t.Fatalf("pickSingle() after refresh error = %v", errPick)
			}
			if got == nil || got.ID != auth.ID {
				t.Fatalf("pickSingle() after refresh auth = %v, want %q", got, auth.ID)
			}
		})
	}
}

func TestManager_PickNext_RebuildsSchedulerAfterModelCooldownError(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(schedulerProviderTestExecutor{provider: "gemini"})

	registerSchedulerModels(t, "gemini", "scheduler-cooldown-rebuild-model", "cooldown-stale-old")

	oldAuth := &Auth{
		ID:       "cooldown-stale-old",
		Provider: "gemini",
	}
	if _, errRegister := manager.Register(ctx, oldAuth); errRegister != nil {
		t.Fatalf("register old auth: %v", errRegister)
	}

	manager.MarkResult(ctx, Result{
		AuthID:   oldAuth.ID,
		Provider: "gemini",
		Model:    "scheduler-cooldown-rebuild-model",
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
	})

	newAuth := &Auth{
		ID:       "cooldown-stale-new",
		Provider: "gemini",
	}
	if _, errRegister := manager.Register(ctx, newAuth); errRegister != nil {
		t.Fatalf("register new auth: %v", errRegister)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(newAuth.ID, "gemini", []*registry.ModelInfo{{ID: "scheduler-cooldown-rebuild-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient(newAuth.ID)
	})

	got, errPick := manager.scheduler.pickSingle(ctx, "gemini", "scheduler-cooldown-rebuild-model", cliproxyexecutor.Options{}, nil)
	var cooldownErr *modelCooldownError
	if !errors.As(errPick, &cooldownErr) {
		t.Fatalf("pickSingle() before sync error = %v, want modelCooldownError", errPick)
	}
	if got != nil {
		t.Fatalf("pickSingle() before sync auth = %v, want nil", got)
	}

	got, executor, errPick := manager.pickNext(ctx, "gemini", "scheduler-cooldown-rebuild-model", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if executor == nil {
		t.Fatal("pickNext() executor = nil")
	}
	if got == nil || got.ID != newAuth.ID {
		t.Fatalf("pickNext() auth = %v, want %q", got, newAuth.ID)
	}
}

func TestManager_RegisterUpdate_PreservesCreatedAtAndAdvancesUpdatedAt(t *testing.T) {
	ctx := WithSkipPersist(context.Background())
	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	createdAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	auth := &Auth{
		ID:        "timestamps-auth",
		Provider:  "gemini",
		CreatedAt: createdAt,
	}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	stored, ok := manager.GetByID(auth.ID)
	if !ok || stored == nil {
		t.Fatal("GetByID() after register returned no auth")
	}
	if !stored.CreatedAt.Equal(createdAt) {
		t.Fatalf("CreatedAt after register = %v, want %v", stored.CreatedAt, createdAt)
	}
	if !stored.UpdatedAt.Equal(createdAt) {
		t.Fatalf("UpdatedAt after register = %v, want %v", stored.UpdatedAt, createdAt)
	}

	time.Sleep(10 * time.Millisecond)
	updated := stored.Clone()
	updated.Metadata = map[string]any{"updated": true}
	if _, errUpdate := manager.Update(ctx, updated); errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}

	stored, ok = manager.GetByID(auth.ID)
	if !ok || stored == nil {
		t.Fatal("GetByID() after update returned no auth")
	}
	if !stored.CreatedAt.Equal(createdAt) {
		t.Fatalf("CreatedAt after update = %v, want %v", stored.CreatedAt, createdAt)
	}
	if !stored.UpdatedAt.After(createdAt) {
		t.Fatalf("UpdatedAt after update = %v, want after %v", stored.UpdatedAt, createdAt)
	}
}

func TestManager_MarkResult_SuccessRecordsLastUsedAt(t *testing.T) {
	ctx := WithSkipPersist(context.Background())
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := &Auth{
		ID:       "last-used-auth",
		Provider: "gemini",
	}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gemini-2.5-pro",
		Success:  true,
	})

	stored, ok := manager.GetByID(auth.ID)
	if !ok || stored == nil {
		t.Fatal("GetByID() returned no auth")
	}
	lastUsedAt, ok := authLastUsedAt(stored)
	if !ok || lastUsedAt.IsZero() {
		t.Fatalf("authLastUsedAt() = %v, %v, want non-zero timestamp", lastUsedAt, ok)
	}
}

func TestManager_MarkResult_SlowRequestPenaltyDemotesAuth(t *testing.T) {
	ctx := WithSkipPersist(context.Background())
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			SlowRequestPenaltyEnabled: commonBoolPtr(true),
			SlowRequestThresholdMs:    20000,
			SlowRequestWindowSeconds:  300,
			SlowRequestTriggerCount:   2,
			SlowRequestPenaltyStep:    3,
			SlowRequestPenaltyMax:     6,
			SlowRequestPenaltyRecover: 1,
			SlowRequestCooldownSeconds: 0,
		},
	})

	if _, err := manager.Register(ctx, &Auth{ID: "slow-auth", Provider: "codex", Attributes: map[string]string{"priority": "10", "plan_type": "plus"}}); err != nil {
		t.Fatalf("Register(slow-auth) error = %v", err)
	}
	if _, err := manager.Register(ctx, &Auth{ID: "healthy-auth", Provider: "codex", Attributes: map[string]string{"priority": "10", "plan_type": "plus"}}); err != nil {
		t.Fatalf("Register(healthy-auth) error = %v", err)
	}
	registerSchedulerModels(t, "codex", "gpt-5.4", "slow-auth", "healthy-auth")
	manager.RefreshSchedulerEntry("slow-auth")
	manager.RefreshSchedulerEntry("healthy-auth")

	manager.MarkResult(context.Background(), Result{
		AuthID:   "slow-auth",
		Provider: "codex",
		Model:    "gpt-5.4",
		Success:  false,
		Latency:  21 * time.Second,
		Error:    &Error{HTTPStatus: http.StatusBadGateway, Message: "slow upstream"},
	})
	manager.MarkResult(context.Background(), Result{
		AuthID:   "slow-auth",
		Provider: "codex",
		Model:    "gpt-5.4",
		Success:  false,
		Latency:  22 * time.Second,
		Error:    &Error{HTTPStatus: http.StatusBadGateway, Message: "slow upstream"},
	})

	stored, ok := manager.GetByID("slow-auth")
	if !ok || stored == nil {
		t.Fatal("GetByID(slow-auth) returned no auth")
	}
	if stored.SlowRequestPriorityPenalty != 3 {
		t.Fatalf("SlowRequestPriorityPenalty = %d, want %d", stored.SlowRequestPriorityPenalty, 3)
	}

	got, errPick := manager.scheduler.pickSingle(context.Background(), "codex", "gpt-5.4", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	if got == nil || got.ID != "healthy-auth" {
		t.Fatalf("pickSingle() auth = %v, want healthy-auth", got)
	}
}

func TestManager_MarkResult_SlowRequestPenaltyCooldownBlocksAuth(t *testing.T) {
	ctx := WithSkipPersist(context.Background())
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			SlowRequestPenaltyEnabled: commonBoolPtr(true),
			SlowRequestThresholdMs:    20000,
			SlowRequestTriggerCount:   1,
			SlowRequestCooldownSeconds: 120,
		},
	})

	if _, err := manager.Register(ctx, &Auth{ID: "slow-auth", Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}}); err != nil {
		t.Fatalf("Register(slow-auth) error = %v", err)
	}
	if _, err := manager.Register(ctx, &Auth{ID: "healthy-auth", Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}}); err != nil {
		t.Fatalf("Register(healthy-auth) error = %v", err)
	}
	registerSchedulerModels(t, "codex", "gpt-5.4", "slow-auth", "healthy-auth")
	manager.RefreshSchedulerEntry("slow-auth")
	manager.RefreshSchedulerEntry("healthy-auth")

	manager.MarkResult(context.Background(), Result{
		AuthID:   "slow-auth",
		Provider: "codex",
		Model:    "gpt-5.4",
		Success:  false,
		Latency:  25 * time.Second,
		Error:    &Error{HTTPStatus: http.StatusBadGateway, Message: "slow upstream"},
	})

	stored, ok := manager.GetByID("slow-auth")
	if !ok || stored == nil {
		t.Fatal("GetByID(slow-auth) returned no auth")
	}
	if stored.SlowRequestCooldownUntil.IsZero() || !stored.SlowRequestCooldownUntil.After(time.Now().Add(100*time.Second)) {
		t.Fatalf("SlowRequestCooldownUntil = %v, want active cooldown", stored.SlowRequestCooldownUntil)
	}

	got, errPick := manager.scheduler.pickSingle(context.Background(), "codex", "gpt-5.4", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	if got == nil || got.ID != "healthy-auth" {
		t.Fatalf("pickSingle() auth = %v, want healthy-auth", got)
	}
}

func TestManager_MarkResult_SlowRequestPenaltyRecoversOnSuccess(t *testing.T) {
	auth := &Auth{SlowRequestPriorityPenalty: 3, SlowRequestCooldownUntil: time.Now().Add(5 * time.Minute)}
	cfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			SlowRequestPenaltyEnabled: commonBoolPtr(true),
			SlowRequestThresholdMs:    20000,
			SlowRequestPenaltyRecover: 2,
		},
	}
	applySlowRequestPenaltyResult(auth, Result{Success: true, Latency: time.Second}, time.Now(), cfg)
	if auth.SlowRequestPriorityPenalty != 1 {
		t.Fatalf("SlowRequestPriorityPenalty = %d, want %d", auth.SlowRequestPriorityPenalty, 1)
	}
	if !auth.SlowRequestCooldownUntil.IsZero() && auth.SlowRequestCooldownUntil.After(time.Now().Add(time.Second)) {
		t.Fatalf("SlowRequestCooldownUntil = %v, want cleared or immediate", auth.SlowRequestCooldownUntil)
	}
}

func commonBoolPtr(v bool) *bool {
	return &v
}
