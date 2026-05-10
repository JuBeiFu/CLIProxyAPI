package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type deletingStore struct {
	deleted []string
	saved   []*Auth
}

func (s *deletingStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *deletingStore) Save(_ context.Context, auth *Auth) (string, error) {
	if auth != nil {
		s.saved = append(s.saved, auth.Clone())
		return auth.ID, nil
	}
	return "", nil
}

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

type revokedRequestExecutor struct {
	id   string
	code string

	mu           sync.Mutex
	executeCalls int
	refreshCalls int
}

func (e *revokedRequestExecutor) Identifier() string { return e.id }

func (e *revokedRequestExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.executeCalls++
	e.mu.Unlock()
	return cliproxyexecutor.Response{}, &Error{
		HTTPStatus: http.StatusUnauthorized,
		Message:    `{"error":{"message":"Encountered invalidated oauth token for user, failing request","code":"` + e.code + `"},"status":401}`,
	}
}

func (e *revokedRequestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *revokedRequestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.mu.Lock()
	e.refreshCalls++
	e.mu.Unlock()
	return auth, nil
}

func (e *revokedRequestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *revokedRequestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *revokedRequestExecutor) calls() (int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.executeCalls, e.refreshCalls
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
			"type":          "codex",
			"access_token":  "old-access",
			"refresh_token": "refresh-token",
		},
	}
}

func assertPersistedAuthDeleted(t *testing.T, mgr *Manager, store *deletingStore, auth *Auth) {
	t.Helper()
	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatalf("expected auth %s to be removed from manager", auth.ID)
	}
	waitForDeletedIDs(t, store, []string{auth.ID})
	if !HasRevokedAuthTombstone(auth, time.Now()) {
		t.Fatalf("expected auth %s to leave a tombstone", auth.ID)
	}
}

func TestManager_Execute_DeletesPersistedCodexOnTokenRevokedOrInvalidated(t *testing.T) {
	for _, code := range []string{"token_revoked", "token_invalidated"} {
		t.Run(code, func(t *testing.T) {
			store := &deletingStore{}
			mgr := NewManager(store, nil, nil)
			auth := newPersistedCodexAuth("auths/" + code + ".json")
			if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
				t.Fatalf("Register returned error: %v", err)
			}
			exec := &revokedRequestExecutor{id: "codex", code: code}
			mgr.RegisterExecutor(exec)

			model := "gpt-5-" + code
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

			_, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model, Payload: []byte(`{}`)}, cliproxyexecutor.Options{})
			if err == nil {
				t.Fatal("expected Execute to return the terminal auth error")
			}
			executeCalls, refreshCalls := exec.calls()
			if executeCalls != 1 || refreshCalls != 0 {
				t.Fatalf("expected execute=1 refresh=0, got execute=%d refresh=%d", executeCalls, refreshCalls)
			}
			assertPersistedAuthDeleted(t, mgr, store, auth)
		})
	}
}

func TestManager_MarkResult_DisablesPersistedAuthOnForbiddenMessage(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	auth := newPersistedCodexAuth("auths/forbidden.json")
	clearRevokedAuthTombstoneForTest(t, auth)
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

	assertPersistedAuthDeleted(t, mgr, store, auth)
}

func TestManager_MarkResult_DisablesPersistedAuthOnMessageOnlyInvalidatedToken(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	auth := newPersistedCodexAuth("auths/message-only.json")
	clearRevokedAuthTombstoneForTest(t, auth)
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

	assertPersistedAuthDeleted(t, mgr, store, auth)
}

func TestManager_DeleteRevokedAuth_InvalidatesRouteAwareProviderCache(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "route-aware-delete",
		Provider: "gemini",
		Prefix:   "tenant-a",
	}

	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if !mgr.providerMayNeedRouteAwareSelection("gemini") {
		t.Fatal("expected route-aware provider cache to be populated")
	}

	mgr.deleteRevokedAuth(auth.Clone(), "test cleanup", "test")

	if mgr.providerMayNeedRouteAwareSelection("gemini") {
		t.Fatal("expected route-aware provider cache to clear after auth deletion")
	}
}

func TestManager_MarkRevokedAuthDisabled_InvalidatesRouteAwareProviderCache(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "route-aware-disable",
		Provider: "gemini",
		Prefix:   "tenant-b",
	}

	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if !mgr.providerMayNeedRouteAwareSelection("gemini") {
		t.Fatal("expected route-aware provider cache to be populated")
	}

	mgr.mu.Lock()
	current := mgr.auths[auth.ID]
	if current == nil {
		mgr.mu.Unlock()
		t.Fatal("expected auth to be present")
	}
	current.Disabled = true
	current.Status = StatusDisabled
	snapshot := current.Clone()
	mgr.mu.Unlock()

	mgr.markRevokedAuthDisabled(snapshot, "test disable", "test")

	if mgr.providerMayNeedRouteAwareSelection("gemini") {
		t.Fatal("expected route-aware provider cache to clear after auth disable")
	}
}

func TestManager_MarkResult_DisablesRuntimeOnlyAuthOnUnauthorized(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "runtime-only-auth",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
	}
	clearRevokedAuthTombstoneForTest(t, auth)
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

func TestManager_RefreshAuth_DisablesPersistedAuthOnOrgRequired(t *testing.T) {
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
	clearRevokedAuthTombstoneForTest(t, auth)
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	mgr.refreshAuth(context.Background(), auth.ID)

	assertPersistedAuthDeleted(t, mgr, store, auth)
}

func TestManager_RefreshAuth_DeletesPersistedAuthOnRefreshTokenReused(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(&revokedRefreshExecutor{
		id: "codex",
		err: &Error{
			HTTPStatus: 0,
			Message:    `token refresh failed with status 401: {"error":{"message":"Your refresh token has already been used...","code":"refresh_token_reused"}}`,
		},
	})

	auth := newPersistedCodexAuth("auths/refresh-reused.json")
	clearRevokedAuthTombstoneForTest(t, auth)
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.refreshAuth(context.Background(), auth.ID)

	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatal("expected reused-token auth to be removed after refresh failure")
	}
	waitForDeletedIDs(t, store, []string{auth.ID})
	if len(store.saved) != 0 {
		t.Fatalf("expected reused-token auth not to be re-saved before delete, got saves=%d", len(store.saved))
	}
	if !HasRevokedAuthTombstone(auth, time.Now()) {
		t.Fatal("expected refresh_token_reused auth to leave a tombstone")
	}
}

func TestManager_RefreshAuth_DeletesPersistedAuthOnRefreshTokenInvalidated(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	mgr.RegisterExecutor(&revokedRefreshExecutor{
		id: "codex",
		err: &Error{
			HTTPStatus: 0,
			Message:    `token refresh failed with status 401: {"error":{"message":"Your refresh token has been invalidated. Please try signing in again.","code":"refresh_token_invalidated"}}`,
		},
	})

	auth := newPersistedCodexAuth("auths/refresh-invalidated.json")
	clearRevokedAuthTombstoneForTest(t, auth)
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.refreshAuth(context.Background(), auth.ID)

	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatal("expected invalidated-refresh-token auth to be removed after refresh failure")
	}
	waitForDeletedIDs(t, store, []string{auth.ID})
	if len(store.saved) != 0 {
		t.Fatalf("expected invalidated-refresh-token auth not to be re-saved before delete, got saves=%d", len(store.saved))
	}
	if !HasRevokedAuthTombstone(auth, time.Now()) {
		t.Fatal("expected refresh_token_invalidated auth to leave a tombstone")
	}
}

func TestManager_MarkResult_DeletesPersistedAuthOnRefreshTokenReused(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	auth := newPersistedCodexAuth("auths/request-reused.json")
	clearRevokedAuthTombstoneForTest(t, auth)
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5.4",
		Success:  false,
		Error: &Error{
			HTTPStatus: 0,
			Message:    `token refresh failed with status 401: {"error":{"message":"Your refresh token has already been used...","code":"refresh_token_reused"}}`,
		},
	})

	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatal("expected refresh_token_reused auth to be removed")
	}
	waitForDeletedIDs(t, store, []string{auth.ID})
	if !HasRevokedAuthTombstone(auth, time.Now()) {
		t.Fatal("expected refresh_token_reused auth to leave a tombstone")
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
	clearRevokedAuthTombstoneForTest(t, auth)
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

func TestManager_MarkResult_WritesBanRecordOnRevokedDisable(t *testing.T) {
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
	clearRevokedAuthTombstoneForTest(t, auth)
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
			Message:    `{"error":{"message":"You must be a member of an organization to use the API."},"status":401}`,
		},
	})

	assertPersistedAuthDeleted(t, mgr, store, auth)

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
