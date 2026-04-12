package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type deletingStore struct {
	deleted []string
}

func (s *deletingStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *deletingStore) Save(context.Context, *Auth) (string, error) { return "", nil }

func (s *deletingStore) Delete(_ context.Context, id string) error {
	s.deleted = append(s.deleted, id)
	return nil
}

type revokedRefreshExecutor struct {
	id  string
	err error
}

func (e *revokedRefreshExecutor) Identifier() string { return e.id }

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

func waitForDeletedIDs(t *testing.T, store *deletingStore, want []string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if len(store.deleted) == len(want) {
			match := true
			for i := range want {
				if store.deleted[i] != want[i] {
					match = false
					break
				}
			}
			if match {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected deleted ids %v, got %v", want, store.deleted)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newPersistedCodexAuth(id string) *Auth {
	return &Auth{
		ID:       id,
		FileName: id,
		Provider: "codex",
		Attributes: map[string]string{
			"path": "/tmp/" + id,
		},
		Metadata: map[string]any{
			"type": "codex",
		},
	}
}

func TestManager_MarkResult_DeletesPersistedAuthOnRevoked401(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	auth := newPersistedCodexAuth("auths/revoked.json")
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	mgr.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5",
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusUnauthorized,
			Message:    `{"error":{"message":"Encountered invalidated oauth token for user, failing request","code":"token_revoked"},"status":401}`,
		},
	})

	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatal("expected revoked auth to be removed from manager")
	}
	waitForDeletedIDs(t, store, []string{auth.ID})
	if models := reg.GetModelsForClient(auth.ID); len(models) != 0 {
		t.Fatalf("expected revoked auth models to be unregistered, got %d", len(models))
	}
}

func TestManager_MarkResult_DeletesPersistedAuthOnForbiddenMessage(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	auth := newPersistedCodexAuth("auths/forbidden.json")
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	mgr.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5.4",
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusForbidden,
			Message:    "authorization lost",
		},
	})

	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatal("expected forbidden auth to be removed from manager")
	}
	waitForDeletedIDs(t, store, []string{auth.ID})
}

func TestManager_MarkResult_DeletesPersistedAuthOnMessageOnlyInvalidatedToken(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	auth := newPersistedCodexAuth("auths/message-only.json")
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	mgr.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5.4",
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusInternalServerError,
			Message:    "Your authentication token has been invalidated. Please try signing in again.",
		},
	})

	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatal("expected invalidated-token auth to be removed from manager")
	}
	waitForDeletedIDs(t, store, []string{auth.ID})
}

func TestManager_MarkResult_DisablesRuntimeOnlyAuthOnUnauthorized(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "runtime-only-auth",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
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
			HTTPStatus: http.StatusUnauthorized,
			Message:    `{"error":{"message":"Unauthorized"},"status":401}`,
		},
	})

	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected runtime-only auth to remain registered")
	}
	if !stored.Disabled || stored.Status != StatusDisabled {
		t.Fatalf("expected runtime-only auth to be disabled, got disabled=%v status=%q", stored.Disabled, stored.Status)
	}
	if len(store.deleted) != 0 {
		t.Fatalf("expected runtime-only auth not to be deleted from store, got %v", store.deleted)
	}
}

func TestManager_RefreshAuth_DeletesPersistedAuthOnOrgRequired(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(&revokedRefreshExecutor{
		id: "codex",
		err: &Error{
			HTTPStatus: http.StatusUnauthorized,
			Message:    `{"error":{"message":"You must be a member of an organization to use the API."},"status":401}`,
		},
	})

	auth := newPersistedCodexAuth("auths/refresh-org-required.json")
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	mgr.refreshAuth(context.Background(), auth.ID)

	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatal("expected org-required auth to be removed after refresh failure")
	}
	waitForDeletedIDs(t, store, []string{auth.ID})
	if models := reg.GetModelsForClient(auth.ID); len(models) != 0 {
		t.Fatalf("expected revoked auth models to be unregistered after refresh, got %d", len(models))
	}
}

func TestManager_RefreshAuth_KeepsNonTerminalFailures(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(&revokedRefreshExecutor{
		id: "codex",
		err: &Error{
			HTTPStatus: http.StatusInternalServerError,
			Message:    "refresh upstream timeout",
		},
	})

	auth := newPersistedCodexAuth("auths/refresh-error.json")
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.refreshAuth(context.Background(), auth.ID)

	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected auth to remain registered after non-terminal refresh failure")
	}
	if stored.Disabled {
		t.Fatal("expected non-terminal refresh failure not to disable auth")
	}
	if stored.LastError == nil || stored.LastError.Message != "refresh upstream timeout" {
		t.Fatalf("expected refresh error to be recorded, got %+v", stored.LastError)
	}
	if len(store.deleted) != 0 {
		t.Fatalf("expected no delete, got %v", store.deleted)
	}
}

func TestManager_MarkResult_WritesBanRecordOnRevokedDelete(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)

	authDir := t.TempDir()
	authPath := filepath.Join(authDir, "codex-ban-record.json")
	expectedCreatedAt := time.Date(2026, 4, 1, 12, 34, 56, 0, time.UTC)
	auth := &Auth{
		ID:       "auths/codex-ban-record.json",
		FileName: "codex-ban-record.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": authPath,
		},
		Metadata: map[string]any{
			"type":     "codex",
			"email":    "ban-record@example.com",
			"id_token": testJWTWithCodexClaims(t, map[string]any{"email": "ban-record@example.com", "https://api.openai.com/auth": map[string]any{"chatgpt_subscription_active_start": expectedCreatedAt.Format(time.RFC3339)}}),
		},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	mgr.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5",
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusUnauthorized,
			Message:    `{"error":{"message":"Encountered invalidated oauth token for user, failing request","code":"token_revoked"},"status":401}`,
		},
	})

	waitForDeletedIDs(t, store, []string{auth.ID})

	recordPath := filepath.Join(authDir, ".system", "ban-records", "banned-auth-records-"+time.Now().Format("2006-01-02")+".jsonl")
	recordBytes, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("expected ban record file to exist, got error: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(recordBytes)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected one ban record line, got %d", len(lines))
	}

	var record struct {
		Name      string     `json:"name"`
		Account   string     `json:"account"`
		Provider  string     `json:"provider"`
		Source    string     `json:"source"`
		Reason    string     `json:"reason"`
		CreatedAt *time.Time `json:"created_at"`
		BannedAt  time.Time  `json:"banned_at"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("failed to decode ban record: %v", err)
	}

	if record.Name != auth.FileName {
		t.Fatalf("expected record name %q, got %q", auth.FileName, record.Name)
	}
	if record.Account != "ban-record@example.com" {
		t.Fatalf("expected account %q, got %q", "ban-record@example.com", record.Account)
	}
	if record.Provider != "codex" {
		t.Fatalf("expected provider %q, got %q", "codex", record.Provider)
	}
	if record.Source != "request" {
		t.Fatalf("expected source %q, got %q", "request", record.Source)
	}
	if record.Reason == "" {
		t.Fatal("expected non-empty ban reason")
	}
	if record.CreatedAt == nil || !record.CreatedAt.Equal(expectedCreatedAt) {
		t.Fatalf("expected created_at %s, got %+v", expectedCreatedAt.Format(time.RFC3339), record.CreatedAt)
	}
	if record.BannedAt.IsZero() {
		t.Fatal("expected banned_at to be set")
	}
}

func testJWTWithCodexClaims(t *testing.T, claims map[string]any) string {
	t.Helper()

	header := encodeJWTPart(t, map[string]any{"alg": "none", "typ": "JWT"})
	payload := encodeJWTPart(t, claims)
	return header + "." + payload + ".signature"
}

func encodeJWTPart(t *testing.T, payload map[string]any) string {
	t.Helper()

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to marshal JWT payload: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
