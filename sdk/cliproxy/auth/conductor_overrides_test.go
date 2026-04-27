package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

const requestScopedNotFoundMessage = "Item with id 'rs_0b5f3eb6f51f175c0169ca74e4a85881998539920821603a74' not found. Items are not persisted when `store` is set to false. Try again with `store` set to true, or remove this item from your input."

func TestManager_ShouldRetryAfterError_RespectsAuthRequestRetryOverride(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 30*time.Second, 0)

	model := "test-model"
	next := time.Now().Add(5 * time.Second)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{
			"request_retry": float64(0),
		},
		ModelStates: map[string]*ModelState{
			model: {
				Unavailable:    true,
				Status:         StatusError,
				NextRetryAfter: next,
			},
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	_, _, maxWait := m.retrySettings()
	wait, shouldRetry := m.shouldRetryAfterError(&Error{HTTPStatus: 500, Message: "boom"}, 0, []string{"claude"}, model, maxWait)
	if shouldRetry {
		t.Fatalf("expected shouldRetry=false for request_retry=0, got true (wait=%v)", wait)
	}

	auth.Metadata["request_retry"] = float64(1)
	if _, errUpdate := m.Update(context.Background(), auth); errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}

	wait, shouldRetry = m.shouldRetryAfterError(&Error{HTTPStatus: 500, Message: "boom"}, 0, []string{"claude"}, model, maxWait)
	if !shouldRetry {
		t.Fatalf("expected shouldRetry=true for request_retry=1, got false")
	}
	if wait <= 0 {
		t.Fatalf("expected wait > 0, got %v", wait)
	}

	_, shouldRetry = m.shouldRetryAfterError(&Error{HTTPStatus: 500, Message: "boom"}, 1, []string{"claude"}, model, maxWait)
	if shouldRetry {
		t.Fatalf("expected shouldRetry=false on attempt=1 for request_retry=1, got true")
	}
}

func TestManager_MarkResult_ImageUnsupportedRegionClearsBoundProxy(t *testing.T) {
	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-image-region",
		Provider: "codex",
		Metadata: map[string]any{
			MetadataProbedPlanTypeKey: "plus",
		},
	}
	SetBoundProxyEntry(auth, "proxy-a")
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-image-2",
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusInternalServerError,
			Message:    `{"error":{"code":"unsupported_country_region_territory","message":"Country, region, or territory not supported","type":"request_forbidden"}}`,
		},
		RequestPayload: []byte(`{"model":"gpt-5.4-mini","tools":[{"type":"image_generation","model":"gpt-image-2"}]}`),
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected auth to remain registered")
	}
	if updated.Disabled {
		t.Fatal("expected auth to remain enabled")
	}
	if got := BoundProxyEntry(updated); got != "" {
		t.Fatalf("bound proxy = %q, want empty", got)
	}
	if updated.Status != StatusError {
		t.Fatalf("status = %q, want %q", updated.Status, StatusError)
	}
	if updated.StatusMessage == "" || !strings.Contains(updated.StatusMessage, "unsupported_country_region_territory") {
		t.Fatalf("expected unsupported region status message, got %q", updated.StatusMessage)
	}
}

func TestManager_MarkResult_ImageInputRateLimitAppliesToolModelCooldown(t *testing.T) {
	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-image-input-limit",
		Provider: "codex",
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	retryAfter := 240 * time.Millisecond

	m.MarkResult(context.Background(), Result{
		AuthID:     auth.ID,
		Provider:   "codex",
		Model:      "gpt-image-2",
		Success:    false,
		RetryAfter: &retryAfter,
		Error: &Error{
			HTTPStatus: http.StatusTooManyRequests,
			Message:    `{"error":{"message":"Rate limit reached for gpt-image-2 (for limit gpt-image) on input-images per min: Limit 250, Used 250, Requested 1. Please try again in 240ms.","type":"upstream_error","code":"rate_limit_exceeded"}}`,
		},
		RequestPayload: []byte(`{"model":"gpt-5.4-mini","tools":[{"type":"image_generation","model":"gpt-image-2"}]}`),
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected auth to remain registered")
	}
	imageState := updated.ModelStates["gpt-image-2"]
	if imageState == nil {
		t.Fatalf("gpt-image-2 model state missing; states=%v", updated.ModelStates)
	}
	if !imageState.Unavailable {
		t.Fatalf("gpt-image-2 unavailable = false; state=%+v", imageState)
	}
	if imageState.Quota.Reason != "rate_limit" {
		t.Fatalf("quota reason = %q, want rate_limit", imageState.Quota.Reason)
	}
	if imageState.NextRetryAfter.IsZero() || time.Until(imageState.NextRetryAfter) > time.Second {
		t.Fatalf("next retry after = %s, want short image rate-limit cooldown", imageState.NextRetryAfter)
	}
}

func TestRecordCyberPolicyTriggerLocked_IncludesRequestTrace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Request.Header.Set("X-Oneapi-Request-Id", "oneapi-req-1")
	c.Request.Header.Set("X-Oneapi-User-Id", "784")
	c.Request.Header.Set("X-Oneapi-Username", "shsizhen")
	c.Request.Header.Set("X-Oneapi-Token-Id", "1031")
	c.Request.Header.Set("X-Oneapi-Token-Name", "11")
	c.Request.Header.Set("X-Forwarded-For", "23.151.104.40, 172.18.0.1")
	ctx := logging.WithRequestID(context.Background(), "cpa-req-1")
	ctx = context.WithValue(ctx, "gin", c)

	auth := &Auth{ID: "auth-1", Provider: "codex"}
	record := recordCyberPolicyTriggerLocked(auth, Result{
		AuthID:         auth.ID,
		Provider:       "codex",
		Model:          "gpt-5.5",
		Success:        false,
		Error:          &Error{HTTPStatus: http.StatusBadRequest, Code: "cyber_policy", Message: "cyber_policy"},
		RequestPayload: []byte(`{"model":"gpt-5.5"}`),
	}, time.Unix(1777182062, 0), ctx)

	if record.RequestID != "cpa-req-1" {
		t.Fatalf("request_id = %q, want cpa-req-1", record.RequestID)
	}
	if record.SourceRequestID != "oneapi-req-1" {
		t.Fatalf("source_request_id = %q, want oneapi-req-1", record.SourceRequestID)
	}
	if record.SourceUserID != "784" || record.SourceUsername != "shsizhen" {
		t.Fatalf("source user = %q/%q, want 784/shsizhen", record.SourceUserID, record.SourceUsername)
	}
	if record.SourceTokenID != "1031" || record.SourceTokenName != "11" {
		t.Fatalf("source token = %q/%q, want 1031/11", record.SourceTokenID, record.SourceTokenName)
	}
	if record.SourceIP != "23.151.104.40" {
		t.Fatalf("source_ip = %q, want 23.151.104.40", record.SourceIP)
	}
}

func TestManager_ShouldRetryAfterError_UsesOAuthModelAliasForCooldown(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 30*time.Second, 0)
	m.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"qwen": {
			{Name: "qwen3.6-plus", Alias: "coder-model"},
		},
	})

	routeModel := "coder-model"
	upstreamModel := "qwen3.6-plus"
	next := time.Now().Add(5 * time.Second)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "qwen",
		ModelStates: map[string]*ModelState{
			upstreamModel: {
				Unavailable:    true,
				Status:         StatusError,
				NextRetryAfter: next,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: next,
				},
			},
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	_, _, maxWait := m.retrySettings()
	wait, shouldRetry := m.shouldRetryAfterError(&Error{HTTPStatus: 429, Message: "quota"}, 0, []string{"qwen"}, routeModel, maxWait)
	if !shouldRetry {
		t.Fatalf("expected shouldRetry=true, got false (wait=%v)", wait)
	}
	if wait <= 0 {
		t.Fatalf("expected wait > 0, got %v", wait)
	}
}

type credentialRetryLimitExecutor struct {
	id string

	mu    sync.Mutex
	calls int
}

func (e *credentialRetryLimitExecutor) Identifier() string {
	return e.id
}

func (e *credentialRetryLimitExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.recordCall()
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: 500, Message: "boom"}
}

func (e *credentialRetryLimitExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.recordCall()
	return nil, &Error{HTTPStatus: 500, Message: "boom"}
}

func (e *credentialRetryLimitExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *credentialRetryLimitExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.recordCall()
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: 500, Message: "boom"}
}

func (e *credentialRetryLimitExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *credentialRetryLimitExecutor) recordCall() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
}

func (e *credentialRetryLimitExecutor) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

type authFallbackExecutor struct {
	id string

	mu                sync.Mutex
	executeCalls      []string
	streamCalls       []string
	executeErrors     map[string]error
	streamFirstErrors map[string]error
}

func (e *authFallbackExecutor) Identifier() string {
	return e.id
}

func (e *authFallbackExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.executeCalls = append(e.executeCalls, auth.ID)
	err := e.executeErrors[auth.ID]
	e.mu.Unlock()
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *authFallbackExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.streamCalls = append(e.streamCalls, auth.ID)
	err := e.streamFirstErrors[auth.ID]
	e.mu.Unlock()

	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	if err != nil {
		ch <- cliproxyexecutor.StreamChunk{Err: err}
		close(ch)
		return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Auth": {auth.ID}}, Chunks: ch}, nil
	}
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte(auth.ID)}
	close(ch)
	return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Auth": {auth.ID}}, Chunks: ch}, nil
}

func (e *authFallbackExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *authFallbackExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: 500, Message: "not implemented"}
}

func (e *authFallbackExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *authFallbackExecutor) ExecuteCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.executeCalls))
	copy(out, e.executeCalls)
	return out
}

func (e *authFallbackExecutor) StreamCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.streamCalls))
	copy(out, e.streamCalls)
	return out
}

type retryAfterStatusError struct {
	status     int
	message    string
	retryAfter time.Duration
}

func (e *retryAfterStatusError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func (e *retryAfterStatusError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.status
}

func (e *retryAfterStatusError) RetryAfter() *time.Duration {
	if e == nil {
		return nil
	}
	d := e.retryAfter
	return &d
}

func newCredentialRetryLimitTestManager(t *testing.T, maxRetryCredentials int) (*Manager, *credentialRetryLimitExecutor) {
	t.Helper()

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 0, maxRetryCredentials)

	executor := &credentialRetryLimitExecutor{id: "claude"}
	m.RegisterExecutor(executor)

	baseID := uuid.NewString()
	auth1 := &Auth{ID: baseID + "-auth-1", Provider: "claude"}
	auth2 := &Auth{ID: baseID + "-auth-2", Provider: "claude"}

	// Auth selection requires that the global model registry knows each credential supports the model.
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth1.ID, "claude", []*registry.ModelInfo{{ID: "test-model"}})
	reg.RegisterClient(auth2.ID, "claude", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth1.ID)
		reg.UnregisterClient(auth2.ID)
	})

	if _, errRegister := m.Register(context.Background(), auth1); errRegister != nil {
		t.Fatalf("register auth1: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), auth2); errRegister != nil {
		t.Fatalf("register auth2: %v", errRegister)
	}

	return m, executor
}

func TestManager_MaxRetryCredentials_LimitsCrossCredentialRetries(t *testing.T) {
	request := cliproxyexecutor.Request{Model: "test-model"}
	testCases := []struct {
		name   string
		invoke func(*Manager) error
	}{
		{
			name: "execute",
			invoke: func(m *Manager) error {
				_, errExecute := m.Execute(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
				return errExecute
			},
		},
		{
			name: "execute_count",
			invoke: func(m *Manager) error {
				_, errExecute := m.ExecuteCount(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
				return errExecute
			},
		},
		{
			name: "execute_stream",
			invoke: func(m *Manager) error {
				_, errExecute := m.ExecuteStream(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
				return errExecute
			},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			limitedManager, limitedExecutor := newCredentialRetryLimitTestManager(t, 1)
			if errInvoke := tc.invoke(limitedManager); errInvoke == nil {
				t.Fatalf("expected error for limited retry execution")
			}
			if calls := limitedExecutor.Calls(); calls != 1 {
				t.Fatalf("expected 1 call with max-retry-credentials=1, got %d", calls)
			}

			unlimitedManager, unlimitedExecutor := newCredentialRetryLimitTestManager(t, 0)
			if errInvoke := tc.invoke(unlimitedManager); errInvoke == nil {
				t.Fatalf("expected error for unlimited retry execution")
			}
			if calls := unlimitedExecutor.Calls(); calls != 2 {
				t.Fatalf("expected 2 calls with max-retry-credentials=0, got %d", calls)
			}
		})
	}
}

func TestManager_ModelSupportBadRequest_FallsBackAndSuspendsAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "claude",
		executeErrors: map[string]error{
			"aa-bad-auth": &Error{
				HTTPStatus: http.StatusBadRequest,
				Message:    "invalid_request_error: The requested model is not supported.",
			},
		},
	}
	m.RegisterExecutor(executor)

	model := "claude-opus-4-6"
	badAuth := &Auth{ID: "aa-bad-auth", Provider: "claude"}
	goodAuth := &Auth{ID: "bb-good-auth", Provider: "claude"}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(badAuth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(goodAuth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(badAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})

	if _, errRegister := m.Register(context.Background(), badAuth); errRegister != nil {
		t.Fatalf("register bad auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), goodAuth); errRegister != nil {
		t.Fatalf("register good auth: %v", errRegister)
	}

	request := cliproxyexecutor.Request{Model: model}
	for i := 0; i < 2; i++ {
		resp, errExecute := m.Execute(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
		if errExecute != nil {
			t.Fatalf("execute %d error = %v, want success", i, errExecute)
		}
		if string(resp.Payload) != goodAuth.ID {
			t.Fatalf("execute %d payload = %q, want %q", i, string(resp.Payload), goodAuth.ID)
		}
	}

	got := executor.ExecuteCalls()
	want := []string{badAuth.ID, goodAuth.ID, goodAuth.ID}
	if len(got) != len(want) {
		t.Fatalf("execute calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("execute call %d auth = %q, want %q", i, got[i], want[i])
		}
	}

	updatedBad, ok := m.GetByID(badAuth.ID)
	if !ok || updatedBad == nil {
		t.Fatalf("expected bad auth to remain registered")
	}
	state := updatedBad.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state for %q", model)
	}
	if !state.Unavailable {
		t.Fatalf("expected bad auth model state to be unavailable")
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatalf("expected bad auth model state cooldown to be set")
	}
}

func TestManagerExecuteStream_ModelSupportBadRequestFallsBackAndSuspendsAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "claude",
		streamFirstErrors: map[string]error{
			"aa-bad-auth": &Error{
				HTTPStatus: http.StatusBadRequest,
				Message:    "invalid_request_error: The requested model is not supported.",
			},
		},
	}
	m.RegisterExecutor(executor)

	model := "claude-opus-4-6"
	badAuth := &Auth{ID: "aa-bad-auth", Provider: "claude"}
	goodAuth := &Auth{ID: "bb-good-auth", Provider: "claude"}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(badAuth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(goodAuth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(badAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})

	if _, errRegister := m.Register(context.Background(), badAuth); errRegister != nil {
		t.Fatalf("register bad auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), goodAuth); errRegister != nil {
		t.Fatalf("register good auth: %v", errRegister)
	}

	request := cliproxyexecutor.Request{Model: model}
	for i := 0; i < 2; i++ {
		streamResult, errExecute := m.ExecuteStream(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
		if errExecute != nil {
			t.Fatalf("execute stream %d error = %v, want success", i, errExecute)
		}
		var payload []byte
		for chunk := range streamResult.Chunks {
			if chunk.Err != nil {
				t.Fatalf("execute stream %d chunk error = %v, want success", i, chunk.Err)
			}
			payload = append(payload, chunk.Payload...)
		}
		if string(payload) != goodAuth.ID {
			t.Fatalf("execute stream %d payload = %q, want %q", i, string(payload), goodAuth.ID)
		}
	}

	got := executor.StreamCalls()
	want := []string{badAuth.ID, goodAuth.ID, goodAuth.ID}
	if len(got) != len(want) {
		t.Fatalf("stream calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stream call %d auth = %q, want %q", i, got[i], want[i])
		}
	}

	updatedBad, ok := m.GetByID(badAuth.ID)
	if !ok || updatedBad == nil {
		t.Fatalf("expected bad auth to remain registered")
	}
	state := updatedBad.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state for %q", model)
	}
	if !state.Unavailable {
		t.Fatalf("expected bad auth model state to be unavailable")
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatalf("expected bad auth model state cooldown to be set")
	}
}

func TestManager_MarkResult_RespectsAuthDisableCoolingOverride(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "test-model"
	m.MarkResult(context.Background(), Result{
		AuthID:   "auth-1",
		Provider: "claude",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: 500, Message: "boom"},
	})

	updated, ok := m.GetByID("auth-1")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if !state.NextRetryAfter.IsZero() {
		t.Fatalf("expected NextRetryAfter to be zero when disable_cooling=true, got %v", state.NextRetryAfter)
	}
}

func TestManager_MarkResult_RespectsAuthDisableCoolingOverride_On403(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)

	auth := &Auth{
		ID:       "auth-403",
		Provider: "claude",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "test-model-403"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "claude",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusForbidden, Message: "forbidden"},
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if !state.NextRetryAfter.IsZero() {
		t.Fatalf("expected NextRetryAfter to be zero when disable_cooling=true, got %v", state.NextRetryAfter)
	}

	if count := reg.GetModelCount(model); count <= 0 {
		t.Fatalf("expected model count > 0 when disable_cooling=true, got %d", count)
	}
}

func TestManager_Execute_DisableCooling_DoesNotBlackoutAfter403(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "claude",
		executeErrors: map[string]error{
			"auth-403-exec": &Error{
				HTTPStatus: http.StatusForbidden,
				Message:    "forbidden",
			},
		},
	}
	m.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "auth-403-exec",
		Provider: "claude",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "test-model-403-exec"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	req := cliproxyexecutor.Request{Model: model}
	_, errExecute1 := m.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
	if errExecute1 == nil {
		t.Fatal("expected first execute error")
	}
	if statusCodeFromError(errExecute1) != http.StatusForbidden {
		t.Fatalf("first execute status = %d, want %d", statusCodeFromError(errExecute1), http.StatusForbidden)
	}

	_, errExecute2 := m.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
	if errExecute2 == nil {
		t.Fatal("expected second execute error")
	}
	if statusCodeFromError(errExecute2) != http.StatusForbidden {
		t.Fatalf("second execute status = %d, want %d", statusCodeFromError(errExecute2), http.StatusForbidden)
	}
}

func TestManager_Execute_DisableCooling_DoesNotBlackoutAfter429RetryAfter(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "claude",
		executeErrors: map[string]error{
			"auth-429-exec": &retryAfterStatusError{
				status:     http.StatusTooManyRequests,
				message:    "quota exhausted",
				retryAfter: 2 * time.Minute,
			},
		},
	}
	m.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "auth-429-exec",
		Provider: "claude",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "test-model-429-exec"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	req := cliproxyexecutor.Request{Model: model}
	_, errExecute1 := m.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
	if errExecute1 == nil {
		t.Fatal("expected first execute error")
	}
	if statusCodeFromError(errExecute1) != http.StatusTooManyRequests {
		t.Fatalf("first execute status = %d, want %d", statusCodeFromError(errExecute1), http.StatusTooManyRequests)
	}

	_, errExecute2 := m.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
	if errExecute2 == nil {
		t.Fatal("expected second execute error")
	}
	if statusCodeFromError(errExecute2) != http.StatusTooManyRequests {
		t.Fatalf("second execute status = %d, want %d", statusCodeFromError(errExecute2), http.StatusTooManyRequests)
	}

	calls := executor.ExecuteCalls()
	if len(calls) != 2 {
		t.Fatalf("execute calls = %d, want 2", len(calls))
	}

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if !state.NextRetryAfter.IsZero() {
		t.Fatalf("expected NextRetryAfter to be zero when disable_cooling=true, got %v", state.NextRetryAfter)
	}
}

func TestManager_Execute_DisableCooling_RetriesAfter429RetryAfter(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 100*time.Millisecond, 0)

	executor := &authFallbackExecutor{
		id: "claude",
		executeErrors: map[string]error{
			"auth-429-retryafter-exec": &retryAfterStatusError{
				status:     http.StatusTooManyRequests,
				message:    "quota exhausted",
				retryAfter: 5 * time.Millisecond,
			},
		},
	}
	m.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "auth-429-retryafter-exec",
		Provider: "claude",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "test-model-429-retryafter-exec"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	req := cliproxyexecutor.Request{Model: model}
	_, errExecute := m.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
	if errExecute == nil {
		t.Fatal("expected execute error")
	}
	if statusCodeFromError(errExecute) != http.StatusTooManyRequests {
		t.Fatalf("execute status = %d, want %d", statusCodeFromError(errExecute), http.StatusTooManyRequests)
	}

	calls := executor.ExecuteCalls()
	if len(calls) != 4 {
		t.Fatalf("execute calls = %d, want 4 (initial + 3 retries)", len(calls))
	}
}

func TestManager_Execute_Plain429RateLimitSuspendsAuthFor60SecondsAndSwitchesAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(1, 100*time.Millisecond, 2)

	testKey := strings.ReplaceAll(t.Name(), "/", "-") + "-" + uuid.NewString()
	rateLimitedID := testKey + "-auth-001-rate-limited"
	healthyID := testKey + "-auth-999-healthy"
	executor := &authFallbackExecutor{
		id: "codex",
		executeErrors: map[string]error{
			rateLimitedID: &retryAfterStatusError{
				status:  http.StatusTooManyRequests,
				message: `{"detail":"Rate limit exceeded"}`,
			},
		},
	}
	m.RegisterExecutor(executor)

	baseCreatedAt := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	rateLimited := &Auth{
		ID:        rateLimitedID,
		Provider:  "codex",
		CreatedAt: baseCreatedAt.Add(time.Second),
		Metadata: map[string]any{
			"disable_cooling": false,
		},
	}
	healthy := &Auth{ID: healthyID, Provider: "codex", CreatedAt: baseCreatedAt}

	if _, errRegister := m.Register(context.Background(), rateLimited); errRegister != nil {
		t.Fatalf("register rate limited auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), healthy); errRegister != nil {
		t.Fatalf("register healthy auth: %v", errRegister)
	}

	model := testKey + "-model"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(rateLimited.ID, "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(healthy.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(rateLimited.ID)
		reg.UnregisterClient(healthy.ID)
	})

	resp, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute error = %v, want nil", errExecute)
	}
	if string(resp.Payload) != healthy.ID {
		t.Fatalf("response payload = %q, want %q", string(resp.Payload), healthy.ID)
	}

	calls := executor.ExecuteCalls()
	wantCalls := []string{rateLimited.ID, healthy.ID}
	if len(calls) != len(wantCalls) {
		t.Fatalf("execute calls = %v, want %v", calls, wantCalls)
	}
	for i := range wantCalls {
		if calls[i] != wantCalls[i] {
			t.Fatalf("execute call %d auth = %q, want %q", i, calls[i], wantCalls[i])
		}
	}

	updated, ok := m.GetByID(rateLimited.ID)
	if !ok || updated == nil {
		t.Fatalf("expected rate-limited auth to remain present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to exist")
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatalf("expected NextRetryAfter to be set")
	}
	remaining := time.Until(state.NextRetryAfter)
	if remaining < 55*time.Second || remaining > 65*time.Second {
		t.Fatalf("cooldown remaining = %v, want about 60s", remaining)
	}
	if !updated.NextRetryAfter.After(time.Now().Add(55 * time.Second)) {
		t.Fatalf("auth NextRetryAfter = %v, want about 60s in future", updated.NextRetryAfter)
	}
}

func TestManager_Execute_CyberPolicyRecordsCountCoolsAuthAndSwitchesAuth(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })
	writablePath := t.TempDir()
	t.Setenv("WRITABLE_PATH", writablePath)

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(1, 100*time.Millisecond, 2)

	executor := &authFallbackExecutor{
		id: "codex",
		executeErrors: map[string]error{
			"auth-001-cyber": &retryAfterStatusError{
				status:  http.StatusBadRequest,
				message: `{"error":{"code":"cyber_policy","message":"This chat was flagged for possible cybersecurity risk"}}`,
			},
		},
	}
	m.RegisterExecutor(executor)

	suffix := uuid.NewString()
	flagged := &Auth{ID: "auth-001-cyber-" + suffix, Provider: "codex"}
	healthy := &Auth{ID: "auth-999-healthy-cyber-" + suffix, Provider: "codex"}
	executor.executeErrors[flagged.ID] = executor.executeErrors["auth-001-cyber"]
	delete(executor.executeErrors, "auth-001-cyber")

	if _, errRegister := m.Register(context.Background(), flagged); errRegister != nil {
		t.Fatalf("register flagged auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), healthy); errRegister != nil {
		t.Fatalf("register healthy auth: %v", errRegister)
	}

	model := "cyber-policy-test-model-" + suffix
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(flagged.ID, "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(healthy.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(flagged.ID)
		reg.UnregisterClient(healthy.ID)
	})

	longInput := strings.Repeat("inspect sandbox target for CTF flow ", 180)
	payload := []byte(`{"model":"` + model + `","input":"` + longInput + `"}`)
	resp, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model, Payload: payload}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute error = %v, want nil", errExecute)
	}
	if string(resp.Payload) != healthy.ID {
		t.Fatalf("response payload = %q, want %q", string(resp.Payload), healthy.ID)
	}

	calls := executor.ExecuteCalls()
	wantCalls := []string{flagged.ID, healthy.ID}
	if len(calls) != len(wantCalls) {
		t.Fatalf("execute calls = %v, want %v", calls, wantCalls)
	}
	for i := range wantCalls {
		if calls[i] != wantCalls[i] {
			t.Fatalf("execute call %d auth = %q, want %q", i, calls[i], wantCalls[i])
		}
	}

	updated, ok := m.GetByID(flagged.ID)
	if !ok || updated == nil {
		t.Fatalf("expected flagged auth to remain present")
	}
	if got := authCyberPolicyTriggerCount(updated); got != 1 {
		t.Fatalf("cyber trigger count = %d, want 1", got)
	}
	if got, _ := updated.Metadata["cyber_policy_last_model"].(string); got != model {
		t.Fatalf("last model = %q, want %q", got, model)
	}
	wantPreview := string(payload[:cyberPolicyRequestPreviewMaxBytes])
	if got, _ := updated.Metadata["cyber_policy_last_request_preview"].(string); got != wantPreview {
		t.Fatalf("request preview length = %d, want preview length %d", len(got), len(wantPreview))
	}
	sum := sha256.Sum256(payload)
	wantHash := hex.EncodeToString(sum[:])
	if got, _ := updated.Metadata["cyber_policy_last_request_sha256"].(string); got != wantHash {
		t.Fatalf("request sha256 = %q, want %q", got, wantHash)
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to exist")
	}
	if !state.NextRetryAfter.After(time.Now().Add(4 * time.Minute)) {
		t.Fatalf("model NextRetryAfter = %v, want cyber cooldown", state.NextRetryAfter)
	}
	if state.Quota.Reason != "cyber_policy" {
		t.Fatalf("quota reason = %q, want cyber_policy", state.Quota.Reason)
	}
	if updated.Quota.Reason != "cyber_policy" {
		t.Fatalf("auth quota reason = %q, want cyber_policy", updated.Quota.Reason)
	}

	auditPath := filepath.Join(writablePath, "logs", cyberPolicyAuditFilename)
	auditData, errRead := os.ReadFile(auditPath)
	if errRead != nil {
		t.Fatalf("read cyber policy audit: %v", errRead)
	}
	lines := strings.Split(strings.TrimSpace(string(auditData)), "\n")
	if len(lines) != 1 {
		t.Fatalf("audit line count = %d, want 1", len(lines))
	}
	var audit map[string]any
	if errUnmarshal := json.Unmarshal([]byte(lines[0]), &audit); errUnmarshal != nil {
		t.Fatalf("unmarshal cyber policy audit: %v", errUnmarshal)
	}
	if got, _ := audit["auth_id"].(string); got != flagged.ID {
		t.Fatalf("audit auth_id = %q, want %q", got, flagged.ID)
	}
	if got, _ := audit["model"].(string); got != model {
		t.Fatalf("audit model = %q, want %q", got, model)
	}
	if got, _ := audit["request_sha256"].(string); got != wantHash {
		t.Fatalf("audit request_sha256 = %q, want %q", got, wantHash)
	}
	if got, _ := audit["request_preview"].(string); got != wantPreview {
		t.Fatalf("audit request_preview length = %d, want %d", len(got), cyberPolicyRequestPreviewMaxBytes)
	}
	if got, _ := audit["request_payload"].(string); got != string(payload) {
		t.Fatalf("audit request_payload length = %d, want %d", len(got), len(payload))
	}
}

func TestAuthPreferredBeforeUsesCyberPolicyTriggerCount(t *testing.T) {
	flagged := &Auth{
		ID:       "auth-flagged",
		Provider: "codex",
		Metadata: map[string]any{
			"cyber_policy_trigger_count": 3,
		},
	}
	clean := &Auth{ID: "auth-clean", Provider: "codex"}

	if authPreferredBefore(flagged, clean) {
		t.Fatalf("flagged auth ranked ahead of clean auth")
	}
	if !authPreferredBefore(clean, flagged) {
		t.Fatalf("clean auth should rank ahead of flagged auth")
	}
}

func TestManager_Execute_UsageLimitReachedSuspendsUntilResetAndSwitchesAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(1, 100*time.Millisecond, 2)

	resetAt := time.Now().Add(90 * time.Second).Unix()
	executor := &authFallbackExecutor{
		id: "codex",
		executeErrors: map[string]error{
			"auth-001-usage-limit": &retryAfterStatusError{
				status: http.StatusTooManyRequests,
				message: `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"plus","resets_at":` +
					strconv.FormatInt(resetAt, 10) + `,"eligible_promo":null,"resets_in_seconds":10}}`,
			},
		},
	}
	m.RegisterExecutor(executor)

	limited := &Auth{ID: "auth-001-usage-limit", Provider: "codex"}
	healthy := &Auth{ID: "auth-999-healthy-usage", Provider: "codex"}

	if _, errRegister := m.Register(context.Background(), limited); errRegister != nil {
		t.Fatalf("register limited auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), healthy); errRegister != nil {
		t.Fatalf("register healthy auth: %v", errRegister)
	}

	model := "gpt-5.4"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(limited.ID, "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(healthy.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(limited.ID)
		reg.UnregisterClient(healthy.ID)
	})

	resp, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute error = %v, want nil", errExecute)
	}
	if string(resp.Payload) != healthy.ID {
		t.Fatalf("response payload = %q, want %q", string(resp.Payload), healthy.ID)
	}

	updated, ok := m.GetByID(limited.ID)
	if !ok || updated == nil {
		t.Fatalf("expected limited auth to remain present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to exist")
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatalf("expected NextRetryAfter to be set")
	}
	remaining := time.Until(state.NextRetryAfter)
	if remaining < 80*time.Second || remaining > 95*time.Second {
		t.Fatalf("cooldown remaining = %v, want about 90s", remaining)
	}
	if !updated.NextRetryAfter.After(time.Now().Add(80 * time.Second)) {
		t.Fatalf("auth NextRetryAfter = %v, want about 90s in future", updated.NextRetryAfter)
	}
	if !updated.Unavailable {
		t.Fatalf("expected auth to be unavailable during reset window")
	}
}

func TestManager_Execute_UsageLimitReachedBlocksAuthAcrossModelsWithExistingHealthyState(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(1, 100*time.Millisecond, 2)

	executor := &authFallbackExecutor{id: "codex"}
	m.RegisterExecutor(executor)

	testKey := strings.ReplaceAll(t.Name(), "/", "-") + "-" + uuid.NewString()
	modelLimited := testKey + "-limited"
	modelHealthy := testKey + "-healthy"
	modelFollowup := testKey + "-followup"
	resetAt := time.Now().Add(90 * time.Second).Unix()
	limitedID := testKey + "-auth-001-usage-limit-cross-model"
	healthyID := testKey + "-auth-999-healthy-cross-model"

	limited := &Auth{
		ID:       limitedID,
		Provider: "codex",
		ModelStates: map[string]*ModelState{
			modelHealthy: {
				Status:    StatusActive,
				UpdatedAt: time.Now(),
			},
		},
	}
	healthy := &Auth{ID: healthyID, Provider: "codex"}

	if _, errRegister := m.Register(context.Background(), limited); errRegister != nil {
		t.Fatalf("register limited auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), healthy); errRegister != nil {
		t.Fatalf("register healthy auth: %v", errRegister)
	}

	reg := registry.GetGlobalRegistry()
	supportedModels := []*registry.ModelInfo{{ID: modelLimited}, {ID: modelHealthy}, {ID: modelFollowup}}
	reg.RegisterClient(limited.ID, "codex", supportedModels)
	reg.RegisterClient(healthy.ID, "codex", supportedModels)
	t.Cleanup(func() {
		reg.UnregisterClient(limited.ID)
		reg.UnregisterClient(healthy.ID)
	})

	m.MarkResult(context.Background(), Result{
		AuthID:   limited.ID,
		Provider: "codex",
		Model:    modelLimited,
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusTooManyRequests,
			Message: `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_at":` +
				strconv.FormatInt(resetAt, 10) + `}}`,
		},
	})

	resp, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: modelFollowup}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute error = %v, want nil", errExecute)
	}
	if string(resp.Payload) != healthy.ID {
		t.Fatalf("response payload = %q, want %q", string(resp.Payload), healthy.ID)
	}

	calls := executor.ExecuteCalls()
	wantCalls := []string{healthy.ID}
	if len(calls) != len(wantCalls) {
		t.Fatalf("execute calls = %v, want %v", calls, wantCalls)
	}
	for i := range wantCalls {
		if calls[i] != wantCalls[i] {
			t.Fatalf("execute call %d auth = %q, want %q", i, calls[i], wantCalls[i])
		}
	}
}

func TestManager_Execute_SelectedModelAtCapacitySwitchesAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(1, 100*time.Millisecond, 2)

	testKey := strings.ReplaceAll(t.Name(), "/", "-")
	capacityAuthID := testKey + "-auth-001-capacity"
	healthyAuthID := testKey + "-auth-999-healthy-capacity"
	executor := &authFallbackExecutor{
		id: "codex",
		executeErrors: map[string]error{
			capacityAuthID: &Error{
				HTTPStatus: http.StatusTooManyRequests,
				Message:    "Selected model is at capacity. Please try a different model.",
			},
		},
	}
	m.RegisterExecutor(executor)

	capacityAuth := &Auth{ID: capacityAuthID, Provider: "codex"}
	healthy := &Auth{ID: healthyAuthID, Provider: "codex"}

	if _, errRegister := m.Register(context.Background(), capacityAuth); errRegister != nil {
		t.Fatalf("register capacity auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), healthy); errRegister != nil {
		t.Fatalf("register healthy auth: %v", errRegister)
	}

	model := testKey + "-model"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(capacityAuth.ID, "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(healthy.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(capacityAuth.ID)
		reg.UnregisterClient(healthy.ID)
	})

	resp, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute error = %v, want nil", errExecute)
	}
	if string(resp.Payload) != healthy.ID {
		t.Fatalf("response payload = %q, want %q", string(resp.Payload), healthy.ID)
	}

	calls := executor.ExecuteCalls()
	wantCalls := []string{capacityAuth.ID, healthy.ID}
	if len(calls) != len(wantCalls) {
		t.Fatalf("execute calls = %v, want %v", calls, wantCalls)
	}
	for i := range wantCalls {
		if calls[i] != wantCalls[i] {
			t.Fatalf("execute call %d auth = %q, want %q", i, calls[i], wantCalls[i])
		}
	}
}

func TestManager_Execute_CurrentModelUnavailableSwitchesAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(1, 100*time.Millisecond, 2)

	executor := &authFallbackExecutor{
		id: "codex",
		executeErrors: map[string]error{
			"auth-001-unavailable": &Error{
				HTTPStatus: http.StatusTooManyRequests,
				Message:    "The requested model is currently unavailable. Please switch model.",
			},
		},
	}
	m.RegisterExecutor(executor)

	unavailableAuth := &Auth{ID: "auth-001-unavailable", Provider: "codex"}
	healthy := &Auth{ID: "auth-999-healthy-unavailable", Provider: "codex"}

	if _, errRegister := m.Register(context.Background(), unavailableAuth); errRegister != nil {
		t.Fatalf("register unavailable auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), healthy); errRegister != nil {
		t.Fatalf("register healthy auth: %v", errRegister)
	}

	model := "gpt-5.4"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(unavailableAuth.ID, "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(healthy.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(unavailableAuth.ID)
		reg.UnregisterClient(healthy.ID)
	})

	resp, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute error = %v, want nil", errExecute)
	}
	if string(resp.Payload) != healthy.ID {
		t.Fatalf("response payload = %q, want %q", string(resp.Payload), healthy.ID)
	}

	calls := executor.ExecuteCalls()
	wantCalls := []string{unavailableAuth.ID, healthy.ID}
	if len(calls) != len(wantCalls) {
		t.Fatalf("execute calls = %v, want %v", calls, wantCalls)
	}
	for i := range wantCalls {
		if calls[i] != wantCalls[i] {
			t.Fatalf("execute call %d auth = %q, want %q", i, calls[i], wantCalls[i])
		}
	}
}

func TestManager_ExecuteStream_SelectedModelAtCapacitySwitchesAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(1, 100*time.Millisecond, 2)

	executor := &authFallbackExecutor{
		id: "codex",
		streamFirstErrors: map[string]error{
			"auth-001-capacity-stream": &Error{
				HTTPStatus: http.StatusTooManyRequests,
				Message:    "Selected model is at capacity. Please try a different model.",
			},
		},
	}
	m.RegisterExecutor(executor)

	capacityAuth := &Auth{ID: "auth-001-capacity-stream", Provider: "codex"}
	healthy := &Auth{ID: "auth-999-healthy-capacity-stream", Provider: "codex"}

	if _, errRegister := m.Register(context.Background(), capacityAuth); errRegister != nil {
		t.Fatalf("register capacity auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), healthy); errRegister != nil {
		t.Fatalf("register healthy auth: %v", errRegister)
	}

	model := "gpt-5.4"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(capacityAuth.ID, "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(healthy.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(capacityAuth.ID)
		reg.UnregisterClient(healthy.ID)
	})

	streamResult, errExecute := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute stream error = %v, want nil", errExecute)
	}
	var payload []byte
	for chunk := range streamResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v, want nil", chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	if string(payload) != healthy.ID {
		t.Fatalf("stream payload = %q, want %q", string(payload), healthy.ID)
	}

	calls := executor.StreamCalls()
	wantCalls := []string{capacityAuth.ID, healthy.ID}
	if len(calls) != len(wantCalls) {
		t.Fatalf("stream calls = %v, want %v", calls, wantCalls)
	}
	for i := range wantCalls {
		if calls[i] != wantCalls[i] {
			t.Fatalf("stream call %d auth = %q, want %q", i, calls[i], wantCalls[i])
		}
	}
}

func TestManager_ExecuteStream_CurrentModelUnavailableSwitchesAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(1, 100*time.Millisecond, 2)

	executor := &authFallbackExecutor{
		id: "codex",
		streamFirstErrors: map[string]error{
			"auth-001-unavailable-stream": &Error{
				HTTPStatus: http.StatusTooManyRequests,
				Message:    "The requested model is currently unavailable. Please switch model.",
			},
		},
	}
	m.RegisterExecutor(executor)

	unavailableAuth := &Auth{ID: "auth-001-unavailable-stream", Provider: "codex"}
	healthy := &Auth{ID: "auth-999-healthy-unavailable-stream", Provider: "codex"}

	if _, errRegister := m.Register(context.Background(), unavailableAuth); errRegister != nil {
		t.Fatalf("register unavailable auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), healthy); errRegister != nil {
		t.Fatalf("register healthy auth: %v", errRegister)
	}

	model := "gpt-5.4"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(unavailableAuth.ID, "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(healthy.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(unavailableAuth.ID)
		reg.UnregisterClient(healthy.ID)
	})

	streamResult, errExecute := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute stream error = %v, want nil", errExecute)
	}
	var payload []byte
	for chunk := range streamResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v, want nil", chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	if string(payload) != healthy.ID {
		t.Fatalf("stream payload = %q, want %q", string(payload), healthy.ID)
	}

	calls := executor.StreamCalls()
	wantCalls := []string{unavailableAuth.ID, healthy.ID}
	if len(calls) != len(wantCalls) {
		t.Fatalf("stream calls = %v, want %v", calls, wantCalls)
	}
	for i := range wantCalls {
		if calls[i] != wantCalls[i] {
			t.Fatalf("stream call %d auth = %q, want %q", i, calls[i], wantCalls[i])
		}
	}
}

func TestManager_MarkResult_RequestScopedNotFoundDoesNotCooldownAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "openai",
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "gpt-4.1"
	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    model,
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusNotFound,
			Message:    requestScopedNotFoundMessage,
		},
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if updated.Unavailable {
		t.Fatalf("expected request-scoped 404 to keep auth available")
	}
	if !updated.NextRetryAfter.IsZero() {
		t.Fatalf("expected request-scoped 404 to keep auth cooldown unset, got %v", updated.NextRetryAfter)
	}
	if state := updated.ModelStates[model]; state != nil {
		t.Fatalf("expected request-scoped 404 to avoid model cooldown state, got %#v", state)
	}
}

func TestManager_RequestScopedNotFoundStopsRetryWithoutSuspendingAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "openai",
		executeErrors: map[string]error{
			"aa-bad-auth": &Error{
				HTTPStatus: http.StatusNotFound,
				Message:    requestScopedNotFoundMessage,
			},
		},
	}
	m.RegisterExecutor(executor)

	model := "gpt-4.1"
	badAuth := &Auth{ID: "aa-bad-auth", Provider: "openai"}
	goodAuth := &Auth{ID: "bb-good-auth", Provider: "openai"}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(badAuth.ID, "openai", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(goodAuth.ID, "openai", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(badAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})

	if _, errRegister := m.Register(context.Background(), badAuth); errRegister != nil {
		t.Fatalf("register bad auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), goodAuth); errRegister != nil {
		t.Fatalf("register good auth: %v", errRegister)
	}

	_, errExecute := m.Execute(context.Background(), []string{"openai"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute == nil {
		t.Fatal("expected request-scoped not-found error")
	}
	errResult, ok := errExecute.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", errExecute)
	}
	if errResult.HTTPStatus != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", errResult.HTTPStatus, http.StatusNotFound)
	}
	if errResult.Message != requestScopedNotFoundMessage {
		t.Fatalf("message = %q, want %q", errResult.Message, requestScopedNotFoundMessage)
	}

	got := executor.ExecuteCalls()
	want := []string{badAuth.ID}
	if len(got) != len(want) {
		t.Fatalf("execute calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("execute call %d auth = %q, want %q", i, got[i], want[i])
		}
	}

	updatedBad, ok := m.GetByID(badAuth.ID)
	if !ok || updatedBad == nil {
		t.Fatalf("expected bad auth to remain registered")
	}
	if updatedBad.Unavailable {
		t.Fatalf("expected request-scoped 404 to keep bad auth available")
	}
	if !updatedBad.NextRetryAfter.IsZero() {
		t.Fatalf("expected request-scoped 404 to keep bad auth cooldown unset, got %v", updatedBad.NextRetryAfter)
	}
	if state := updatedBad.ModelStates[model]; state != nil {
		t.Fatalf("expected request-scoped 404 to avoid bad auth model cooldown state, got %#v", state)
	}
}

func TestMarkResult_UsageLimitIgnoresCooldownDisabled(t *testing.T) {
	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-ul",
		Provider: "codex",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
		ModelStates: make(map[string]*ModelState),
	}
	_, _ = m.Register(context.Background(), auth)

	retryAfter := 2 * time.Hour
	m.MarkResult(context.Background(), Result{
		AuthID:     "auth-ul",
		Provider:   "codex",
		Model:      "gpt-4",
		Success:    false,
		RetryAfter: &retryAfter,
		Error: &Error{
			Message:    `{"error":{"type":"usage_limit_reached","resets_in_seconds":7200}}`,
			HTTPStatus: 429,
		},
	})

	got, ok := m.GetByID("auth-ul")
	if !ok {
		t.Fatal("auth not found after MarkResult")
	}
	if !got.Quota.Exceeded {
		t.Error("expected Quota.Exceeded = true")
	}
	if got.Quota.Reason != "usage_limit" {
		t.Errorf("expected Quota.Reason = usage_limit, got %q", got.Quota.Reason)
	}
	if got.NextRetryAfter.IsZero() {
		t.Error("expected NextRetryAfter to be set even though cooling is disabled")
	}
	if state, exists := got.ModelStates["gpt-4"]; !exists || !state.Quota.Exceeded {
		t.Error("expected model state gpt-4 to have Quota.Exceeded = true")
	}
}

func TestMarkResult_PlainRateLimitRespectsDisabledCooling(t *testing.T) {
	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-rl",
		Provider: "codex",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
		ModelStates: make(map[string]*ModelState),
	}
	_, _ = m.Register(context.Background(), auth)

	retryAfter := 60 * time.Second
	m.MarkResult(context.Background(), Result{
		AuthID:     "auth-rl",
		Provider:   "codex",
		Model:      "gpt-4",
		Success:    false,
		RetryAfter: &retryAfter,
		Error: &Error{
			Message:    "rate limit exceeded",
			HTTPStatus: 429,
		},
	})

	got, ok := m.GetByID("auth-rl")
	if !ok {
		t.Fatal("auth not found")
	}
	if !got.NextRetryAfter.IsZero() {
		t.Errorf("expected NextRetryAfter to be zero for plain rate limit with cooling disabled, got %v", got.NextRetryAfter)
	}
}

func TestApplyAuthFailureState_UsageLimitIgnoresDisabledCooling(t *testing.T) {
	auth := &Auth{
		ID:       "auth-af",
		Provider: "codex",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	retryAfter := 3 * time.Hour
	now := time.Now()
	applyAuthFailureState(auth, &Error{
		Message:    `{"error":{"type":"usage_limit_reached","resets_in_seconds":10800}}`,
		HTTPStatus: 429,
	}, &retryAfter, now)

	if auth.NextRetryAfter.IsZero() {
		t.Error("expected NextRetryAfter to be set for usage_limit even with cooling disabled")
	}
	if auth.Quota.Reason != "usage_limit" {
		t.Errorf("expected Quota.Reason = usage_limit, got %q", auth.Quota.Reason)
	}
	if !auth.Quota.Exceeded {
		t.Error("expected Quota.Exceeded = true")
	}
}

func TestApplyAuthFailureState_PlainRateLimitRespectsDisabledCooling(t *testing.T) {
	auth := &Auth{
		ID:       "auth-af2",
		Provider: "codex",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	retryAfter := 60 * time.Second
	now := time.Now()
	applyAuthFailureState(auth, &Error{
		Message:    "rate limit exceeded",
		HTTPStatus: 429,
	}, &retryAfter, now)

	if !auth.NextRetryAfter.IsZero() {
		t.Error("expected NextRetryAfter to be zero for plain rate limit with cooling disabled")
	}
}

func TestQuotaPersistence_SurvivesRestart(t *testing.T) {
	persister := &mockPersister{written: make(map[string]QuotaState)}
	m := NewManager(nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartQuotaFlusher(ctx, persister)

	auth := &Auth{
		ID:          "auth-persist",
		Provider:    "codex",
		Metadata:    map[string]any{},
		ModelStates: make(map[string]*ModelState),
	}
	_, _ = m.Register(context.Background(), auth)

	// Trigger usage_limit 429
	retryAfter := 2 * time.Hour
	m.MarkResult(context.Background(), Result{
		AuthID:     "auth-persist",
		Provider:   "codex",
		Model:      "gpt-4",
		Success:    false,
		RetryAfter: &retryAfter,
		Error: &Error{
			Message:    `{"error":{"type":"usage_limit_reached","resets_in_seconds":7200}}`,
			HTTPStatus: 429,
		},
	})

	// Flush to persister
	if m.quotaFlusher != nil {
		m.quotaFlusher.flushOnce()
	}

	// Verify persisted
	q, ok := persister.getWritten("auth-persist")
	if !ok {
		t.Fatal("expected quota to be persisted")
	}
	if !q.Exceeded {
		t.Error("persisted quota should be exceeded")
	}
	if q.Reason != "usage_limit" {
		t.Errorf("persisted reason should be usage_limit, got %q", q.Reason)
	}
	if q.NextRecoverAt.IsZero() {
		t.Error("persisted NextRecoverAt should not be zero")
	}

	// Simulate "restart" — new Manager, load auth with persisted quota
	m2 := NewManager(nil, nil, nil)
	auth2 := &Auth{
		ID:             "auth-persist",
		Provider:       "codex",
		Quota:          q,
		Unavailable:    true,
		NextRetryAfter: q.NextRecoverAt,
	}
	_, _ = m2.Register(context.Background(), auth2)

	got, ok := m2.GetByID("auth-persist")
	if !ok {
		t.Fatal("auth not found in new manager")
	}
	if !got.Unavailable {
		t.Error("auth should be unavailable after simulated restart")
	}
	if !got.Quota.Exceeded {
		t.Error("auth quota should be exceeded after simulated restart")
	}
	if got.Quota.Reason != "usage_limit" {
		t.Errorf("expected usage_limit, got %q", got.Quota.Reason)
	}
}
