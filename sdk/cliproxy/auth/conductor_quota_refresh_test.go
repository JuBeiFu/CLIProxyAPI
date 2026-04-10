package auth

import (
	"bytes"
	"context"
	"io"
	"errors"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type quotaProbeTestExecutor struct {
	id       string
	attempts int
	execute  func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	httpRequest func(context.Context, *Auth, *http.Request) (*http.Response, error)
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

func (e *quotaProbeTestExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	if e.httpRequest != nil {
		return e.httpRequest(ctx, auth, req)
	}
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

func newCodexUsageHTTPResponse(status int, body string, headers map[string]string) *http.Response {
	resp := &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
	for key, value := range headers {
		resp.Header.Set(key, value)
	}
	return resp
}

func metadataTimeValue(t *testing.T, auth *Auth, key string) time.Time {
	t.Helper()
	if auth == nil || auth.Metadata == nil {
		t.Fatalf("metadataTimeValue(%s): auth metadata is nil", key)
	}
	value, ok := lookupMetadataTime(auth.Metadata, key)
	if !ok {
		t.Fatalf("metadataTimeValue(%s): key not found", key)
	}
	return value
}

func TestPickQuotaProbeModel_UsesRegisteredCodexModelForFreePlan(t *testing.T) {
	auth := &Auth{ID: "quota-model-free", Provider: "codex", Metadata: map[string]any{"type": "codex", "plan_type": "free"}}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}, {ID: "gpt-5"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	if got := pickQuotaProbeModel(auth); got != "gpt-5.4" {
		t.Fatalf("pickQuotaProbeModel() = %q, want %q", got, "gpt-5.4")
	}
}

func TestManager_QuotaRefresh_SkipsUnsupportedProbeWithoutMarkingFailure(t *testing.T) {
	store := &deletingStore{}
	exec := &quotaProbeTestExecutor{id: "custom"}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(exec)
	auth := &Auth{
		ID:       "auths/quota-skip.json",
		FileName: "auths/quota-skip.json",
		Provider: "custom",
		Status:   StatusActive,
		Attributes: map[string]string{
			"path": "/tmp/quota-skip.json",
		},
		Metadata: map[string]any{"type": "custom"},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.refreshQuotaAuth(context.Background(), auth.ID)
	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth to remain registered after quota probe skip")
	}
	if stored.LastError != nil {
		t.Fatalf("expected no last_error after quota probe skip, got %+v", stored.LastError)
	}
	if stored.Status != StatusActive {
		t.Fatalf("expected auth status to remain active, got %q", stored.Status)
	}
	if stored.Disabled {
		t.Fatalf("expected auth to remain enabled")
	}
	if exec.attempts != 0 {
		t.Fatalf("attempts = %d, want 0", exec.attempts)
	}
	nextProbe, lastProbe := quotaProbeSchedule(stored)
	if lastProbe.IsZero() || nextProbe.IsZero() {
		t.Fatalf("expected skip cooldown to be scheduled, got last=%v next=%v", lastProbe, nextProbe)
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
	waitForDeletedIDs(t, store, []string{auth.ID})
	if exec.attempts != 1 {
		t.Fatalf("attempts = %d, want 1", exec.attempts)
	}
}

func TestManager_QuotaRefresh_DeletesPersistedAuthOnDeactivatedWorkspace(t *testing.T) {
	store := &deletingStore{}
	exec := &quotaProbeTestExecutor{
		id: "codex",
		execute: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			return cliproxyexecutor.Response{}, quotaProbeStatusError{status: http.StatusForbidden, message: `{"detail":{"code":"deactivated_workspace"}}`}
		},
	}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(exec)
	auth := &Auth{ID: "auths/quota-deactivated-workspace.json", FileName: "auths/quota-deactivated-workspace.json", Provider: "codex", Attributes: map[string]string{"path": "/tmp/quota-deactivated-workspace.json"}, Metadata: map[string]any{"type": "codex"}}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	mgr.refreshQuotaAuth(context.Background(), auth.ID)

	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatalf("expected deactivated_workspace quota probe auth to be removed")
	}
	waitForDeletedIDs(t, store, []string{auth.ID})
	if exec.attempts != 1 {
		t.Fatalf("attempts = %d, want 1", exec.attempts)
	}
}

func TestManager_QuotaRefresh_DeletesPersistedAuthOnOrgRequired(t *testing.T) {
	store := &deletingStore{}
	exec := &quotaProbeTestExecutor{
		id: "codex",
		execute: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			return cliproxyexecutor.Response{}, quotaProbeStatusError{status: http.StatusUnauthorized, message: `{"error":{"message":"You must be a member of an organization to use the API."},"status":401}`}
		},
	}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(exec)
	auth := &Auth{ID: "auths/quota-org-required.json", FileName: "auths/quota-org-required.json", Provider: "codex", Attributes: map[string]string{"path": "/tmp/quota-org-required.json"}, Metadata: map[string]any{"type": "codex"}}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	mgr.refreshQuotaAuth(context.Background(), auth.ID)

	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatalf("expected org-required quota probe auth to be removed")
	}
	waitForDeletedIDs(t, store, []string{auth.ID})
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
	if stored.Quota.NextRecoverAt.Sub(nextProbe) > time.Second {
		t.Fatalf("expected quota probe to wait until quota recovery, got next=%v recover=%v", nextProbe, stored.Quota.NextRecoverAt)
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

func TestManager_MarkResult_CodexSuccessSchedulesUsageRefreshAfterFiveMinutes(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auths/codex-usage-schedule.json",
		FileName: "auths/codex-usage-schedule.json",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "codex"},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	before := time.Now()
	mgr.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gpt-5",
		Success:  true,
	})

	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth to remain registered")
	}
	nextRefresh := metadataTimeValue(t, stored, metadataCodexUsageAfterKey)
	if nextRefresh.Before(before.Add(4*time.Minute + 50*time.Second)) {
		t.Fatalf("codex usage next refresh = %v, want about 5 minutes after %v", nextRefresh, before)
	}
	lastUsed := metadataTimeValue(t, stored, metadataLastUsedAtKey)
	if lastUsed.Before(before.Add(-5 * time.Second)) || lastUsed.After(time.Now().Add(5*time.Second)) {
		t.Fatalf("last used timestamp looks incorrect: %v", lastUsed)
	}
}

func TestManager_MarkResult_CodexSuccessPreservesEarlierUsageRefreshSchedule(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	now := time.Now().UTC()
	existingNext := now.Add(45 * time.Second)
	auth := &Auth{
		ID:       "auths/codex-usage-preserve.json",
		FileName: "auths/codex-usage-preserve.json",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":                    "codex",
			metadataCodexUsageAfterKey: existingNext.Unix(),
		},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gpt-5",
		Success:  true,
	})

	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth to remain registered")
	}
	nextRefresh := metadataTimeValue(t, stored, metadataCodexUsageAfterKey)
	if delta := nextRefresh.Sub(existingNext); delta < -time.Second || delta > time.Second {
		t.Fatalf("codex usage next refresh = %v, want existing schedule near %v", nextRefresh, existingNext)
	}
}

func TestCodexUsageRefreshSchedule_EnforcesFiveMinuteFetchCooldown(t *testing.T) {
	now := time.Now().UTC()
	auth := &Auth{
		ID:       "auths/codex-usage-cooldown.json",
		Provider: "codex",
		Metadata: map[string]any{
			metadataLastUsedAtKey:      now.Add(-2 * time.Minute).Format(time.RFC3339Nano),
			metadataCodexUsageLastKey:  now.Add(-1 * time.Minute).Unix(),
			metadataCodexUsageAfterKey: now.Add(-30 * time.Second).Unix(),
		},
	}

	due, last := codexUsageRefreshSchedule(auth)
	if last.IsZero() {
		t.Fatalf("expected last fetch to be recorded")
	}
	minDue := last.Add(codexUsageRefreshDelay)
	if due.IsZero() {
		t.Fatalf("expected codex usage refresh to be scheduled")
	}
	if due.Sub(minDue) < -time.Second || due.Sub(minDue) > time.Second {
		t.Fatalf("due = %v, want %v", due, minDue)
	}
}

func TestShouldAutoDisableCodexAuthOnQuotaExceeded_OnlyForQuotaSignals(t *testing.T) {
	cfg := &internalconfig.Config{}
	auth := &Auth{ID: "codex-team", Provider: "codex", Metadata: map[string]any{"type": "codex", "plan_type": "team"}}

	if shouldAutoDisableCodexAuthOnQuotaExceeded(auth, &Error{HTTPStatus: http.StatusTooManyRequests, Message: `{"detail":"Rate limit exceeded"}`}, cfg) {
		t.Fatalf("expected generic rate limit exceeded error not to auto-disable codex auth")
	}
	if !shouldAutoDisableCodexAuthOnQuotaExceeded(auth, &Error{HTTPStatus: http.StatusTooManyRequests, Message: `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`}, cfg) {
		t.Fatalf("expected usage_limit_reached error to auto-disable codex auth")
	}
	if shouldAutoDisableCodexAuthOnQuotaExceeded(auth, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "Selected model is at capacity. Please try a different model."}, cfg) {
		t.Fatalf("expected capacity 429 not to auto-disable codex auth")
	}
}

func TestManager_MarkResult_CodexRateLimit429SchedulesTransientCooldownOnly(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auths/codex-team-429.json",
		FileName: "auths/codex-team-429.json",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "codex", "plan_type": "team"},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	retryAfter := 30 * time.Minute
	mgr.MarkResult(context.Background(), Result{
		AuthID:     auth.ID,
		Provider:   auth.Provider,
		Model:      "gpt-5",
		Success:    false,
		RetryAfter: &retryAfter,
		Error:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: `{"detail":"Rate limit exceeded"}`},
	})

	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth to remain registered")
	}
	if stored.Disabled {
		t.Fatalf("expected auth to remain enabled")
	}
	if stored.Status != StatusError {
		t.Fatalf("status = %q, want %q", stored.Status, StatusError)
	}
	if reason := quotaAutoDisabledReason(stored); reason != "" {
		t.Fatalf("auto-disabled reason = %q, want empty", reason)
	}
	if stored.TransientCooldownUntil.IsZero() || stored.TransientCooldownUntil.Before(time.Now().Add(29*time.Minute)) {
		t.Fatalf("expected transient cooldown from retry-after, got %v", stored.TransientCooldownUntil)
	}
	if stored.Quota.Exceeded || !stored.Quota.NextRecoverAt.IsZero() {
		t.Fatalf("expected no durable quota state, got %+v", stored.Quota)
	}
	if stored.NextRetryAfter.IsZero() || stored.NextRetryAfter.Before(time.Now().Add(29*time.Minute)) {
		t.Fatalf("expected next retry after from retry-after, got %v", stored.NextRetryAfter)
	}
}

func TestManager_MarkResult_CodexRateLimit429WithoutRetryAfterUses60SecondTransientCooldown(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auths/codex-team-429-no-retry-after.json",
		FileName: "auths/codex-team-429-no-retry-after.json",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "codex", "plan_type": "team"},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	before := time.Now()
	mgr.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gpt-5",
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusTooManyRequests, Message: `{"detail":"Rate limit exceeded"}`},
	})

	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth to remain registered")
	}
	if stored.Disabled {
		t.Fatalf("expected auth to remain enabled")
	}
	if stored.Status != StatusError {
		t.Fatalf("status = %q, want %q", stored.Status, StatusError)
	}
	if stored.TransientCooldownUntil.IsZero() {
		t.Fatalf("expected transient cooldown to be set")
	}
	minUntil := before.Add(55 * time.Second)
	maxUntil := before.Add(65 * time.Second)
	if stored.TransientCooldownUntil.Before(minUntil) || stored.TransientCooldownUntil.After(maxUntil) {
		t.Fatalf("TransientCooldownUntil = %v, want around %v..%v", stored.TransientCooldownUntil, minUntil, maxUntil)
	}
	if stored.NextRetryAfter.Before(minUntil) || stored.NextRetryAfter.After(maxUntil) {
		t.Fatalf("NextRetryAfter = %v, want around %v..%v", stored.NextRetryAfter, minUntil, maxUntil)
	}
	if stored.Quota.Exceeded || !stored.Quota.NextRecoverAt.IsZero() {
		t.Fatalf("expected no durable quota state, got %+v", stored.Quota)
	}
}

func TestManager_QuotaRefresh_CodexLowBalanceAutoDisablesWhenUnusedForFiveMinutes(t *testing.T) {
	store := &deletingStore{}
	exec := &quotaProbeTestExecutor{
		id: "codex",
		httpRequest: func(_ context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
			if req == nil {
				t.Fatalf("expected usage refresh request")
			}
			if got := req.Header.Get("Chatgpt-Account-Id"); got != "acct-low-balance" {
				t.Fatalf("Chatgpt-Account-Id = %q, want %q", got, "acct-low-balance")
			}
			return newCodexUsageHTTPResponse(http.StatusOK, `{
				"plan_type":"team",
				"rate_limit":{
					"primary_window":{"used_percent":94,"limit_window_seconds":18000,"reset_after_seconds":900},
					"secondary_window":{"used_percent":91,"limit_window_seconds":604800,"reset_after_seconds":7200}
				}
			}`, nil), nil
		},
	}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(exec)
	now := time.Now().UTC()
	auth := &Auth{
		ID:       "auths/codex-low-balance.json",
		FileName: "auths/codex-low-balance.json",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":                    "codex",
			"account_id":              "acct-low-balance",
			metadataLastUsedAtKey:     now.Add(-6 * time.Minute).Format(time.RFC3339Nano),
			metadataCodexUsageAfterKey: now.Add(-time.Minute).Unix(),
		},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.refreshQuotaAuth(context.Background(), auth.ID)
	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth to remain registered")
	}
	if !stored.Disabled || stored.Status != StatusDisabled {
		t.Fatalf("expected low-balance auth to be disabled, got disabled=%v status=%q", stored.Disabled, stored.Status)
	}
	if !quotaProbeAutoDisabled(stored) {
		t.Fatalf("expected low-balance auth to carry auto-disabled marker")
	}
	if reason := quotaAutoDisabledReason(stored); reason != autoDisabledReasonQuotaLowBalance {
		t.Fatalf("auto-disabled reason = %q, want %q", reason, autoDisabledReasonQuotaLowBalance)
	}
	if stored.Quota.NextRecoverAt.IsZero() {
		t.Fatalf("expected quota next recover at to be set")
	}
	if remaining, ok := stored.Metadata[metadataCodexUsageRemainingKey].(float64); !ok || remaining >= codexLowRemainingThresholdPct {
		t.Fatalf("remaining percent metadata = %#v, want value below %.1f", stored.Metadata[metadataCodexUsageRemainingKey], codexLowRemainingThresholdPct)
	}
	if _, ok := stored.Metadata[metadataCodexUsagePayloadKey].(string); !ok {
		t.Fatalf("expected usage payload to be cached")
	}
}

func TestManager_QuotaRefresh_CodexUsageSuccessReenablesLowBalanceAutoDisabledAuth(t *testing.T) {
	store := &deletingStore{}
	exec := &quotaProbeTestExecutor{
		id: "codex",
		httpRequest: func(context.Context, *Auth, *http.Request) (*http.Response, error) {
			return newCodexUsageHTTPResponse(http.StatusOK, `{
				"plan_type":"team",
				"rate_limit":{
					"primary_window":{"used_percent":40,"limit_window_seconds":18000,"reset_after_seconds":900},
					"secondary_window":{"used_percent":30,"limit_window_seconds":604800,"reset_after_seconds":7200}
				}
			}`, nil), nil
		},
	}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(exec)
	now := time.Now().UTC()
	auth := &Auth{
		ID:       "auths/codex-low-balance-recover.json",
		FileName: "auths/codex-low-balance-recover.json",
		Provider: "codex",
		Disabled: true,
		Status:   StatusDisabled,
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        autoDisabledReasonQuotaLowBalance,
			NextRecoverAt: now.Add(2 * time.Hour),
		},
		Metadata: map[string]any{
			"type":                         "codex",
			"account_id":                   "acct-recover",
			metadataAutoDisabledReasonKey:  autoDisabledReasonQuotaLowBalance,
			metadataCodexUsageAfterKey:     now.Add(-time.Minute).Unix(),
			metadataCodexUsagePayloadKey:   `{"stale":true}`,
			metadataCodexUsageRemainingKey: 3.2,
		},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.refreshQuotaAuth(context.Background(), auth.ID)
	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth to remain registered")
	}
	if stored.Disabled || stored.Status != StatusActive {
		t.Fatalf("expected auth to be re-enabled, got disabled=%v status=%q", stored.Disabled, stored.Status)
	}
	if quotaProbeAutoDisabled(stored) {
		t.Fatalf("expected auto-disabled reason to be cleared")
	}
	if stored.Quota.Exceeded {
		t.Fatalf("expected quota exceeded flag to be cleared, got %+v", stored.Quota)
	}
	remaining, ok := stored.Metadata[metadataCodexUsageRemainingKey].(float64)
	if !ok || remaining <= codexLowRemainingThresholdPct {
		t.Fatalf("remaining percent metadata = %#v, want value above %.1f", stored.Metadata[metadataCodexUsageRemainingKey], codexLowRemainingThresholdPct)
	}
}

func TestManager_QuotaRefresh_CodexUsageLowBalanceRespectsDisableCooling(t *testing.T) {
	store := &deletingStore{}
	exec := &quotaProbeTestExecutor{
		id: "codex",
		httpRequest: func(context.Context, *Auth, *http.Request) (*http.Response, error) {
			return newCodexUsageHTTPResponse(http.StatusOK, `{
				"plan_type":"team",
				"rate_limit":{
					"primary_window":{"used_percent":94,"limit_window_seconds":18000,"reset_after_seconds":900},
					"secondary_window":{"used_percent":91,"limit_window_seconds":604800,"reset_after_seconds":7200}
				}
			}`, nil), nil
		},
	}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(exec)
	now := time.Now().UTC()
	auth := &Auth{
		ID:       "auths/codex-low-balance-disable-cooling.json",
		FileName: "auths/codex-low-balance-disable-cooling.json",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":                     "codex",
			"account_id":               "acct-low-balance-disable-cooling",
			"disable_cooling":          true,
			metadataCodexUsageAfterKey: now.Add(-time.Minute).Unix(),
		},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.refreshQuotaAuth(context.Background(), auth.ID)
	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth to remain registered")
	}
	if stored.Disabled || stored.Status != StatusActive {
		t.Fatalf("expected low-balance auth to remain schedulable when disable_cooling=true, got disabled=%v status=%q", stored.Disabled, stored.Status)
	}
	if quotaProbeAutoDisabled(stored) {
		t.Fatalf("expected low-balance auth not to carry auto-disabled marker when disable_cooling=true")
	}
	if stored.Quota.Exceeded {
		t.Fatalf("expected auth quota state not to mark exceeded when disable_cooling=true, got %+v", stored.Quota)
	}
	if remaining, ok := stored.Metadata[metadataCodexUsageRemainingKey].(float64); !ok || remaining >= codexLowRemainingThresholdPct {
		t.Fatalf("remaining percent metadata = %#v, want value below %.1f", stored.Metadata[metadataCodexUsageRemainingKey], codexLowRemainingThresholdPct)
	}
	if next, ok := stored.Metadata[metadataCodexUsageAfterKey].(int64); !ok || next <= now.Unix() {
		t.Fatalf("expected next usage refresh to still be scheduled, got %#v", stored.Metadata[metadataCodexUsageAfterKey])
	}
}

func TestManager_QuotaRefresh_CodexUsageDisablesWhenWeeklyWindowExhausted(t *testing.T) {
	store := &deletingStore{}
	exec := &quotaProbeTestExecutor{
		id: "codex",
		httpRequest: func(context.Context, *Auth, *http.Request) (*http.Response, error) {
			return newCodexUsageHTTPResponse(http.StatusOK, `{
				"plan_type":"team",
				"rate_limit":{
					"primary_window":{"used_percent":40,"limit_window_seconds":18000,"reset_after_seconds":900},
					"secondary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_after_seconds":7200}
				}
			}`, nil), nil
		},
	}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(exec)
	now := time.Now().UTC()
	auth := &Auth{
		ID:       "auths/codex-weekly-exhausted.json",
		FileName: "auths/codex-weekly-exhausted.json",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":                     "codex",
			"account_id":               "acct-weekly-exhausted",
			metadataCodexUsageAfterKey: now.Add(-time.Minute).Unix(),
		},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.refreshQuotaAuth(context.Background(), auth.ID)
	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth to remain registered")
	}
	if !stored.Disabled || stored.Status != StatusDisabled {
		t.Fatalf("expected weekly-exhausted auth to be disabled, got disabled=%v status=%q", stored.Disabled, stored.Status)
	}
	if stored.Quota.NextRecoverAt.Before(now.Add(7100 * time.Second)) {
		t.Fatalf("expected next recover at to follow weekly window reset, got %v", stored.Quota.NextRecoverAt)
	}
}

func TestManager_QuotaRefresh_CodexUsageKeepsAutoDisabledWhenWeeklyWindowStillBlocked(t *testing.T) {
	store := &deletingStore{}
	exec := &quotaProbeTestExecutor{
		id: "codex",
		httpRequest: func(context.Context, *Auth, *http.Request) (*http.Response, error) {
			return newCodexUsageHTTPResponse(http.StatusOK, `{
				"plan_type":"team",
				"rate_limit":{
					"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":900},
					"secondary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_after_seconds":7200}
				}
			}`, nil), nil
		},
	}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(exec)
	now := time.Now().UTC()
	auth := &Auth{
		ID:       "auths/codex-weekly-still-blocked.json",
		FileName: "auths/codex-weekly-still-blocked.json",
		Provider: "codex",
		Disabled: true,
		Status:   StatusDisabled,
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        autoDisabledReasonQuotaLowBalance,
			NextRecoverAt: now.Add(2 * time.Hour),
		},
		Metadata: map[string]any{
			"type":                        "codex",
			"account_id":                  "acct-weekly-still-blocked",
			metadataAutoDisabledReasonKey: autoDisabledReasonQuotaLowBalance,
			metadataCodexUsageAfterKey:    now.Add(-time.Minute).Unix(),
		},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.refreshQuotaAuth(context.Background(), auth.ID)
	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth to remain registered")
	}
	if !stored.Disabled || stored.Status != StatusDisabled {
		t.Fatalf("expected auth to stay disabled, got disabled=%v status=%q", stored.Disabled, stored.Status)
	}
	if !quotaProbeAutoDisabled(stored) {
		t.Fatalf("expected auto-disabled reason to remain present")
	}
}

func TestManager_ForceRefreshQuotaAuth_TriggersCodexUsageImmediately(t *testing.T) {
	store := &deletingStore{}
	httpCalls := 0
	exec := &quotaProbeTestExecutor{
		id: "codex",
		httpRequest: func(context.Context, *Auth, *http.Request) (*http.Response, error) {
			httpCalls++
			return newCodexUsageHTTPResponse(http.StatusOK, `{
				"plan_type":"team",
				"rate_limit":{
					"primary_window":{"used_percent":20,"limit_window_seconds":18000,"reset_after_seconds":900},
					"secondary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_after_seconds":7200}
				}
			}`, nil), nil
		},
	}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(exec)
	auth := &Auth{
		ID:       "auths/codex-force-refresh.json",
		FileName: "auths/codex-force-refresh.json",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":       "codex",
			"account_id": "acct-force-refresh",
		},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.ForceRefreshQuotaAuth(context.Background(), auth.ID)
	if httpCalls != 1 {
		t.Fatalf("httpCalls = %d, want 1", httpCalls)
	}
}

func TestManager_RecordResult_QuotaErrorSchedulesRecoveryProbeAndPenalty(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{ID: "auths/quota-runtime.json", FileName: "auths/quota-runtime.json", Provider: "codex", Metadata: map[string]any{"type": "codex"}}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	retryAfter := 90 * time.Second
	mgr.recordResult(context.Background(), Result{
		AuthID:     auth.ID,
		Provider:   auth.Provider,
		Model:      "gpt-5",
		Success:    false,
		RetryAfter: &retryAfter,
		Error:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
	})

	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth to remain registered")
	}
	if stored.TransientCooldownUntil.IsZero() || stored.TransientCooldownUntil.Before(time.Now().Add(80*time.Second)) {
		t.Fatalf("expected transient cooldown to be published, got %v", stored.TransientCooldownUntil)
	}
	if stored.QuotaPriorityPenalty <= 0 {
		t.Fatalf("expected quota priority penalty to be increased, got %d", stored.QuotaPriorityPenalty)
	}
	nextProbe, _ := quotaProbeSchedule(stored)
	if nextProbe.IsZero() || nextProbe.Before(time.Now().Add(80*time.Second)) {
		t.Fatalf("expected recovery probe to be scheduled after cooldown, got %v", nextProbe)
	}
	if !stored.Quota.Exceeded || stored.Quota.NextRecoverAt.IsZero() {
		t.Fatalf("expected durable quota state to be recorded, got %+v", stored.Quota)
	}
}

func TestApplyQuotaProbeSuccessState_RelaxesQuotaPriorityPenalty(t *testing.T) {
	auth := &Auth{QuotaPriorityPenalty: 3, TransientCooldownUntil: time.Now().Add(5 * time.Minute)}
	applyQuotaProbeSuccessState(auth, time.Now())
	if auth.QuotaPriorityPenalty != 2 {
		t.Fatalf("QuotaPriorityPenalty = %d, want %d", auth.QuotaPriorityPenalty, 2)
	}
	if !auth.TransientCooldownUntil.IsZero() {
		t.Fatalf("expected transient cooldown to be cleared, got %v", auth.TransientCooldownUntil)
	}
}


func TestManager_QuotaRefresh_DoesNotHoldManagerLockWhilePersisting(t *testing.T) {
	store := &blockingStore{saveStarted: make(chan struct{}), allowSave: make(chan struct{})}
	exec := &quotaProbeTestExecutor{id: "codex"}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(exec)
	auth := &Auth{
		ID:       "auths/quota-lock.json",
		FileName: "auths/quota-lock.json",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	done := make(chan struct{})
	go func() {
		mgr.refreshQuotaAuth(context.Background(), auth.ID)
		close(done)
	}()

	select {
	case <-store.saveStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Save to start")
	}

	lookupDone := make(chan struct{})
	go func() {
		_, _ = mgr.GetByID(auth.ID)
		close(lookupDone)
	}()

	select {
	case <-lookupDone:
	case <-time.After(250 * time.Millisecond):
		close(store.allowSave)
		t.Fatal("GetByID blocked while quota refresh Save was in progress")
	}

	close(store.allowSave)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refreshQuotaAuth did not finish after Save was released")
	}
}

func TestManager_QuotaRefresh_DeletesFreeCodexAuthOn403(t *testing.T) {
	store := &deletingStore{}
	exec := &quotaProbeTestExecutor{
		id: "codex",
		execute: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			return cliproxyexecutor.Response{}, quotaProbeStatusError{status: http.StatusForbidden, message: "authorization lost"}
		},
	}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(exec)
	auth := &Auth{
		ID:       "auths/quota-free-403.json",
		FileName: "auths/quota-free-403.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "/tmp/quota-free-403.json",
		},
		Metadata: map[string]any{"type": "codex", "plan_type": "free"},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	mgr.refreshQuotaAuth(context.Background(), auth.ID)

	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatalf("expected free quota probe auth to be removed on 403")
	}
	waitForDeletedIDs(t, store, []string{auth.ID})
	if exec.attempts != 1 {
		t.Fatalf("attempts = %d, want 1", exec.attempts)
	}
}

func TestQuotaProbeSchedule_UsesPersistedQuotaRecoveryWhenTransientMetadataMissing(t *testing.T) {
	now := time.Now().UTC()
	auth := &Auth{
		ID:       "auths/quota-persisted-recovery.json",
		Provider: "codex",
		Disabled: true,
		Status:   StatusDisabled,
		Metadata: map[string]any{
			"type":                         "codex",
			metadataAutoDisabledReasonKey: autoDisabledReasonQuotaExhausted,
		},
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: now.Add(6 * time.Hour),
		},
		NextRetryAfter: now.Add(6 * time.Hour),
	}

	nextProbe, lastProbe := quotaProbeSchedule(auth)
	if lastProbe != (time.Time{}) {
		t.Fatalf("expected no last probe timestamp from stripped metadata, got %v", lastProbe)
	}
	if nextProbe.IsZero() {
		t.Fatal("expected persisted quota recovery time to be used as next probe")
	}
	if nextProbe.Sub(auth.Quota.NextRecoverAt) > time.Second || auth.Quota.NextRecoverAt.Sub(nextProbe) > time.Second {
		t.Fatalf("next probe = %v, want %v", nextProbe, auth.Quota.NextRecoverAt)
	}
}

func TestManager_QuotaRefresh_DeletesFreeCodexAuthOn429(t *testing.T) {
	store := &deletingStore{}
	exec := &quotaProbeTestExecutor{
		id: "codex",
		execute: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			return cliproxyexecutor.Response{}, quotaProbeStatusError{status: http.StatusTooManyRequests, message: "Selected model is at capacity. Please try a different model."}
		},
	}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(exec)
	auth := &Auth{
		ID:       "auths/quota-free-429.json",
		FileName: "auths/quota-free-429.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "/tmp/quota-free-429.json",
		},
		Metadata: map[string]any{"type": "codex", "plan_type": "free"},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	mgr.refreshQuotaAuth(context.Background(), auth.ID)

	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatalf("expected free quota probe auth to be removed on 429")
	}
	waitForDeletedIDs(t, store, []string{auth.ID})
	if exec.attempts != 1 {
		t.Fatalf("attempts = %d, want 1", exec.attempts)
	}
}

func TestManager_PickQuotaRefreshBatch_SelectsIdleAuthsBeforeNormalDue(t *testing.T) {
	now := time.Now().UTC()
	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			IdleProbeAfterHours: 24,
		},
	})
	mgr.RegisterExecutor(&quotaProbeTestExecutor{id: "codex"})

	idleAuth := &Auth{
		ID:        "idle-auth",
		Provider:  "codex",
		CreatedAt: now.Add(-72 * time.Hour),
		Metadata: map[string]any{
			metadataPlanTypeKey:      "team",
			metadataQuotaProbeAfterKey: now.Add(2 * time.Hour).Unix(),
			metadataQuotaProbeLastKey:  now.Add(-30 * time.Minute).Unix(),
			metadataLastUsedAtKey:      now.Add(-48 * time.Hour).Format(time.RFC3339Nano),
		},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), idleAuth); err != nil {
		t.Fatalf("Register(idleAuth) error = %v", err)
	}

	dueAuth := &Auth{
		ID:        "due-auth",
		Provider:  "codex",
		CreatedAt: now.Add(-2 * time.Hour),
		Metadata: map[string]any{
			metadataPlanTypeKey:      "team",
			metadataQuotaProbeAfterKey: now.Add(-5 * time.Minute).Unix(),
			metadataQuotaProbeLastKey:  now.Add(-2 * time.Hour).Unix(),
			metadataLastUsedAtKey:      now.Add(-30 * time.Minute).Format(time.RFC3339Nano),
		},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), dueAuth); err != nil {
		t.Fatalf("Register(dueAuth) error = %v", err)
	}

	selected := mgr.pickQuotaRefreshBatch(now, 2)
	if len(selected) != 2 {
		t.Fatalf("len(selected) = %d, want 2", len(selected))
	}
	if selected[0] != idleAuth.ID {
		t.Fatalf("selected[0] = %q, want %q", selected[0], idleAuth.ID)
	}
	if selected[1] != dueAuth.ID {
		t.Fatalf("selected[1] = %q, want %q", selected[1], dueAuth.ID)
	}
}

func TestManager_PickQuotaRefreshBatch_PrefersLongestIdleAuthFirst(t *testing.T) {
	now := time.Now().UTC()
	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			IdleProbeAfterHours: 24,
		},
	})
	mgr.RegisterExecutor(&quotaProbeTestExecutor{id: "codex"})

	longIdleAuth := &Auth{
		ID:        "long-idle-auth",
		Provider:  "codex",
		CreatedAt: now.Add(-10 * 24 * time.Hour),
		Metadata: map[string]any{
			metadataPlanTypeKey:      "team",
			metadataQuotaProbeAfterKey: now.Add(3 * time.Hour).Unix(),
			metadataQuotaProbeLastKey:  now.Add(-15 * time.Minute).Unix(),
			metadataLastUsedAtKey:      now.Add(-96 * time.Hour).Format(time.RFC3339Nano),
		},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), longIdleAuth); err != nil {
		t.Fatalf("Register(longIdleAuth) error = %v", err)
	}

	shortIdleAuth := &Auth{
		ID:        "short-idle-auth",
		Provider:  "codex",
		CreatedAt: now.Add(-5 * 24 * time.Hour),
		Metadata: map[string]any{
			metadataPlanTypeKey:      "team",
			metadataQuotaProbeAfterKey: now.Add(3 * time.Hour).Unix(),
			metadataQuotaProbeLastKey:  now.Add(-15 * time.Minute).Unix(),
			metadataLastUsedAtKey:      now.Add(-30 * time.Hour).Format(time.RFC3339Nano),
		},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), shortIdleAuth); err != nil {
		t.Fatalf("Register(shortIdleAuth) error = %v", err)
	}

	selected := mgr.pickQuotaRefreshBatch(now, 2)
	if len(selected) != 2 {
		t.Fatalf("len(selected) = %d, want 2", len(selected))
	}
	if selected[0] != longIdleAuth.ID {
		t.Fatalf("selected[0] = %q, want %q", selected[0], longIdleAuth.ID)
	}
	if selected[1] != shortIdleAuth.ID {
		t.Fatalf("selected[1] = %q, want %q", selected[1], shortIdleAuth.ID)
	}
}

func TestManager_PickQuotaRefreshBatch_DoesNotIdleProbeAutoDisabledQuotaAuthBeforeRecovery(t *testing.T) {
	now := time.Now().UTC()
	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			IdleProbeAfterHours: 24,
		},
	})
	mgr.RegisterExecutor(&quotaProbeTestExecutor{id: "codex"})

	autoDisabled := &Auth{
		ID:        "auto-disabled-quota-auth",
		Provider:  "codex",
		Disabled:  true,
		Status:    StatusDisabled,
		CreatedAt: now.Add(-10 * 24 * time.Hour),
		Metadata: map[string]any{
			metadataPlanTypeKey:           "free",
			metadataAutoDisabledReasonKey: autoDisabledReasonQuotaExhausted,
			metadataLastUsedAtKey:         now.Add(-96 * time.Hour).Format(time.RFC3339Nano),
		},
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: now.Add(3 * time.Hour),
		},
		NextRetryAfter: now.Add(3 * time.Hour),
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), autoDisabled); err != nil {
		t.Fatalf("Register(autoDisabled) error = %v", err)
	}

	selected := mgr.pickQuotaRefreshBatch(now, 1)
	if len(selected) != 0 {
		t.Fatalf("expected auto-disabled quota auth to stay unscheduled before recovery, got %v", selected)
	}
}
