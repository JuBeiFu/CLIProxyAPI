package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type quotaPoolError struct {
	retryAfter time.Duration
}

func (e *quotaPoolError) Error() string { return "quota exhausted" }

func (e *quotaPoolError) StatusCode() int { return http.StatusTooManyRequests }

func (e *quotaPoolError) RetryAfter() *time.Duration { return &e.retryAfter }

type quotaPoolExecutor struct {
	id         string
	retryAfter time.Duration

	mu    sync.Mutex
	calls int
}

func (e *quotaPoolExecutor) Identifier() string { return e.id }

func (e *quotaPoolExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.recordCall()
	return cliproxyexecutor.Response{}, &quotaPoolError{retryAfter: e.retryAfter}
}

func (e *quotaPoolExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.recordCall()
	return nil, &quotaPoolError{retryAfter: e.retryAfter}
}

func (e *quotaPoolExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }

func (e *quotaPoolExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.recordCall()
	return cliproxyexecutor.Response{}, &quotaPoolError{retryAfter: e.retryAfter}
}

func (e *quotaPoolExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) { return nil, nil }

func (e *quotaPoolExecutor) recordCall() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
}

func (e *quotaPoolExecutor) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func newQuotaPoolTestManager(t *testing.T, retryAfter time.Duration) (*Manager, *quotaPoolExecutor) {
	t.Helper()

	m := NewManager(nil, nil, nil)
	// Allow outer retries; the fix should prevent retries when the pool is exhausted.
	m.SetRetryConfig(2, 5*time.Second, 0)

	executor := &quotaPoolExecutor{id: "claude", retryAfter: retryAfter}
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

func TestManager_QuotaPoolExhausted_ReturnsCooldownErrorWithoutRetrying(t *testing.T) {
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
			manager, executor := newQuotaPoolTestManager(t, 200*time.Millisecond)
			errInvoke := invocation.invoke(manager)
			if errInvoke == nil {
				t.Fatalf("expected error")
			}

			// Execute/stream paths record auth state and should surface a pool-wide cooldown
			// error once all credentials have been exhausted.
			if invocation.name != "execute_count" {
				var mce *modelCooldownError
				if !errors.As(errInvoke, &mce) {
					t.Fatalf("expected *modelCooldownError, got %T (%v)", errInvoke, errInvoke)
				}
				if mce.StatusCode() != http.StatusTooManyRequests {
					t.Fatalf("StatusCode() = %d, want %d", mce.StatusCode(), http.StatusTooManyRequests)
				}
			}

			// Expect exactly one attempt per credential; retrying the whole request would
			// cause additional executor calls.
			if calls := executor.Calls(); calls != 2 {
				t.Fatalf("expected 2 calls (one per auth), got %d", calls)
			}
		})
	}
}
