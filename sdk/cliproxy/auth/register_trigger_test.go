package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// signalExecutor wraps forcedRefreshTestExecutor with a channel so tests can
// wait for the async Refresh invocation without sleeping.
type signalExecutor struct {
	id           string
	mu           sync.Mutex
	refreshedIDs []string
	signal       chan string
}

func newSignalExecutor(id string) *signalExecutor {
	return &signalExecutor{id: id, signal: make(chan string, 8)}
}

func (e *signalExecutor) Identifier() string { return e.id }
func (e *signalExecutor) Execute(ctx context.Context, a *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *signalExecutor) ExecuteStream(ctx context.Context, a *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}
func (e *signalExecutor) CountTokens(ctx context.Context, a *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *signalExecutor) HttpRequest(ctx context.Context, a *Auth, r *http.Request) (*http.Response, error) {
	return nil, nil
}
func (e *signalExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.mu.Lock()
	e.refreshedIDs = append(e.refreshedIDs, auth.ID)
	e.mu.Unlock()
	select {
	case e.signal <- auth.ID:
	default:
	}
	return auth, nil
}

// Register of a codex auth triggers an immediate async refresh.
func TestRegisterCodex_TriggersImmediateRefresh(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, nil, nil)
	exec := newSignalExecutor("codex")
	m.RegisterExecutor(exec)

	a := &Auth{
		ID:       "new-codex-auth",
		Provider: "codex",
		Metadata: map[string]any{
			"refresh_token": "dummy",
		},
	}
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatalf("Register err: %v", err)
	}

	select {
	case id := <-exec.signal:
		if id != "new-codex-auth" {
			t.Fatalf("expected refresh for new-codex-auth, got %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for async refresh after Register")
	}
}

// Register of a non-codex auth MUST NOT trigger immediate refresh.
func TestRegisterNonCodex_NoImmediateRefresh(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, nil, nil)
	exec := newSignalExecutor("gemini")
	m.RegisterExecutor(exec)

	a := &Auth{ID: "new-gemini", Provider: "gemini"}
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatalf("Register err: %v", err)
	}

	select {
	case id := <-exec.signal:
		t.Fatalf("unexpected refresh for non-codex auth: %s", id)
	case <-time.After(200 * time.Millisecond):
		// good — no signal received
	}
}

// Register without a registered executor for codex must not panic.
func TestRegisterCodex_NoExecutorSafeNoop(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, nil, nil)
	a := &Auth{ID: "new-codex-noexec", Provider: "codex"}
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatalf("Register err: %v", err)
	}
	// No panic, no hang, no assertion needed.
}
