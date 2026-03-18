package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type deletingStore struct {
	mu      sync.Mutex
	deleted []string
}

func (s *deletingStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *deletingStore) Save(context.Context, *Auth) (string, error) { return "", nil }

func (s *deletingStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, id)
	return nil
}

func (s *deletingStore) Deleted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.deleted))
	copy(out, s.deleted)
	return out
}

func waitForDeletedIDs(t *testing.T, store *deletingStore, want []string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := store.Deleted()
		if len(got) == len(want) {
			match := true
			for i := range want {
				if got[i] != want[i] {
					match = false
					break
				}
			}
			if match {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected deleted ids %v, got %v", want, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type revokedRefreshExecutor struct {
	id  string
	err error
}

func (e *revokedRefreshExecutor) Identifier() string {
	return e.id
}

func (e *revokedRefreshExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *revokedRefreshExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *revokedRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, e.err
}

func (e *revokedRefreshExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *revokedRefreshExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestManager_MarkResult_DeletesRevokedPersistedAuth(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auths/revoked.json",
		FileName: "auths/revoked.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "/tmp/revoked.json",
		},
		Metadata: map[string]any{
			"type": "codex",
		},
	}

	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	mgr.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5",
		Success:  false,
		Error: &Error{
			HTTPStatus: 401,
			Message:    `{"error":{"message":"Encountered invalidated oauth token for user, failing request","code":"token_revoked"},"status":401}`,
		},
	})

	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatalf("expected revoked auth to be removed from manager")
	}

	waitForDeletedIDs(t, store, []string{auth.ID})

	if models := reg.GetModelsForClient(auth.ID); len(models) != 0 {
		t.Fatalf("expected revoked auth models to be unregistered, got %d", len(models))
	}
}

func TestManager_MarkResult_DeletesDeactivatedPersistedAuth(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auths/deactivated.json",
		FileName: "auths/deactivated.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "/tmp/deactivated.json",
		},
		Metadata: map[string]any{
			"type": "codex",
		},
	}

	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	mgr.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5",
		Success:  false,
		Error: &Error{
			HTTPStatus: 401,
			Message:    `{"error":{"message":"Your OpenAI account has been deactivated, please check your email for more information. If you feel this is an error, contact us through our help center at help.openai.com.","type":"invalid_request_error","code":"account_deactivated","param":null},"status":401}`,
		},
	})

	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatalf("expected deactivated auth to be removed from manager")
	}

	waitForDeletedIDs(t, store, []string{auth.ID})

	if models := reg.GetModelsForClient(auth.ID); len(models) != 0 {
		t.Fatalf("expected deactivated auth models to be unregistered, got %d", len(models))
	}
}

func TestManager_MarkResult_DeletesDeactivatedWorkspacePersistedAuth(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auths/deactivated-workspace.json",
		FileName: "auths/deactivated-workspace.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "/tmp/deactivated-workspace.json",
		},
		Metadata: map[string]any{
			"type": "codex",
		},
	}

	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	mgr.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5.4",
		Success:  false,
		Error: &Error{
			HTTPStatus: 403,
			Message:    `{"detail":{"code":"deactivated_workspace"}}`,
		},
	})

	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatalf("expected deactivated_workspace auth to be removed from manager")
	}

	waitForDeletedIDs(t, store, []string{auth.ID})

	if models := reg.GetModelsForClient(auth.ID); len(models) != 0 {
		t.Fatalf("expected deactivated_workspace auth models to be unregistered, got %d", len(models))
	}
}

func TestManager_MarkResult_KeepsAuthForGeneric401(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auths/keep.json",
		FileName: "auths/keep.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "/tmp/keep.json",
		},
		Metadata: map[string]any{
			"type": "codex",
		},
	}

	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5",
		Success:  false,
		Error: &Error{
			HTTPStatus: 401,
			Message:    `{"error":{"message":"Unauthorized"},"status":401}`,
		},
	})

	if _, ok := mgr.GetByID(auth.ID); !ok {
		t.Fatalf("expected generic 401 auth to remain registered")
	}

	if deleted := store.Deleted(); len(deleted) != 0 {
		t.Fatalf("expected no deleted auths, got %v", deleted)
	}
}

func TestManager_MarkResult_IgnoresRevokedErrorForRuntimeOnlyAuth(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "runtime-auth",
		Provider: "codex",
		Metadata: map[string]any{
			"type": "codex",
		},
	}

	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5",
		Success:  false,
		Error: &Error{
			HTTPStatus: 401,
			Message:    `{"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","code":"token_invalidated"},"status":401}`,
		},
	})

	if _, ok := mgr.GetByID(auth.ID); !ok {
		t.Fatalf("expected runtime-only auth to remain registered")
	}

	if deleted := store.Deleted(); len(deleted) != 0 {
		t.Fatalf("expected no deleted auths, got %v", deleted)
	}
}

func TestManager_RefreshAuth_DeletesRevokedPersistedAuth(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(&revokedRefreshExecutor{
		id: "codex",
		err: &Error{
			HTTPStatus: 401,
			Message:    `{"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","code":"token_invalidated"},"status":401}`,
		},
	})

	auth := &Auth{
		ID:       "auths/refresh-revoked.json",
		FileName: "auths/refresh-revoked.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "/tmp/refresh-revoked.json",
		},
		Metadata: map[string]any{
			"type": "codex",
		},
	}

	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	mgr.refreshAuth(context.Background(), auth.ID)

	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatalf("expected revoked auth to be removed after refresh failure")
	}

	waitForDeletedIDs(t, store, []string{auth.ID})

	if models := reg.GetModelsForClient(auth.ID); len(models) != 0 {
		t.Fatalf("expected revoked auth models to be unregistered after refresh, got %d", len(models))
	}
}

func TestManager_RefreshAuth_DeletesDeactivatedPersistedAuth(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(&revokedRefreshExecutor{
		id: "codex",
		err: &Error{
			HTTPStatus: 401,
			Message:    `{"error":{"message":"Your OpenAI account has been deactivated, please check your email for more information. If you feel this is an error, contact us through our help center at help.openai.com.","type":"invalid_request_error","code":"account_deactivated","param":null},"status":401}`,
		},
	})

	auth := &Auth{
		ID:       "auths/refresh-deactivated.json",
		FileName: "auths/refresh-deactivated.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "/tmp/refresh-deactivated.json",
		},
		Metadata: map[string]any{
			"type": "codex",
		},
	}

	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	mgr.refreshAuth(context.Background(), auth.ID)

	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatalf("expected deactivated auth to be removed after refresh failure")
	}

	waitForDeletedIDs(t, store, []string{auth.ID})

	if models := reg.GetModelsForClient(auth.ID); len(models) != 0 {
		t.Fatalf("expected deactivated auth models to be unregistered after refresh, got %d", len(models))
	}
}

func TestManager_RefreshAuth_PreservesDetailedFailureState(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(&revokedRefreshExecutor{
		id: "codex",
		err: &Error{
			HTTPStatus: 500,
			Message:    "refresh upstream timeout while contacting auth server",
		},
	})

	auth := &Auth{
		ID:       "auths/refresh-error.json",
		FileName: "auths/refresh-error.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "/tmp/refresh-error.json",
		},
		Metadata: map[string]any{
			"type": "codex",
		},
	}

	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.refreshAuth(context.Background(), auth.ID)

	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth to remain registered after non-401 refresh failure")
	}
	if stored.Status != StatusError {
		t.Fatalf("status = %q, want %q", stored.Status, StatusError)
	}
	if !stored.Unavailable {
		t.Fatalf("expected auth to be marked unavailable")
	}
	if stored.StatusMessage != "refresh upstream timeout while contacting auth server" {
		t.Fatalf("status_message = %q", stored.StatusMessage)
	}
	if stored.LastError == nil {
		t.Fatalf("expected last_error to be recorded")
	}
	if stored.LastError.HTTPStatus != 500 {
		t.Fatalf("last_error.http_status = %d, want 500", stored.LastError.HTTPStatus)
	}
	if stored.LastError.Message != "refresh upstream timeout while contacting auth server" {
		t.Fatalf("last_error.message = %q", stored.LastError.Message)
	}
	if stored.NextRefreshAfter.IsZero() {
		t.Fatalf("expected next_refresh_after to be scheduled")
	}
	if deleted := store.Deleted(); len(deleted) != 0 {
		t.Fatalf("expected no deleted auths, got %v", deleted)
	}
}
