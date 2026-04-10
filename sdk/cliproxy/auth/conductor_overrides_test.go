package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
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
		t.Fatalf("expected outer shouldRetry=false for stream disconnect, got true (wait=%v)", wait)
	}
}

func TestShouldRetryAcrossAuths_DoesNotRetryTimeouts(t *testing.T) {
	if shouldRetryAcrossAuths(&Error{HTTPStatus: http.StatusRequestTimeout, Message: "stream bootstrap timeout"}) {
		t.Fatal("expected request timeout to stop cross-auth rotation")
	}
	if !shouldRetryAcrossAuths(&Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"}) {
		t.Fatal("expected 429 to remain cross-auth retryable")
	}
	if !shouldRetryAcrossAuths(&Error{HTTPStatus: http.StatusBadGateway, Message: "bad gateway"}) {
		t.Fatal("expected 502 to remain cross-auth retryable")
	}
	if !shouldRetryAcrossAuths(&Error{HTTPStatus: http.StatusRequestTimeout, Message: "stream disconnected before completion"}) {
		t.Fatal("expected stream disconnect to be cross-auth retryable")
	}
}

func TestManager_ShouldRetryAfterError_DoesNotRetryCompactExecuteTimeout(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(2, 30*time.Second, 6)
	err := &Error{
		HTTPStatus: http.StatusRequestTimeout,
		Code:       "execute_timeout",
		Message:    `Post "https://chatgpt.com/backend-api/codex/responses/compact": context deadline exceeded`,
	}
	wait, shouldRetry := m.shouldRetryAfterError(err, 0, []string{"codex"}, "gpt-5.4", 6*time.Second)
	if shouldRetry {
		t.Fatalf("expected compact execute timeout to stop outer retry, got wait=%v", wait)
	}
}

func TestCompactExecuteAttemptTimeout(t *testing.T) {
	if got := CompactExecuteAttemptTimeout(&internalconfig.Config{ExecuteTimeoutSeconds: 60}); got != 0 {
		t.Fatalf("timeout = %v, want %v", got, 0*time.Second)
	}
	if got := CompactExecuteAttemptTimeout(&internalconfig.Config{ExecuteTimeoutSeconds: 120}); got != 0 {
		t.Fatalf("timeout = %v, want %v", got, 0*time.Second)
	}
	if got := CompactExecuteAttemptTimeout(&internalconfig.Config{ExecuteTimeoutSeconds: 400}); got != 0 {
		t.Fatalf("timeout = %v, want %v", got, 0*time.Second)
	}
}

func TestWithExecuteAttemptTimeout_CanDisableDefaultTimeout(t *testing.T) {
	ctx := WithExecuteAttemptTimeout(context.Background(), 0)
	if got := executeAttemptTimeout(ctx); got != 0 {
		t.Fatalf("executeAttemptTimeout(ctx) = %v, want 0", got)
	}
}

type credentialRetryLimitExecutor struct {
	id string

	mu    sync.Mutex
	calls int
	err   error
}

func (e *credentialRetryLimitExecutor) Identifier() string {
	return e.id
}

func (e *credentialRetryLimitExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.recordCall()
	if e.err != nil {
		return cliproxyexecutor.Response{}, e.err
	}
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"}
}

func (e *credentialRetryLimitExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.recordCall()
	if e.err != nil {
		return nil, e.err
	}
	return nil, &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"}
}

func (e *credentialRetryLimitExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *credentialRetryLimitExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.recordCall()
	if e.err != nil {
		return cliproxyexecutor.Response{}, e.err
	}
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

	mu        sync.Mutex
	calls     int
	authSeen  map[string]int
	modelSeen []string
}

func (e *sameAuthRetryExecutor) Identifier() string {
	return e.id
}

func (e *sameAuthRetryExecutor) Execute(_ context.Context, auth *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.recordCall(auth, req.Model)
	return cliproxyexecutor.Response{}, e.err
}

func (e *sameAuthRetryExecutor) ExecuteStream(_ context.Context, auth *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.recordCall(auth, req.Model)
	return nil, e.err
}

func (e *sameAuthRetryExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *sameAuthRetryExecutor) CountTokens(_ context.Context, auth *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.recordCall(auth, req.Model)
	return cliproxyexecutor.Response{}, e.err
}

func (e *sameAuthRetryExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *sameAuthRetryExecutor) recordCall(auth *Auth, model string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if e.authSeen == nil {
		e.authSeen = make(map[string]int)
	}
	if model != "" {
		e.modelSeen = append(e.modelSeen, model)
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

func (e *sameAuthRetryExecutor) Models() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.modelSeen))
	copy(out, e.modelSeen)
	return out
}

func newSameAuthRetryTestManager(t *testing.T, retry int, err error) (*Manager, *sameAuthRetryExecutor) {
	t.Helper()

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 30*time.Second, 0)

	executor := &sameAuthRetryExecutor{id: "claude", err: err}
	m.RegisterExecutor(executor)

	baseID := uuid.NewString()
	authMeta := map[string]any{"same_auth_retry": float64(retry)}
	auth1 := &Auth{ID: baseID + "-auth-1", Provider: "claude", Metadata: authMeta}
	auth2 := &Auth{ID: baseID + "-auth-2", Provider: "claude", Metadata: map[string]any{"same_auth_retry": float64(retry)}}

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

type bootstrapEarlyDisconnectExecutor struct {
	id string

	mu    sync.Mutex
	calls int
}

func (e *bootstrapEarlyDisconnectExecutor) Identifier() string {
	return e.id
}

func (e *bootstrapEarlyDisconnectExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusUnauthorized, Message: "not used"}
}

func (e *bootstrapEarlyDisconnectExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.calls++
	call := e.calls
	e.mu.Unlock()

	out := make(chan cliproxyexecutor.StreamChunk, 1)
	if call == 1 {
		out <- cliproxyexecutor.StreamChunk{Err: errors.New("stream disconnected before completion: stream closed before response.completed")}
		close(out)
		return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Attempt": []string{"1"}}, Chunks: out}, nil
	}
	out <- cliproxyexecutor.StreamChunk{Payload: []byte("ok")}
	close(out)
	return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Attempt": []string{"2"}}, Chunks: out}, nil
}

func (e *bootstrapEarlyDisconnectExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *bootstrapEarlyDisconnectExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusUnauthorized, Message: "not used"}
}

func (e *bootstrapEarlyDisconnectExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *bootstrapEarlyDisconnectExecutor) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

type partialOutputThenDisconnectExecutor struct {
	id string

	mu    sync.Mutex
	calls int
}

func (e *partialOutputThenDisconnectExecutor) Identifier() string {
	return e.id
}

func (e *partialOutputThenDisconnectExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusUnauthorized, Message: "not used"}
}

func (e *partialOutputThenDisconnectExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()

	out := make(chan cliproxyexecutor.StreamChunk, 2)
	out <- cliproxyexecutor.StreamChunk{Payload: []byte("partial")}
	out <- cliproxyexecutor.StreamChunk{Err: errors.New("stream disconnected before completion: stream closed before response.completed")}
	close(out)
	return &cliproxyexecutor.StreamResult{Chunks: out}, nil
}

func (e *partialOutputThenDisconnectExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *partialOutputThenDisconnectExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusUnauthorized, Message: "not used"}
}

func (e *partialOutputThenDisconnectExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *partialOutputThenDisconnectExecutor) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func newCredentialRetryLimitTestManager(t *testing.T, maxRetryCredentials int, err error) (*Manager, *credentialRetryLimitExecutor) {
	t.Helper()

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 0, maxRetryCredentials)

	executor := &credentialRetryLimitExecutor{id: "claude", err: err}
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
	budgetedCrossAuthErr := &Error{HTTPStatus: http.StatusNotFound, Message: "upstream resource missing"}
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
			limitedManager, limitedExecutor := newCredentialRetryLimitTestManager(t, 1, budgetedCrossAuthErr)
			if errInvoke := tc.invoke(limitedManager); errInvoke == nil {
				t.Fatalf("expected error for limited retry execution")
			}
			if calls := limitedExecutor.Calls(); calls != 1 {
				t.Fatalf("expected 1 call with max-retry-credentials=1, got %d", calls)
			}

			unlimitedManager, unlimitedExecutor := newCredentialRetryLimitTestManager(t, 0, budgetedCrossAuthErr)
			if errInvoke := tc.invoke(unlimitedManager); errInvoke == nil {
				t.Fatalf("expected error for unlimited retry execution")
			}
			if calls := unlimitedExecutor.Calls(); calls != 2 {
				t.Fatalf("expected 2 calls with max-retry-credentials=0, got %d", calls)
			}
		})
	}
}

func TestShouldCountRetryCredentialBudget_UsesTerminalAuthStringsWithoutStatus(t *testing.T) {
	testCases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "capacity_message",
			err:  errors.New("Selected model is at capacity"),
			want: false,
		},
		{
			name: "deactivated_message",
			err:  errors.New("Your OpenAI account has been deactivated, please check your email for more information."),
			want: false,
		},
		{
			name: "authorization_lost_message",
			err:  errors.New("authorization lost"),
			want: false,
		},
		{
			name: "generic_transport_error",
			err:  errors.New("dial tcp 1.2.3.4:443: i/o timeout"),
			want: true,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldCountRetryCredentialBudget(tc.err); got != tc.want {
				t.Fatalf("shouldCountRetryCredentialBudget(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestManager_MaxRetryCredentials_DoesNotCountAuthTerminalErrors(t *testing.T) {
	request := cliproxyexecutor.Request{Model: "test-model"}
	testCases := []struct {
		name   string
		invoke func(*Manager) error
		err    error
	}{
		{
			name: "execute_401",
			invoke: func(m *Manager) error {
				_, errExecute := m.Execute(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
				return errExecute
			},
			err: &Error{HTTPStatus: http.StatusUnauthorized, Message: "account_deactivated"},
		},
		{
			name: "execute_count_429",
			invoke: func(m *Manager) error {
				_, errExecute := m.ExecuteCount(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
				return errExecute
			},
			err: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "Selected model is at capacity"},
		},
		{
			name: "execute_stream_403",
			invoke: func(m *Manager) error {
				_, errExecute := m.ExecuteStream(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
				return errExecute
			},
			err: &Error{HTTPStatus: http.StatusForbidden, Message: "authorization lost"},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			limitedManager, limitedExecutor := newCredentialRetryLimitTestManager(t, 1, tc.err)
			if errInvoke := tc.invoke(limitedManager); errInvoke == nil {
				t.Fatalf("expected error for terminal auth execution")
			}
			if calls := limitedExecutor.Calls(); calls != 2 {
				t.Fatalf("expected 2 calls with max-retry-credentials=1 when auth terminal errors are exempt, got %d", calls)
			}
		})
	}
}

func TestManager_MaxRetryCredentials_DoesNotCrossCredentialRetryOnStreamConnectTimeout(t *testing.T) {
	request := cliproxyexecutor.Request{Model: "test-model"}
	errTimeout := &Error{HTTPStatus: http.StatusRequestTimeout, Code: "stream_connect_timeout", Message: "upstream stream setup timed out after 10s"}

	limitedManager, limitedExecutor := newCredentialRetryLimitTestManager(t, 1, errTimeout)
	if _, errExecute := limitedManager.ExecuteStream(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{}); errExecute == nil {
		t.Fatal("expected error for limited retry execution")
	}
	if calls := limitedExecutor.Calls(); calls != 1 {
		t.Fatalf("expected 1 call with max-retry-credentials=1, got %d", calls)
	}

	unlimitedManager, unlimitedExecutor := newCredentialRetryLimitTestManager(t, 0, errTimeout)
	if _, errExecute := unlimitedManager.ExecuteStream(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{}); errExecute == nil {
		t.Fatal("expected error for unlimited retry execution")
	}
	if calls := unlimitedExecutor.Calls(); calls != 1 {
		t.Fatalf("expected 1 call with max-retry-credentials=0, got %d", calls)
	}
}

func TestManager_StreamDisconnect_RotatesAcrossAuths(t *testing.T) {
	prevDelay := sameAuthRetryDelayFunc
	sameAuthRetryDelayFunc = func() time.Duration { return 0 }
	t.Cleanup(func() { sameAuthRetryDelayFunc = prevDelay })
	prevStreamDelay := streamDisconnectRetryDelayFunc
	streamDisconnectRetryDelayFunc = func() time.Duration { return 0 }
	t.Cleanup(func() { streamDisconnectRetryDelayFunc = prevStreamDelay })

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
				t.Fatalf("expected stream disconnect error after bounded retries")
			}
			if errInvoke.Error() != "stream disconnected before completion" {
				t.Fatalf("unexpected error: %v", errInvoke)
			}
			if calls := executor.Calls(); calls != 2 {
				t.Fatalf("expected 2 attempts across auths, got %d", calls)
			}
			if distinct := executor.DistinctAuths(); distinct != 2 {
				t.Fatalf("expected retries to rotate across two auths, got %d auths", distinct)
			}
		})
	}
}

func TestManager_BadGateway_RotatesAcrossAuths(t *testing.T) {
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
			manager, executor := newCredentialRetryLimitTestManager(t, 0, &Error{HTTPStatus: http.StatusBadGateway, Message: "bad gateway"})
			errInvoke := invocation.invoke(manager)
			if errInvoke == nil {
				t.Fatalf("expected bad gateway error after bounded retries")
			}
			if calls := executor.Calls(); calls != 2 {
				t.Fatalf("expected 2 attempts across auths for 502, got %d", calls)
			}
		})
	}
}

func TestManager_RequestRetry_DoesNotRetrySameAuthByDefault(t *testing.T) {
	prevDelay := sameAuthRetryDelayFunc
	sameAuthRetryDelayFunc = func() time.Duration { return 0 }
	t.Cleanup(func() { sameAuthRetryDelayFunc = prevDelay })

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(2, 30*time.Second, 0)
	executor := &sameAuthRetryExecutor{id: "claude", err: errors.New("stream disconnected before completion")}
	m.RegisterExecutor(executor)

	authID := uuid.NewString() + "-auth"
	if _, err := m.Register(context.Background(), &Auth{ID: authID, Provider: "claude"}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	_, err := m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("expected non-switchable error")
	}
	if calls := executor.Calls(); calls != 1 {
		t.Fatalf("expected manager request-retry to avoid same-auth retries by default, got %d calls", calls)
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

func TestManager_ExecuteStream_UsesConfiguredBootstrapTimeout(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 0, 1)
	m.SetConfig(&internalconfig.Config{SDKConfig: internalconfig.SDKConfig{Streaming: internalconfig.StreamingConfig{BootstrapTimeoutSeconds: 1}}})

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

	start := time.Now()
	_, err := m.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
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
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("elapsed = %v, want about 1s configured bootstrap timeout", elapsed)
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

func TestManager_ExecuteStream_DoesNotRotateAuthPoolOnStreamConnectTimeout(t *testing.T) {
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
	if calls := executor.Calls("stream"); calls != 1 {
		t.Fatalf("stream calls = %d, want 1", calls)
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

func TestManager_ExecuteStream_RetriesOnceOnZeroOutputEarlyDisconnect(t *testing.T) {
	prevDelay := streamDisconnectRetryDelayFunc
	streamDisconnectRetryDelayFunc = func() time.Duration { return 0 }
	t.Cleanup(func() { streamDisconnectRetryDelayFunc = prevDelay })

	m := NewManager(nil, nil, nil)
	executor := &bootstrapEarlyDisconnectExecutor{id: "codex"}
	m.RegisterExecutor(executor)

	authID := uuid.NewString() + "-auth"
	if _, err := m.Register(context.Background(), &Auth{ID: authID, Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	streamResult, err := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
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
	if got := streamResult.Headers.Get("X-Attempt"); got != "2" {
		t.Fatalf("header X-Attempt = %q, want %q", got, "2")
	}
	if calls := executor.Calls(); calls != 2 {
		t.Fatalf("stream calls = %d, want 2", calls)
	}
}

func TestManager_ExecuteStream_DoesNotRetryAfterFirstPayloadOnDisconnect(t *testing.T) {
	prevDelay := streamDisconnectRetryDelayFunc
	streamDisconnectRetryDelayFunc = func() time.Duration { return 0 }
	t.Cleanup(func() { streamDisconnectRetryDelayFunc = prevDelay })

	m := NewManager(nil, nil, nil)
	executor := &partialOutputThenDisconnectExecutor{id: "codex"}
	m.RegisterExecutor(executor)

	authID := uuid.NewString() + "-auth"
	if _, err := m.Register(context.Background(), &Auth{ID: authID, Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	streamResult, err := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("execute stream: %v", err)
	}
	var (
		payload []byte
		gotErr  error
	)
	for chunk := range streamResult.Chunks {
		if chunk.Err != nil {
			gotErr = chunk.Err
			continue
		}
		payload = append(payload, chunk.Payload...)
	}
	if string(payload) != "partial" {
		t.Fatalf("payload = %q, want %q", string(payload), "partial")
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "stream closed before response.completed") {
		t.Fatalf("stream error = %v, want disconnect error", gotErr)
	}
	if calls := executor.Calls(); calls != 1 {
		t.Fatalf("stream calls = %d, want 1", calls)
	}
}

func TestResolveGPT54FallbackModelName(t *testing.T) {
	testCases := []struct {
		model string
		want  string
	}{
		{model: "gpt-5.4", want: "gpt-5.3-codex"},
		{model: "gpt-5.4-high", want: "gpt-5.3-codex-high"},
		{model: "gpt-5.4-low", want: "gpt-5.3-codex-low"},
		{model: "gpt-5.4-xhigh", want: "gpt-5.3-codex-xhigh"},
		{model: "gpt-5.4-openai-compact", want: "gpt-5.3-codex-openai-compact"},
		{model: "GPT-5.4-OPENAI-COMPACT", want: "gpt-5.3-codex-openai-compact"},
		{model: "gpt-5.3-codex", want: ""},
	}

	for _, tc := range testCases {
		if got := resolveGPT54FallbackModelName(tc.model); got != tc.want {
			t.Fatalf("resolveGPT54FallbackModelName(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}

func TestShouldFallbackGPT54ByError(t *testing.T) {
	testCases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "stream_disconnected_help_center",
			err:  errors.New("stream disconnected before completion: An error occurred while processing your request. You can retry your request, or contact us through our help center at help.openai.com if the error persists."),
			want: true,
		},
		{
			name: "model_at_capacity",
			err:  errors.New("Selected model is at capacity."),
			want: true,
		},
		{
			name: "bootstrap_timeout_message",
			err:  &Error{HTTPStatus: http.StatusRequestTimeout, Message: "stream_bootstrap_timeout: upstream stream timed out waiting for first payload after 30s"},
			want: true,
		},
		{
			name: "stream_connect_timeout_message",
			err:  &Error{HTTPStatus: http.StatusRequestTimeout, Code: "stream_connect_timeout", Message: "upstream stream setup timed out after 30s"},
			want: true,
		},
		{
			name: "goaway_protocol_error",
			err:  errors.New(`Post "https://chatgpt.com/backend-api/codex/responses": http2: server sent GOAWAY and closed the connection; LastStreamID=3623, ErrCode=PROTOCOL_ERROR, debug=""`),
			want: true,
		},
		{
			name: "error_sending_request_for_url",
			err:  errors.New("stream disconnected before completion: error sending request for url (https://api.openai.com/v1/responses)"),
			want: true,
		},
		{
			name: "stream_closed_before_completed",
			err:  errors.New("stream disconnected before completion: stream closed before response.completed"),
			want: true,
		},
		{
			name: "server_error_message",
			err:  errors.New("HTTP 500 server_error: The server had an error processing your request."),
			want: true,
		},
		{
			name: "generic_disconnect_without_signature",
			err:  errors.New("stream disconnected before completion"),
			want: false,
		},
		{
			name: "irrelevant_error",
			err:  errors.New("dial tcp timeout"),
			want: false,
		},
	}

	for _, tc := range testCases {
		got := shouldFallbackGPT54ByError(tc.err)
		if got != tc.want {
			t.Fatalf("%s: shouldFallbackGPT54ByError() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestShouldRetryAcrossAuthPool_DoesNotRotateOnStreamSetupTimeout(t *testing.T) {
	testCases := []struct {
		name string
		err  error
	}{
		{
			name: "stream_bootstrap_timeout",
			err:  &Error{HTTPStatus: http.StatusRequestTimeout, Code: "stream_bootstrap_timeout", Message: "upstream stream timed out waiting for first payload after 30s"},
		},
		{
			name: "stream_connect_timeout",
			err:  &Error{HTTPStatus: http.StatusRequestTimeout, Code: "stream_connect_timeout", Message: "upstream stream setup timed out after 30s"},
		},
	}

	for _, tc := range testCases {
		if got := shouldRetryAcrossAuthPool(tc.err); got {
			t.Fatalf("%s: shouldRetryAcrossAuthPool() = true, want false", tc.name)
		}
	}
}

func TestManager_Execute_GPT54FallbackToGPT53Codex(t *testing.T) {
	prevDelay := sameAuthRetryDelayFunc
	sameAuthRetryDelayFunc = func() time.Duration { return 0 }
	t.Cleanup(func() { sameAuthRetryDelayFunc = prevDelay })

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 30*time.Second, 0)
	executor := &sameAuthRetryExecutor{
		id:  "codex",
		err: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "Selected model is at capacity."},
	}
	m.RegisterExecutor(executor)

	authID := uuid.NewString() + "-auth"
	if _, err := m.Register(context.Background(), &Auth{ID: authID, Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4-high"}, {ID: "gpt-5.3-codex-high"}, {ID: "gpt-5.3-codex"}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4-high"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("expected fallback error")
	}

	models := executor.Models()
	if len(models) < 2 {
		t.Fatalf("expected at least 2 attempts, got %d", len(models))
	}
	if models[0] != "gpt-5.4-high" {
		t.Fatalf("first model = %q, want %q", models[0], "gpt-5.4-high")
	}
	if models[1] != "gpt-5.3-codex-high" {
		t.Fatalf("second model = %q, want %q", models[1], "gpt-5.3-codex-high")
	}
}

func TestManager_Execute_GPT54DoesNotFallbackToGPT53Codex_OnLongStreamDisconnectError(t *testing.T) {
	prevDelay := sameAuthRetryDelayFunc
	sameAuthRetryDelayFunc = func() time.Duration { return 0 }
	t.Cleanup(func() { sameAuthRetryDelayFunc = prevDelay })

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 30*time.Second, 0)
	executor := &sameAuthRetryExecutor{
		id: "codex",
		err: errors.New("do request failed: upstream error: do request failed: stream disconnected before completion: " +
			"An error occurred while processing your request. You can retry your request, or contact us through our help center at help.openai.com if the error persists. " +
			"Please include the request ID 4730fdb2-d1a8-4496-b1f9-a271b42b9a3e in your message."),
	}
	m.RegisterExecutor(executor)

	authID := uuid.NewString() + "-auth"
	if _, err := m.Register(context.Background(), &Auth{ID: authID, Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}, {ID: "gpt-5.3-codex"}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("expected error")
	}

	models := executor.Models()
	if len(models) != 1 {
		t.Fatalf("expected no fallback attempt, got models %v", models)
	}
	if models[0] != "gpt-5.4" {
		t.Fatalf("first model = %q, want %q", models[0], "gpt-5.4")
	}
}

func TestManager_Execute_GPT54FallbackToGPT53Codex_OnWrappedCapacityError(t *testing.T) {
	prevDelay := sameAuthRetryDelayFunc
	sameAuthRetryDelayFunc = func() time.Duration { return 0 }
	t.Cleanup(func() { sameAuthRetryDelayFunc = prevDelay })

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 30*time.Second, 0)
	executor := &sameAuthRetryExecutor{
		id:  "codex",
		err: errors.New("do request failed: upstream error: do request failed: Selected model is at capacity."),
	}
	m.RegisterExecutor(executor)

	authID := uuid.NewString() + "-auth"
	if _, err := m.Register(context.Background(), &Auth{ID: authID, Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}, {ID: "gpt-5.3-codex"}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("expected fallback error")
	}

	models := executor.Models()
	if len(models) < 2 {
		t.Fatalf("expected at least 2 attempts, got %d", len(models))
	}
	if models[0] != "gpt-5.4" {
		t.Fatalf("first model = %q, want %q", models[0], "gpt-5.4")
	}
	if models[1] != "gpt-5.3-codex" {
		t.Fatalf("second model = %q, want %q", models[1], "gpt-5.3-codex")
	}
}

func TestManager_Execute_GPT54FallbackVariantDowngradeToBaseOnNoModel(t *testing.T) {
	prevDelay := sameAuthRetryDelayFunc
	sameAuthRetryDelayFunc = func() time.Duration { return 0 }
	t.Cleanup(func() { sameAuthRetryDelayFunc = prevDelay })

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 30*time.Second, 0)
	executor := &sameAuthRetryExecutor{
		id:  "codex",
		err: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "Selected model is at capacity."},
	}
	m.RegisterExecutor(executor)

	authID := uuid.NewString() + "-auth"
	if _, err := m.Register(context.Background(), &Auth{ID: authID, Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4-xhigh"}, {ID: "gpt-5.3-codex"}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4-xhigh"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("expected fallback error")
	}

	models := executor.Models()
	if len(models) < 2 {
		t.Fatalf("expected at least 2 attempts (primary -> base/variant fallback), got %d", len(models))
	}
	if models[0] != "gpt-5.4-xhigh" {
		t.Fatalf("first model = %q, want %q", models[0], "gpt-5.4-xhigh")
	}
	if models[len(models)-1] != "gpt-5.3-codex" {
		t.Fatalf("last model = %q, want %q", models[len(models)-1], "gpt-5.3-codex")
	}
	if len(models) >= 3 && models[1] != "gpt-5.3-codex-xhigh" {
		t.Fatalf("second model = %q, want %q when variant attempt exists", models[1], "gpt-5.3-codex-xhigh")
	}
}

func newGPT54FallbackMockManager(t *testing.T, execErr error, models []string) (*Manager, *sameAuthRetryExecutor) {
	t.Helper()

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 30*time.Second, 0)
	executor := &sameAuthRetryExecutor{
		id:  "codex",
		err: execErr,
	}
	m.RegisterExecutor(executor)

	authID := uuid.NewString() + "-auth"
	if _, err := m.Register(context.Background(), &Auth{ID: authID, Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	modelInfos := make([]*registry.ModelInfo, 0, len(models))
	for _, modelID := range models {
		modelInfos = append(modelInfos, &registry.ModelInfo{ID: modelID})
	}
	reg.RegisterClient(authID, "codex", modelInfos)
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	return m, executor
}

func assertFallbackFirstSecondModel(t *testing.T, models []string, first string, second string) {
	t.Helper()
	if len(models) < 2 {
		t.Fatalf("expected at least 2 attempts, got %d", len(models))
	}
	if models[0] != first {
		t.Fatalf("first model = %q, want %q", models[0], first)
	}
	if models[1] != second {
		t.Fatalf("second model = %q, want %q", models[1], second)
	}
}

func TestManager_Execute_GPT54DoesNotFallbackToGPT53Codex_OnBootstrapTimeoutError(t *testing.T) {
	prevDelay := sameAuthRetryDelayFunc
	sameAuthRetryDelayFunc = func() time.Duration { return 0 }
	t.Cleanup(func() { sameAuthRetryDelayFunc = prevDelay })

	m, executor := newGPT54FallbackMockManager(
		t,
		&Error{HTTPStatus: http.StatusRequestTimeout, Message: "stream_bootstrap_timeout: upstream stream timed out waiting for first payload after 30s"},
		[]string{"gpt-5.4", "gpt-5.3-codex"},
	)

	_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("expected error")
	}
	models := executor.Models()
	if len(models) != 1 {
		t.Fatalf("expected no fallback attempt, got models %v", models)
	}
	if models[0] != "gpt-5.4" {
		t.Fatalf("expected first model to stay gpt-5.4, got %v", models)
	}
}

func TestManager_ExecuteCount_GPT54FallbackToGPT53Codex_OnlyOnCapacityError(t *testing.T) {
	prevDelay := sameAuthRetryDelayFunc
	sameAuthRetryDelayFunc = func() time.Duration { return 0 }
	t.Cleanup(func() { sameAuthRetryDelayFunc = prevDelay })

	testCases := []struct {
		name         string
		err          error
		wantFallback bool
	}{
		{
			name: "long_stream_disconnect",
			err: errors.New("do request failed: upstream error: do request failed: stream disconnected before completion: " +
				"An error occurred while processing your request. You can retry your request, or contact us through our help center at help.openai.com if the error persists. " +
				"Please include the request ID 4730fdb2-d1a8-4496-b1f9-a271b42b9a3e in your message."),
			wantFallback: false,
		},
		{
			name:         "selected_model_capacity",
			err:          errors.New("do request failed: upstream error: do request failed: Selected model is at capacity."),
			wantFallback: true,
		},
		{
			name:         "goaway_protocol_error",
			err:          errors.New(`do request failed: upstream error: do request failed: Post "https://chatgpt.com/backend-api/codex/responses": http2: server sent GOAWAY and closed the connection; LastStreamID=3623, ErrCode=PROTOCOL_ERROR, debug=""`),
			wantFallback: false,
		},
		{
			name:         "error_sending_request_for_url",
			err:          errors.New("do request failed: upstream error: do request failed: stream disconnected before completion: error sending request for url (https://api.openai.com/v1/responses)"),
			wantFallback: false,
		},
		{
			name:         "stream_closed_before_completed",
			err:          errors.New("do request failed: upstream error: do request failed: stream disconnected before completion: stream closed before response.completed"),
			wantFallback: false,
		},
		{
			name:         "server_error_message",
			err:          errors.New("HTTP 500 server_error: The server had an error processing your request."),
			wantFallback: false,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			m, executor := newGPT54FallbackMockManager(t, tc.err, []string{"gpt-5.4", "gpt-5.3-codex"})

			_, err := m.ExecuteCount(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
			if err == nil {
				t.Fatal("expected error")
			}
			models := executor.Models()
			if tc.wantFallback {
				assertFallbackFirstSecondModel(t, models, "gpt-5.4", "gpt-5.3-codex")
				return
			}
			if len(models) != 1 {
				t.Fatalf("expected no fallback attempt, got models %v", models)
			}
			if models[0] != "gpt-5.4" {
				t.Fatalf("expected first model to stay gpt-5.4, got %v", models)
			}
		})
	}
}

func TestManager_ExecuteStream_GPT54FallbackToGPT53Codex_OnlyOnCapacityError(t *testing.T) {
	prevDelay := sameAuthRetryDelayFunc
	sameAuthRetryDelayFunc = func() time.Duration { return 0 }
	t.Cleanup(func() { sameAuthRetryDelayFunc = prevDelay })

	testCases := []struct {
		name         string
		err          error
		wantFallback bool
	}{
		{
			name: "long_stream_disconnect",
			err: errors.New("do request failed: upstream error: do request failed: stream disconnected before completion: " +
				"An error occurred while processing your request. You can retry your request, or contact us through our help center at help.openai.com if the error persists. " +
				"Please include the request ID 4730fdb2-d1a8-4496-b1f9-a271b42b9a3e in your message."),
			wantFallback: false,
		},
		{
			name:         "selected_model_capacity",
			err:          errors.New("do request failed: upstream error: do request failed: Selected model is at capacity."),
			wantFallback: true,
		},
		{
			name:         "goaway_protocol_error",
			err:          errors.New(`do request failed: upstream error: do request failed: Post "https://chatgpt.com/backend-api/codex/responses": http2: server sent GOAWAY and closed the connection; LastStreamID=3623, ErrCode=PROTOCOL_ERROR, debug=""`),
			wantFallback: false,
		},
		{
			name:         "error_sending_request_for_url",
			err:          errors.New("do request failed: upstream error: do request failed: stream disconnected before completion: error sending request for url (https://api.openai.com/v1/responses)"),
			wantFallback: false,
		},
		{
			name:         "stream_closed_before_completed",
			err:          errors.New("do request failed: upstream error: do request failed: stream disconnected before completion: stream closed before response.completed"),
			wantFallback: false,
		},
		{
			name:         "server_error_message",
			err:          errors.New("HTTP 500 server_error: The server had an error processing your request."),
			wantFallback: false,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			m, executor := newGPT54FallbackMockManager(t, tc.err, []string{"gpt-5.4", "gpt-5.3-codex"})

			_, err := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
			if err == nil {
				t.Fatal("expected error")
			}
			models := executor.Models()
			if tc.wantFallback {
				assertFallbackFirstSecondModel(t, models, "gpt-5.4", "gpt-5.3-codex")
				return
			}
			if len(models) != 1 {
				t.Fatalf("expected no fallback attempt, got models %v", models)
			}
			if models[0] != "gpt-5.4" {
				t.Fatalf("expected first model to stay gpt-5.4, got %v", models)
			}
		})
	}
}

func TestManager_ExecuteStream_GPT54FallbackToGPT53Codex_DoesNotFallbackOnConnectTimeoutsOrCancellation(t *testing.T) {
	prevDelay := sameAuthRetryDelayFunc
	sameAuthRetryDelayFunc = func() time.Duration { return 0 }
	t.Cleanup(func() { sameAuthRetryDelayFunc = prevDelay })

	testCases := []struct {
		name string
		err  error
	}{
		{
			name: "stream_connect_timeout",
			err:  &Error{HTTPStatus: http.StatusRequestTimeout, Code: "stream_connect_timeout", Message: "upstream stream setup timed out after 10s"},
		},
		{
			name: "stream_bootstrap_timeout",
			err:  &Error{HTTPStatus: http.StatusRequestTimeout, Code: "stream_bootstrap_timeout", Message: "upstream stream timed out waiting for first payload after 10s"},
		},
		{
			name: "context_canceled",
			err:  errors.New(`Post "https://chatgpt.com/backend-api/codex/responses": context canceled`),
		},
		{
			name: "context_deadline_exceeded",
			err:  errors.New(`Post "https://chatgpt.com/backend-api/codex/responses": context deadline exceeded`),
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			m, executor := newGPT54FallbackMockManager(t, tc.err, []string{"gpt-5.4", "gpt-5.3-codex"})

			_, err := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
			if err == nil {
				t.Fatal("expected error")
			}

			models := executor.Models()
			if len(models) != 1 {
				t.Fatalf("expected no fallback attempt, got models %v", models)
			}
			if models[0] != "gpt-5.4" {
				t.Fatalf("expected first model to stay gpt-5.4, got %v", models)
			}
		})
	}
}

func TestManager_Execute_StopsImmediatelyWhenRetryErrorIsCanceled(t *testing.T) {
	m, executor := newSameAuthRetryTestManager(t, 2, errors.New(`Post "https://chatgpt.com/backend-api/codex/responses": context canceled`))

	_, err := m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(strings.ToLower(err.Error()), "context canceled") {
		t.Fatalf("expected canceled error, got %v", err)
	}
	if calls := executor.Calls(); calls != 1 {
		t.Fatalf("expected single attempt on canceled retry error, got %d", calls)
	}
	if auths := executor.DistinctAuths(); auths != 1 {
		t.Fatalf("expected single auth on canceled retry error, got %d", auths)
	}
}
