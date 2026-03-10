package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type quotaProbeTestExecutor struct {
	id       string
	attempts int
	execute  func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
}

func (e *quotaProbeTestExecutor) Identifier() string { return e.id }

func (e *quotaProbeTestExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.attempts++
	if e.execute != nil {
		return e.execute(ctx, auth, req, opts)
	}
	return cliproxyexecutor.Response{}, nil
}

func (e *quotaProbeTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}
func (e *quotaProbeTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *quotaProbeTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *quotaProbeTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

type quotaProbeStatusError struct {
	status     int
	message    string
	retryAfter *time.Duration
}

func (e quotaProbeStatusError) Error() string { return e.message }

func (e quotaProbeStatusError) StatusCode() int { return e.status }

func (e quotaProbeStatusError) RetryAfter() *time.Duration { return e.retryAfter }

func TestPickQuotaProbeModel_PrefersNonRestrictedCodexModelForFreePlan(t *testing.T) {
	auth := &Auth{ID: "quota-model-free", Provider: "codex", Metadata: map[string]any{"type": "codex", "plan_type": "free"}}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}, {ID: "gpt-5"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	if got := pickQuotaProbeModel(auth); got != "gpt-5" {
		t.Fatalf("pickQuotaProbeModel() = %q, want %q", got, "gpt-5")
	}
}

func TestManager_QuotaRefresh_DeletesPersistedAuthOn401(t *testing.T) {
	store := &deletingStore{}
	exec := &quotaProbeTestExecutor{
		id: "codex",
		execute: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			return cliproxyexecutor.Response{}, quotaProbeStatusError{status: http.StatusUnauthorized, message: `{"error":{"message":"Your OpenAI account has been deactivated, please check your email for more information.","code":"account_deactivated"},"status":401}`}
		},
	}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(exec)
	auth := &Auth{ID: "auths/quota-delete.json", FileName: "auths/quota-delete.json", Provider: "codex", Attributes: map[string]string{"path": "/tmp/quota-delete.json"}, Metadata: map[string]any{"type": "codex"}}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	mgr.refreshQuotaAuth(context.Background(), auth.ID)

	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatalf("expected 401 quota probe auth to be removed")
	}
	deleted := store.Deleted()
	if len(deleted) != 1 || deleted[0] != auth.ID {
		t.Fatalf("expected deleted ids [%q], got %v", auth.ID, deleted)
	}
	if exec.attempts != 1 {
		t.Fatalf("attempts = %d, want 1", exec.attempts)
	}
}

func TestManager_QuotaRefresh_DisablesQuotaLimitedAuthAndSetsCooldown(t *testing.T) {
	store := &deletingStore{}
	retryAfter := 90 * time.Second
	exec := &quotaProbeTestExecutor{
		id: "codex",
		execute: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			return cliproxyexecutor.Response{}, quotaProbeStatusError{status: http.StatusTooManyRequests, message: `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":90},"status":429}`, retryAfter: &retryAfter}
		},
	}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(exec)
	auth := &Auth{ID: "auths/quota-disable.json", FileName: "auths/quota-disable.json", Provider: "codex", Attributes: map[string]string{"path": "/tmp/quota-disable.json"}, Metadata: map[string]any{"type": "codex"}}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	before := time.Now()
	mgr.refreshQuotaAuth(context.Background(), auth.ID)

	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected quota limited auth to remain registered")
	}
	if !stored.Disabled || stored.Status != StatusDisabled {
		t.Fatalf("expected auth to be auto-disabled, got disabled=%v status=%q", stored.Disabled, stored.Status)
	}
	if !stored.Quota.Exceeded || stored.Quota.NextRecoverAt.IsZero() {
		t.Fatalf("expected quota cooldown to be recorded, got %+v", stored.Quota)
	}
	if !quotaProbeAutoDisabled(stored) {
		t.Fatalf("expected auth to be marked as auto-disabled by quota probe")
	}
	if stored.LastError == nil || stored.LastError.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("expected 429 last_error, got %+v", stored.LastError)
	}
	if stored.Quota.NextRecoverAt.Before(before.Add(80 * time.Second)) {
		t.Fatalf("quota next recover at = %v, want at least %v", stored.Quota.NextRecoverAt, before.Add(80*time.Second))
	}
	nextProbe, lastProbe := quotaProbeSchedule(stored)
	if lastProbe.IsZero() || nextProbe.IsZero() {
		t.Fatalf("expected quota probe cooldown schedule, got last=%v next=%v", lastProbe, nextProbe)
	}
	if nextProbe.Sub(lastProbe) < 14*time.Minute {
		t.Fatalf("expected quota probe cooldown near %s, got %s", quotaProbeCooldown, nextProbe.Sub(lastProbe))
	}

	if exec.attempts != 1 {
		t.Fatalf("attempts = %d, want 1", exec.attempts)
	}
}

func TestManager_QuotaRefresh_ReenablesAutoDisabledAuthOnSuccess(t *testing.T) {
	store := &deletingStore{}
	exec := &quotaProbeTestExecutor{id: "codex"}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(exec)
	now := time.Now()
	auth := &Auth{
		ID:       "auths/quota-recover.json",
		FileName: "auths/quota-recover.json",
		Provider: "codex",
		Disabled: true,
		Status:   StatusDisabled,
		Metadata: map[string]any{"type": "codex", metadataAutoDisabledReasonKey: autoDisabledReasonQuotaExhausted},
		Quota:    QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(10 * time.Minute)},
		ModelStates: map[string]*ModelState{
			"gpt-5": {Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(10 * time.Minute), Quota: QuotaState{Exceeded: true, NextRecoverAt: now.Add(10 * time.Minute)}},
		},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	mgr.refreshQuotaAuth(context.Background(), auth.ID)
	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected recovered auth to remain registered")
	}
	if stored.Disabled || stored.Status != StatusActive {
		t.Fatalf("expected auth to be re-enabled, got disabled=%v status=%q", stored.Disabled, stored.Status)
	}
	if stored.Quota.Exceeded || stored.LastError != nil {
		t.Fatalf("expected quota and last_error to be cleared, got quota=%+v last_error=%+v", stored.Quota, stored.LastError)
	}
	if quotaProbeAutoDisabled(stored) {
		t.Fatalf("expected auto-disabled marker to be cleared")
	}
	state := stored.ModelStates["gpt-5"]
	if state == nil || state.Unavailable || state.Quota.Exceeded {
		t.Fatalf("expected model state to be reset, got %+v", state)
	}
}

func TestManager_QuotaRefresh_FailureStillSetsCooldown(t *testing.T) {
	store := &deletingStore{}
	attempts := 0
	exec := &quotaProbeTestExecutor{
		id: "codex",
		execute: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			attempts++
			return cliproxyexecutor.Response{}, errors.New("dial tcp timeout")
		},
	}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(exec)
	auth := &Auth{ID: "auths/quota-failure.json", FileName: "auths/quota-failure.json", Provider: "codex", Attributes: map[string]string{"path": "/tmp/quota-failure.json"}, Metadata: map[string]any{"type": "codex"}}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	mgr.refreshQuotaAuth(context.Background(), auth.ID)
	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth to remain registered after quota probe failure")
	}
	if attempts != 3 {
		t.Fatalf("attempt count = %d, want 3", attempts)
	}
	if stored.LastError == nil || stored.LastError.Message != "dial tcp timeout" {
		t.Fatalf("expected last_error to be recorded, got %+v", stored.LastError)
	}
	nextProbe, lastProbe := quotaProbeSchedule(stored)
	if lastProbe.IsZero() || nextProbe.IsZero() {
		t.Fatalf("expected failure cooldown to be scheduled, got last=%v next=%v", lastProbe, nextProbe)
	}
	if nextProbe.Sub(lastProbe) < 14*time.Minute {
		t.Fatalf("expected failure cooldown near %s, got %s", quotaProbeCooldown, nextProbe.Sub(lastProbe))
	}
}

func TestManager_QuotaRefresh_RetriesTransientFailuresTwice(t *testing.T) {
	store := &deletingStore{}
	attempts := 0
	exec := &quotaProbeTestExecutor{
		id: "codex",
		execute: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			attempts++
			if attempts < 3 {
				return cliproxyexecutor.Response{}, errors.New("dial tcp timeout")
			}
			return cliproxyexecutor.Response{}, nil
		},
	}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(exec)
	auth := &Auth{ID: "auths/quota-retry.json", FileName: "auths/quota-retry.json", Provider: "codex", Attributes: map[string]string{"path": "/tmp/quota-retry.json"}, Metadata: map[string]any{"type": "codex"}}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	mgr.refreshQuotaAuth(context.Background(), auth.ID)
	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth to remain registered after retry success")
	}
	if stored.Status != StatusActive || stored.Disabled {
		t.Fatalf("expected auth to be active after retry success, got disabled=%v status=%q", stored.Disabled, stored.Status)
	}
	if attempts != 3 {
		t.Fatalf("attempt count = %d, want 3", attempts)
	}
	nextProbe, _ := quotaProbeSchedule(stored)
	if nextProbe.IsZero() {
		t.Fatalf("expected next quota probe cooldown to be scheduled")
	}
}
