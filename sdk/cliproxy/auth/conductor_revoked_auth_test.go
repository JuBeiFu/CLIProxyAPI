package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"

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

	deleted := store.Deleted()
	if len(deleted) != 1 || deleted[0] != auth.ID {
		t.Fatalf("expected deleted ids [%q], got %v", auth.ID, deleted)
	}

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

	deleted := store.Deleted()
	if len(deleted) != 1 || deleted[0] != auth.ID {
		t.Fatalf("expected deleted ids [%q], got %v", auth.ID, deleted)
	}

	if models := reg.GetModelsForClient(auth.ID); len(models) != 0 {
		t.Fatalf("expected deactivated auth models to be unregistered, got %d", len(models))
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

	deleted := store.Deleted()
	if len(deleted) != 1 || deleted[0] != auth.ID {
		t.Fatalf("expected deleted ids [%q], got %v", auth.ID, deleted)
	}

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

	deleted := store.Deleted()
	if len(deleted) != 1 || deleted[0] != auth.ID {
		t.Fatalf("expected deleted ids [%q], got %v", auth.ID, deleted)
	}

	if models := reg.GetModelsForClient(auth.ID); len(models) != 0 {
		t.Fatalf("expected deactivated auth models to be unregistered after refresh, got %d", len(models))
	}
}
