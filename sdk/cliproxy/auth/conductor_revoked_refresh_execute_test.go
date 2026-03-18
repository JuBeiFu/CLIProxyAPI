package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type revokedThenOKExecutor struct {
	id string

	mu           sync.Mutex
	executeCalls int
	refreshCalls int
	refreshErr   error
}

func (e *revokedThenOKExecutor) Identifier() string { return e.id }

func (e *revokedThenOKExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.executeCalls++
	if e.executeCalls == 1 {
		return cliproxyexecutor.Response{}, &Error{
			HTTPStatus: http.StatusUnauthorized,
			Message:    `{"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","code":"token_invalidated"},"status":401}`,
		}
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *revokedThenOKExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *revokedThenOKExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.mu.Lock()
	e.refreshCalls++
	e.mu.Unlock()
	if e.refreshErr != nil {
		return auth, e.refreshErr
	}
	if auth != nil {
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}
		auth.Metadata["access_token"] = "refreshed"
	}
	return auth, nil
}

func (e *revokedThenOKExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *revokedThenOKExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestManager_Execute_RefreshesRevokedAuthBeforeDeleting(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	exec := &revokedThenOKExecutor{id: "codex"}
	mgr.RegisterExecutor(exec)

	auth := &Auth{
		ID:       "auths/revoked-refreshable.json",
		FileName: "auths/revoked-refreshable.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "/tmp/revoked-refreshable.json",
		},
		Metadata: map[string]any{
			"type":          "codex",
			"refresh_token": "rt",
		},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	resp, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if string(resp.Payload) != `{"ok":true}` {
		t.Fatalf("unexpected payload: %s", string(resp.Payload))
	}

	if _, ok := mgr.GetByID(auth.ID); !ok {
		t.Fatalf("expected auth to remain registered after refresh")
	}
	if deleted := store.Deleted(); len(deleted) != 0 {
		t.Fatalf("expected no deleted auths, got %v", deleted)
	}

	exec.mu.Lock()
	defer exec.mu.Unlock()
	if exec.refreshCalls != 1 {
		t.Fatalf("refreshCalls = %d, want 1", exec.refreshCalls)
	}
	if exec.executeCalls != 2 {
		t.Fatalf("executeCalls = %d, want 2", exec.executeCalls)
	}
}

func TestManager_Execute_DeletesRevokedAuthWhenRefreshFails(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	exec := &revokedThenOKExecutor{
		id: "codex",
		refreshErr: &Error{
			HTTPStatus: http.StatusUnauthorized,
			Message:    `{"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","code":"token_invalidated"},"status":401}`,
		},
	}
	mgr.RegisterExecutor(exec)

	auth := &Auth{
		ID:       "auths/revoked-refresh-fails.json",
		FileName: "auths/revoked-refresh-fails.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "/tmp/revoked-refresh-fails.json",
		},
		Metadata: map[string]any{
			"type":          "codex",
			"refresh_token": "rt",
		},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	_, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatalf("expected Execute to fail when refresh fails")
	}

	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatalf("expected auth to be removed after refresh failure")
	}
	waitForDeletedIDs(t, store, []string{auth.ID})
}
