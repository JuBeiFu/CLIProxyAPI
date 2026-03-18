package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

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
	wait, shouldRetry := m.shouldRetryAfterError(&Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"}, 0, []string{"claude"}, model, maxWait)
	if shouldRetry {
		t.Fatalf("expected shouldRetry=false for request_retry=0, got true (wait=%v)", wait)
	}

	auth.Metadata["request_retry"] = float64(1)
	if _, errUpdate := m.Update(context.Background(), auth); errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}

	wait, shouldRetry = m.shouldRetryAfterError(&Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"}, 0, []string{"claude"}, model, maxWait)
	if !shouldRetry {
		t.Fatalf("expected shouldRetry=true for request_retry=1, got false")
	}
	if wait <= 0 {
		t.Fatalf("expected wait > 0, got %v", wait)
	}

	_, shouldRetry = m.shouldRetryAfterError(&Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"}, 1, []string{"claude"}, model, maxWait)
	if shouldRetry {
		t.Fatalf("expected shouldRetry=false on attempt=1 for request_retry=1, got true")
	}

	wait, shouldRetry = m.shouldRetryAfterError(errors.New("stream disconnected before completion"), 0, []string{"claude"}, model, maxWait)
	if shouldRetry {
		t.Fatalf("expected shouldRetry=false for non-switchable stream error, got true (wait=%v)", wait)
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
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"}
}

func (e *credentialRetryLimitExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.recordCall()
	return nil, &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"}
}

func (e *credentialRetryLimitExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *credentialRetryLimitExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.recordCall()
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"}
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

type invalidRequestExecutor struct {
	id  string
	err error

	mu    sync.Mutex
	calls int
}

func (e *invalidRequestExecutor) Identifier() string {
	return e.id
}

func (e *invalidRequestExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.recordCall()
	return cliproxyexecutor.Response{}, e.err
}

func (e *invalidRequestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.recordCall()
	return nil, e.err
}

func (e *invalidRequestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *invalidRequestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.recordCall()
	return cliproxyexecutor.Response{}, e.err
}

func (e *invalidRequestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *invalidRequestExecutor) recordCall() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
}

func (e *invalidRequestExecutor) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

type sameAuthRetryExecutor struct {
	id  string
	err error

	mu       sync.Mutex
	calls    int
	authSeen map[string]int
}

func (e *sameAuthRetryExecutor) Identifier() string {
	return e.id
}

func (e *sameAuthRetryExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.recordCall(auth)
	return cliproxyexecutor.Response{}, e.err
}

func (e *sameAuthRetryExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.recordCall(auth)
	return nil, e.err
}

func (e *sameAuthRetryExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *sameAuthRetryExecutor) CountTokens(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.recordCall(auth)
	return cliproxyexecutor.Response{}, e.err
}

func (e *sameAuthRetryExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *sameAuthRetryExecutor) recordCall(auth *Auth) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if e.authSeen == nil {
		e.authSeen = make(map[string]int)
	}
	if auth != nil {
		e.authSeen[auth.ID]++
	}
}

func (e *sameAuthRetryExecutor) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func (e *sameAuthRetryExecutor) DistinctAuths() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.authSeen)
}

func newSameAuthRetryTestManager(t *testing.T, retry int, err error) (*Manager, *sameAuthRetryExecutor) {
	t.Helper()

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(retry, 30*time.Second, 0)

	executor := &sameAuthRetryExecutor{id: "claude", err: err}
	m.RegisterExecutor(executor)

	baseID := uuid.NewString()
	auth1 := &Auth{ID: baseID + "-auth-1", Provider: "claude"}
	auth2 := &Auth{ID: baseID + "-auth-2", Provider: "claude"}

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

type executionTimeoutExecutor struct {
	id         string
	callCounts map[string]int
	mu         sync.Mutex
}

type contextCauseStatusError struct {
	status int
	msg    string
}

func (e *contextCauseStatusError) Error() string   { return e.msg }
func (e *contextCauseStatusError) StatusCode() int { return e.status }

func (e *executionTimeoutExecutor) Identifier() string {
	return e.id
}

func (e *executionTimeoutExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = auth
	_ = req
	_ = opts
	return cliproxyexecutor.Response{}, e.waitForTimeout(ctx, "execute")
}

func (e *executionTimeoutExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	_ = auth
	_ = req
	_ = opts
	if err := e.waitForTimeout(ctx, "stream"); err != nil {
		return nil, err
	}
	return nil, nil
}

func (e *executionTimeoutExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *executionTimeoutExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = auth
	_ = req
	_ = opts
	return cliproxyexecutor.Response{}, e.waitForTimeout(ctx, "count")
}

func (e *executionTimeoutExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *executionTimeoutExecutor) waitForTimeout(ctx context.Context, key string) error {
	e.mu.Lock()
	if e.callCounts == nil {
		e.callCounts = make(map[string]int)
	}
	e.callCounts[key]++
	e.mu.Unlock()
	<-ctx.Done()
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return ctx.Err()
}

func (e *executionTimeoutExecutor) Calls(key string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.callCounts[key]
}

type bootstrapTimeoutExecutor struct {
	id string

	mu    sync.Mutex
	calls int
}

func (e *bootstrapTimeoutExecutor) Identifier() string {
	return e.id
}

func (e *bootstrapTimeoutExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusUnauthorized, Message: "not used"}
}

func (e *bootstrapTimeoutExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	_ = ctx
	_ = auth
	_ = req
	_ = opts
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	ch := make(chan cliproxyexecutor.StreamChunk)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (e *bootstrapTimeoutExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *bootstrapTimeoutExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusUnauthorized, Message: "not used"}
}

func (e *bootstrapTimeoutExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *bootstrapTimeoutExecutor) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

type streamContextCancellationExecutor struct {
	id    string
	delay time.Duration

	mu    sync.Mutex
	calls int
}

func (e *streamContextCancellationExecutor) Identifier() string {
	return e.id
}

func (e *streamContextCancellationExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.recordCall()
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusUnauthorized, Message: "not used"}
}

func (e *streamContextCancellationExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	_ = auth
	_ = req
	_ = opts
	e.recordCall()
	out := make(chan cliproxyexecutor.StreamChunk, 1)
	go func() {
		defer close(out)
		select {
		case <-ctx.Done():
			out <- cliproxyexecutor.StreamChunk{Err: ctx.Err()}
		case <-time.After(e.delay):
			out <- cliproxyexecutor.StreamChunk{Payload: []byte("ok")}
		}
	}()
	return &cliproxyexecutor.StreamResult{Chunks: out}, nil
}

func (e *streamContextCancellationExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *streamContextCancellationExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.recordCall()
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusUnauthorized, Message: "not used"}
}

func (e *streamContextCancellationExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *streamContextCancellationExecutor) recordCall() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
}

func (e *streamContextCancellationExecutor) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
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

func newInvalidRequestTestManager(t *testing.T, err error) (*Manager, *invalidRequestExecutor) {
	t.Helper()

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 30*time.Second, 0)

	executor := &invalidRequestExecutor{id: "claude", err: err}
	m.RegisterExecutor(executor)

	baseID := uuid.NewString()
	auth1 := &Auth{ID: baseID + "-auth-1", Provider: "claude"}
	auth2 := &Auth{ID: baseID + "-auth-2", Provider: "claude"}

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

func TestManager_NonSwitchableErrors_RetryCurrentAuthOnly(t *testing.T) {
	prevDelay := sameAuthRetryDelayFunc
	sameAuthRetryDelayFunc = func() time.Duration { return 0 }
	t.Cleanup(func() { sameAuthRetryDelayFunc = prevDelay })

	request := cliproxyexecutor.Request{Model: "test-model"}
	invocations := []struct {
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

	for _, invocation := range invocations {
		invocation := invocation
		t.Run(invocation.name, func(t *testing.T) {
			manager, executor := newSameAuthRetryTestManager(t, 2, errors.New("stream disconnected before completion"))
			errInvoke := invocation.invoke(manager)
			if errInvoke == nil {
				t.Fatalf("expected non-switchable error")
			}
			if errInvoke.Error() != "stream disconnected before completion" {
				t.Fatalf("unexpected error: %v", errInvoke)
			}
			if calls := executor.Calls(); calls != 3 {
				t.Fatalf("expected 3 attempts on the same auth, got %d", calls)
			}
			if distinct := executor.DistinctAuths(); distinct != 1 {
				t.Fatalf("expected retries to stay on one auth, got %d auths", distinct)
			}
		})
	}
}

func TestNextSameAuthRetryDelay_Range(t *testing.T) {
	for i := 0; i < 32; i++ {
		wait := nextSameAuthRetryDelay()
		if wait < sameAuthRetryMinWait || wait > sameAuthRetryMaxWait {
			t.Fatalf("retry wait %v out of range [%v, %v]", wait, sameAuthRetryMinWait, sameAuthRetryMaxWait)
		}
	}
}

func TestManager_InvalidRequestErrors_StopCrossCredentialRetries(t *testing.T) {
	request := cliproxyexecutor.Request{Model: "test-model"}
	testCases := []struct {
		name string
		err  error
	}{
		{
			name: "unsupported_parameter_detail",
			err:  &Error{HTTPStatus: http.StatusBadRequest, Message: `{"detail":"Unsupported parameter: metadata"}`},
		},
		{
			name: "unknown_parameter_detail",
			err:  &Error{HTTPStatus: http.StatusUnprocessableEntity, Message: `{"detail":"Unknown parameter: metadata"}`},
		},
	}
	invocations := []struct {
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
			for _, invocation := range invocations {
				invocation := invocation
				t.Run(invocation.name, func(t *testing.T) {
					manager, executor := newInvalidRequestTestManager(t, tc.err)
					errInvoke := invocation.invoke(manager)
					if errInvoke == nil {
						t.Fatalf("expected invalid request error")
					}
					if errInvoke.Error() != tc.err.Error() {
						t.Fatalf("expected error %q, got %q", tc.err.Error(), errInvoke.Error())
					}
					if calls := executor.Calls(); calls != 1 {
						t.Fatalf("expected 1 executor call for invalid request, got %d", calls)
					}
				})
			}
		})
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

func TestManager_ExecuteStream_ReturnsBootstrapTimeoutWhenNoAlternativeCredential(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 0, 1)

	authID := uuid.NewString() + "-auth"
	executor := &bootstrapTimeoutExecutor{id: "claude"}
	m.RegisterExecutor(executor)

	auth := &Auth{ID: authID, Provider: "claude", Status: StatusActive}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient(authID)
	})

	ctx := context.WithValue(context.Background(), streamBootstrapTimeoutContextKey{}, 50*time.Millisecond)
	_, err := m.ExecuteStream(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("expected bootstrap timeout error")
	}
	var authErr *Error
	if !errors.As(err, &authErr) {
		t.Fatalf("error = %T %v, want *Error", err, err)
	}
	if authErr.Code != "stream_bootstrap_timeout" {
		t.Fatalf("code = %q, want %q", authErr.Code, "stream_bootstrap_timeout")
	}
	if authErr.StatusCode() != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want %d", authErr.StatusCode(), http.StatusRequestTimeout)
	}
	if calls := executor.Calls(); calls != 1 {
		t.Fatalf("expected 1 stream attempt, got %d", calls)
	}
}

func TestManager_Execute_ReturnsExecuteTimeout(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetConfig(&internalconfig.Config{ExecuteTimeoutSeconds: 1})
	executor := &executionTimeoutExecutor{id: "claude"}
	m.RegisterExecutor(executor)
	authID := uuid.NewString() + "-auth"
	if _, err := m.Register(context.Background(), &Auth{ID: authID, Provider: "claude", Status: StatusActive}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	start := time.Now()
	_, err := m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("expected execute timeout")
	}
	var authErr *Error
	if !errors.As(err, &authErr) {
		t.Fatalf("error = %T %v, want *Error", err, err)
	}
	if authErr.Code != "execute_timeout" {
		t.Fatalf("code = %q, want %q", authErr.Code, "execute_timeout")
	}
	if authErr.StatusCode() != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want %d", authErr.StatusCode(), http.StatusRequestTimeout)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("elapsed = %v, want about 1s", elapsed)
	}
	if calls := executor.Calls("execute"); calls != 1 {
		t.Fatalf("execute calls = %d, want 1", calls)
	}
}

func TestManager_ExecuteCount_ReturnsCountTimeout(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetConfig(&internalconfig.Config{CountTimeoutSeconds: 1})
	executor := &executionTimeoutExecutor{id: "claude"}
	m.RegisterExecutor(executor)
	authID := uuid.NewString() + "-auth"
	if _, err := m.Register(context.Background(), &Auth{ID: authID, Provider: "claude", Status: StatusActive}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	_, err := m.ExecuteCount(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("expected count timeout")
	}
	var authErr *Error
	if !errors.As(err, &authErr) {
		t.Fatalf("error = %T %v, want *Error", err, err)
	}
	if authErr.Code != "count_timeout" {
		t.Fatalf("code = %q, want %q", authErr.Code, "count_timeout")
	}
	if authErr.StatusCode() != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want %d", authErr.StatusCode(), http.StatusRequestTimeout)
	}
	if calls := executor.Calls("count"); calls != 1 {
		t.Fatalf("count calls = %d, want 1", calls)
	}
}

func TestManager_ExecuteStream_FallsBackOnStreamConnectTimeout(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetConfig(&internalconfig.Config{StreamConnectTimeoutSeconds: 1})
	executor := &executionTimeoutExecutor{id: "claude"}
	m.RegisterExecutor(executor)
	auth1 := uuid.NewString() + "-auth-1"
	auth2 := uuid.NewString() + "-auth-2"
	if _, err := m.Register(context.Background(), &Auth{ID: auth1, Provider: "claude", Status: StatusActive}); err != nil {
		t.Fatalf("register auth1: %v", err)
	}
	if _, err := m.Register(context.Background(), &Auth{ID: auth2, Provider: "claude", Status: StatusActive}); err != nil {
		t.Fatalf("register auth2: %v", err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth1, "claude", []*registry.ModelInfo{{ID: "test-model"}})
	reg.RegisterClient(auth2, "claude", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth1)
		reg.UnregisterClient(auth2)
	})

	_, err := m.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("expected stream connect timeout")
	}
	var authErr *Error
	if !errors.As(err, &authErr) {
		t.Fatalf("error = %T %v, want *Error", err, err)
	}
	if authErr.Code != "stream_connect_timeout" {
		t.Fatalf("code = %q, want %q", authErr.Code, "stream_connect_timeout")
	}
	if authErr.StatusCode() != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want %d", authErr.StatusCode(), http.StatusRequestTimeout)
	}
	if calls := executor.Calls("stream"); calls != 2 {
		t.Fatalf("stream calls = %d, want 2", calls)
	}
}

func TestManager_ExecuteStream_DoesNotCancelStreamAfterSuccessfulConnect(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetConfig(&internalconfig.Config{StreamConnectTimeoutSeconds: 1})
	executor := &streamContextCancellationExecutor{id: "claude", delay: 100 * time.Millisecond}
	m.RegisterExecutor(executor)
	authID := uuid.NewString() + "-auth"
	if _, err := m.Register(context.Background(), &Auth{ID: authID, Provider: "claude", Status: StatusActive}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	streamResult, err := m.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("execute stream: %v", err)
	}
	var payload []byte
	for chunk := range streamResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	if string(payload) != "ok" {
		t.Fatalf("payload = %q, want %q", string(payload), "ok")
	}
	if calls := executor.Calls(); calls != 1 {
		t.Fatalf("stream calls = %d, want 1", calls)
	}
}
