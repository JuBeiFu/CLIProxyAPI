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

type selectiveRevokedRefreshExecutor struct {
	id                string
	revokedAuthID     string
	refreshErrs       map[string]error
	payloads          map[string]string
	countErrors       map[string]error
	countPayloads     map[string]string
	streamPayloads    map[string]string
	streamFirstErrors map[string]error

	mu           sync.Mutex
	executeCalls map[string]int
	countCalls   map[string]int
	streamCalls  map[string]int
	refreshCalls map[string]int
}

func (e *selectiveRevokedRefreshExecutor) Identifier() string { return e.id }

func (e *selectiveRevokedRefreshExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.executeCalls == nil {
		e.executeCalls = make(map[string]int)
	}

	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	e.executeCalls[authID]++
	if authID == e.revokedAuthID && e.executeCalls[authID] == 1 {
		return cliproxyexecutor.Response{}, &Error{
			HTTPStatus: http.StatusUnauthorized,
			Message:    `{"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","code":"token_invalidated"},"status":401}`,
		}
	}

	payload := `{"auth":"ok"}`
	if raw := e.payloads[authID]; raw != "" {
		payload = raw
	}
	return cliproxyexecutor.Response{Payload: []byte(payload)}, nil
}

func (e *selectiveRevokedRefreshExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	if e.streamCalls == nil {
		e.streamCalls = make(map[string]int)
	}

	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	e.streamCalls[authID]++
	callCount := e.streamCalls[authID]
	firstErr := e.streamFirstErrors[authID]
	payload := `{"auth":"ok"}`
	if raw := e.streamPayloads[authID]; raw != "" {
		payload = raw
	} else if raw := e.payloads[authID]; raw != "" {
		payload = raw
	}
	e.mu.Unlock()

	if callCount == 1 && firstErr != nil {
		return nil, firstErr
	}

	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(payload)}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *selectiveRevokedRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.mu.Lock()
	if e.refreshCalls == nil {
		e.refreshCalls = make(map[string]int)
	}
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	e.refreshCalls[authID]++
	err := e.refreshErrs[authID]
	e.mu.Unlock()

	if err != nil {
		return auth, err
	}
	if auth != nil {
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}
		auth.Metadata["access_token"] = "refreshed-" + authID
	}
	return auth, nil
}

func (e *selectiveRevokedRefreshExecutor) CountTokens(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	if e.countCalls == nil {
		e.countCalls = make(map[string]int)
	}

	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	e.countCalls[authID]++
	err := e.countErrors[authID]
	payload := `{"auth":"ok"}`
	if raw := e.countPayloads[authID]; raw != "" {
		payload = raw
	} else if raw := e.payloads[authID]; raw != "" {
		payload = raw
	}
	e.mu.Unlock()

	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	return cliproxyexecutor.Response{Payload: []byte(payload)}, nil
}

func (e *selectiveRevokedRefreshExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *selectiveRevokedRefreshExecutor) ExecuteCalls(authID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.executeCalls[authID]
}

func (e *selectiveRevokedRefreshExecutor) RefreshCalls(authID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.refreshCalls[authID]
}

func (e *selectiveRevokedRefreshExecutor) CountCalls(authID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.countCalls[authID]
}

func (e *selectiveRevokedRefreshExecutor) StreamCalls(authID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.streamCalls[authID]
}

func drainStreamPayload(t *testing.T, result *cliproxyexecutor.StreamResult) string {
	t.Helper()
	if result == nil {
		t.Fatal("stream result is nil")
	}

	var payload []byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	return string(payload)
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

func TestManager_Execute_CodexPaidRevokedRefreshSuccessKeepsPaidSelection(t *testing.T) {
	testCases := []struct {
		name  string
		model string
	}{
		{name: "GPT53Codex", model: "gpt-5.3-codex"},
		{name: "GPT54", model: "gpt-5.4"},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			store := &deletingStore{}
			mgr := NewManager(store, nil, nil)

			paidAuth := &Auth{
				ID:       "auths/" + tc.name + "-paid-refreshable.json",
				FileName: "auths/" + tc.name + "-paid-refreshable.json",
				Provider: "codex",
				Attributes: map[string]string{
					"path": "/tmp/" + tc.name + "-paid-refreshable.json",
				},
				Metadata: map[string]any{
					"type":          "codex",
					"plan_type":     "team",
					"refresh_token": "rt-paid",
				},
			}
			freeAuth := &Auth{
				ID:       "auths/" + tc.name + "-free-backup.json",
				FileName: "auths/" + tc.name + "-free-backup.json",
				Provider: "codex",
				Attributes: map[string]string{
					"path": "/tmp/" + tc.name + "-free-backup.json",
				},
				Metadata: map[string]any{
					"type":      "codex",
					"plan_type": "free",
				},
			}

			exec := &selectiveRevokedRefreshExecutor{
				id:            "codex",
				revokedAuthID: paidAuth.ID,
				payloads: map[string]string{
					paidAuth.ID: `{"auth":"paid"}`,
					freeAuth.ID: `{"auth":"free"}`,
				},
			}
			mgr.RegisterExecutor(exec)

			if _, err := mgr.Register(WithSkipPersist(context.Background()), paidAuth); err != nil {
				t.Fatalf("register paid auth: %v", err)
			}
			if _, err := mgr.Register(WithSkipPersist(context.Background()), freeAuth); err != nil {
				t.Fatalf("register free auth: %v", err)
			}

			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(paidAuth.ID, "codex", []*registry.ModelInfo{{ID: tc.model}})
			reg.RegisterClient(freeAuth.ID, "codex", []*registry.ModelInfo{{ID: tc.model}})
			mgr.RefreshSchedulerEntry(paidAuth.ID)
			mgr.RefreshSchedulerEntry(freeAuth.ID)
			t.Cleanup(func() {
				reg.UnregisterClient(paidAuth.ID)
				reg.UnregisterClient(freeAuth.ID)
			})

			resp, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: tc.model}, cliproxyexecutor.Options{})
			if err != nil {
				t.Fatalf("Execute returned error: %v", err)
			}
			if string(resp.Payload) != `{"auth":"paid"}` {
				t.Fatalf("unexpected payload: %s", string(resp.Payload))
			}

			if _, ok := mgr.GetByID(paidAuth.ID); !ok {
				t.Fatalf("expected paid auth to remain registered after refresh success")
			}
			if _, ok := mgr.GetByID(freeAuth.ID); !ok {
				t.Fatalf("expected free auth to remain registered")
			}
			if deleted := store.Deleted(); len(deleted) != 0 {
				t.Fatalf("expected no deleted auths, got %v", deleted)
			}
			if got := exec.RefreshCalls(paidAuth.ID); got != 1 {
				t.Fatalf("paid refresh calls = %d, want 1", got)
			}
			if got := exec.ExecuteCalls(paidAuth.ID); got != 2 {
				t.Fatalf("paid execute calls = %d, want 2", got)
			}
			if got := exec.ExecuteCalls(freeAuth.ID); got != 0 {
				t.Fatalf("free execute calls = %d, want 0", got)
			}
		})
	}
}

func TestManager_Execute_CodexPaidRevokedRefreshFailureFallsBackToFree(t *testing.T) {
	testCases := []struct {
		name  string
		model string
	}{
		{name: "GPT53Codex", model: "gpt-5.3-codex"},
		{name: "GPT54", model: "gpt-5.4"},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			store := &deletingStore{}
			mgr := NewManager(store, nil, nil)

			paidAuth := &Auth{
				ID:       "auths/" + tc.name + "-paid-revoked.json",
				FileName: "auths/" + tc.name + "-paid-revoked.json",
				Provider: "codex",
				Attributes: map[string]string{
					"path": "/tmp/" + tc.name + "-paid-revoked.json",
				},
				Metadata: map[string]any{
					"type":          "codex",
					"plan_type":     "team",
					"refresh_token": "rt-paid",
				},
			}
			freeAuth := &Auth{
				ID:       "auths/" + tc.name + "-free-fallback.json",
				FileName: "auths/" + tc.name + "-free-fallback.json",
				Provider: "codex",
				Attributes: map[string]string{
					"path": "/tmp/" + tc.name + "-free-fallback.json",
				},
				Metadata: map[string]any{
					"type":      "codex",
					"plan_type": "free",
				},
			}

			exec := &selectiveRevokedRefreshExecutor{
				id:            "codex",
				revokedAuthID: paidAuth.ID,
				refreshErrs: map[string]error{
					paidAuth.ID: &Error{
						HTTPStatus: http.StatusUnauthorized,
						Message:    `{"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","code":"token_invalidated"},"status":401}`,
					},
				},
				payloads: map[string]string{
					paidAuth.ID: `{"auth":"paid"}`,
					freeAuth.ID: `{"auth":"free"}`,
				},
			}
			mgr.RegisterExecutor(exec)

			if _, err := mgr.Register(WithSkipPersist(context.Background()), paidAuth); err != nil {
				t.Fatalf("register paid auth: %v", err)
			}
			if _, err := mgr.Register(WithSkipPersist(context.Background()), freeAuth); err != nil {
				t.Fatalf("register free auth: %v", err)
			}

			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(paidAuth.ID, "codex", []*registry.ModelInfo{{ID: tc.model}})
			reg.RegisterClient(freeAuth.ID, "codex", []*registry.ModelInfo{{ID: tc.model}})
			mgr.RefreshSchedulerEntry(paidAuth.ID)
			mgr.RefreshSchedulerEntry(freeAuth.ID)
			t.Cleanup(func() {
				reg.UnregisterClient(paidAuth.ID)
				reg.UnregisterClient(freeAuth.ID)
			})

			resp, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: tc.model}, cliproxyexecutor.Options{})
			if err != nil {
				t.Fatalf("Execute returned error: %v", err)
			}
			if string(resp.Payload) != `{"auth":"free"}` {
				t.Fatalf("unexpected payload: %s", string(resp.Payload))
			}

			if _, ok := mgr.GetByID(paidAuth.ID); ok {
				t.Fatalf("expected paid auth to be removed after refresh failure")
			}
			if _, ok := mgr.GetByID(freeAuth.ID); !ok {
				t.Fatalf("expected free auth to remain registered")
			}
			waitForDeletedIDs(t, store, []string{paidAuth.ID})
			if models := reg.GetModelsForClient(paidAuth.ID); len(models) != 0 {
				t.Fatalf("expected paid auth models to be unregistered, got %d", len(models))
			}
			if got := exec.RefreshCalls(paidAuth.ID); got != 1 {
				t.Fatalf("paid refresh calls = %d, want 1", got)
			}
			if got := exec.ExecuteCalls(paidAuth.ID); got != 1 {
				t.Fatalf("paid execute calls = %d, want 1", got)
			}
			if got := exec.ExecuteCalls(freeAuth.ID); got != 1 {
				t.Fatalf("free execute calls = %d, want 1", got)
			}
		})
	}
}

func TestManager_ExecuteCount_CodexSingleProviderPrefersPaidForHighValueModels(t *testing.T) {
	testCases := []struct {
		name  string
		model string
	}{
		{name: "GPT53Codex", model: "gpt-5.3-codex"},
		{name: "GPT54", model: "gpt-5.4"},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewManager(nil, nil, nil)

			paidAuth := &Auth{
				ID:       "auths/" + tc.name + "-count-paid.json",
				FileName: "auths/" + tc.name + "-count-paid.json",
				Provider: "codex",
				Attributes: map[string]string{
					"path": "/tmp/" + tc.name + "-count-paid.json",
				},
				Metadata: map[string]any{
					"type":      "codex",
					"plan_type": "team",
				},
			}
			freeAuth := &Auth{
				ID:       "auths/" + tc.name + "-count-free.json",
				FileName: "auths/" + tc.name + "-count-free.json",
				Provider: "codex",
				Attributes: map[string]string{
					"path": "/tmp/" + tc.name + "-count-free.json",
				},
				Metadata: map[string]any{
					"type":      "codex",
					"plan_type": "free",
				},
			}

			exec := &selectiveRevokedRefreshExecutor{
				id: "codex",
				countPayloads: map[string]string{
					paidAuth.ID: `{"auth":"paid"}`,
					freeAuth.ID: `{"auth":"free"}`,
				},
			}
			mgr.RegisterExecutor(exec)

			if _, err := mgr.Register(WithSkipPersist(context.Background()), paidAuth); err != nil {
				t.Fatalf("register paid auth: %v", err)
			}
			if _, err := mgr.Register(WithSkipPersist(context.Background()), freeAuth); err != nil {
				t.Fatalf("register free auth: %v", err)
			}

			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(paidAuth.ID, "codex", []*registry.ModelInfo{{ID: tc.model}})
			reg.RegisterClient(freeAuth.ID, "codex", []*registry.ModelInfo{{ID: tc.model}})
			mgr.RefreshSchedulerEntry(paidAuth.ID)
			mgr.RefreshSchedulerEntry(freeAuth.ID)
			t.Cleanup(func() {
				reg.UnregisterClient(paidAuth.ID)
				reg.UnregisterClient(freeAuth.ID)
			})

			resp, err := mgr.ExecuteCount(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: tc.model}, cliproxyexecutor.Options{})
			if err != nil {
				t.Fatalf("ExecuteCount returned error: %v", err)
			}
			if string(resp.Payload) != `{"auth":"paid"}` {
				t.Fatalf("unexpected payload: %s", string(resp.Payload))
			}
			if got := exec.CountCalls(paidAuth.ID); got != 1 {
				t.Fatalf("paid count calls = %d, want 1", got)
			}
			if got := exec.CountCalls(freeAuth.ID); got != 0 {
				t.Fatalf("free count calls = %d, want 0", got)
			}
		})
	}
}

func TestManager_ExecuteCount_CodexPaidFailureFallsBackToFree(t *testing.T) {
	testCases := []struct {
		name  string
		model string
	}{
		{name: "GPT53Codex", model: "gpt-5.3-codex"},
		{name: "GPT54", model: "gpt-5.4"},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewManager(nil, nil, nil)

			paidAuth := &Auth{
				ID:       "auths/" + tc.name + "-count-paid-rate-limited.json",
				FileName: "auths/" + tc.name + "-count-paid-rate-limited.json",
				Provider: "codex",
				Attributes: map[string]string{
					"path": "/tmp/" + tc.name + "-count-paid-rate-limited.json",
				},
				Metadata: map[string]any{
					"type":      "codex",
					"plan_type": "team",
				},
			}
			freeAuth := &Auth{
				ID:       "auths/" + tc.name + "-count-free-fallback.json",
				FileName: "auths/" + tc.name + "-count-free-fallback.json",
				Provider: "codex",
				Attributes: map[string]string{
					"path": "/tmp/" + tc.name + "-count-free-fallback.json",
				},
				Metadata: map[string]any{
					"type":      "codex",
					"plan_type": "free",
				},
			}

			exec := &selectiveRevokedRefreshExecutor{
				id: "codex",
				countErrors: map[string]error{
					paidAuth.ID: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"},
				},
				countPayloads: map[string]string{
					paidAuth.ID: `{"auth":"paid"}`,
					freeAuth.ID: `{"auth":"free"}`,
				},
			}
			mgr.RegisterExecutor(exec)

			if _, err := mgr.Register(WithSkipPersist(context.Background()), paidAuth); err != nil {
				t.Fatalf("register paid auth: %v", err)
			}
			if _, err := mgr.Register(WithSkipPersist(context.Background()), freeAuth); err != nil {
				t.Fatalf("register free auth: %v", err)
			}

			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(paidAuth.ID, "codex", []*registry.ModelInfo{{ID: tc.model}})
			reg.RegisterClient(freeAuth.ID, "codex", []*registry.ModelInfo{{ID: tc.model}})
			mgr.RefreshSchedulerEntry(paidAuth.ID)
			mgr.RefreshSchedulerEntry(freeAuth.ID)
			t.Cleanup(func() {
				reg.UnregisterClient(paidAuth.ID)
				reg.UnregisterClient(freeAuth.ID)
			})

			resp, err := mgr.ExecuteCount(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: tc.model}, cliproxyexecutor.Options{})
			if err != nil {
				t.Fatalf("ExecuteCount returned error: %v", err)
			}
			if string(resp.Payload) != `{"auth":"free"}` {
				t.Fatalf("unexpected payload: %s", string(resp.Payload))
			}
			if _, ok := mgr.GetByID(paidAuth.ID); !ok {
				t.Fatalf("expected paid auth to remain registered after count fallback")
			}
			if _, ok := mgr.GetByID(freeAuth.ID); !ok {
				t.Fatalf("expected free auth to remain registered")
			}
			if got := exec.CountCalls(paidAuth.ID); got != 1 {
				t.Fatalf("paid count calls = %d, want 1", got)
			}
			if got := exec.CountCalls(freeAuth.ID); got != 1 {
				t.Fatalf("free count calls = %d, want 1", got)
			}
			if got := exec.RefreshCalls(paidAuth.ID); got != 0 {
				t.Fatalf("paid refresh calls = %d, want 0", got)
			}
		})
	}
}

func TestManager_ExecuteStream_CodexPaidRevokedRefreshSuccessKeepsPaidSelection(t *testing.T) {
	testCases := []struct {
		name  string
		model string
	}{
		{name: "GPT53Codex", model: "gpt-5.3-codex"},
		{name: "GPT54", model: "gpt-5.4"},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			store := &deletingStore{}
			mgr := NewManager(store, nil, nil)

			paidAuth := &Auth{
				ID:       "auths/" + tc.name + "-stream-paid-refreshable.json",
				FileName: "auths/" + tc.name + "-stream-paid-refreshable.json",
				Provider: "codex",
				Attributes: map[string]string{
					"path": "/tmp/" + tc.name + "-stream-paid-refreshable.json",
				},
				Metadata: map[string]any{
					"type":          "codex",
					"plan_type":     "team",
					"refresh_token": "rt-paid",
				},
			}
			freeAuth := &Auth{
				ID:       "auths/" + tc.name + "-stream-free-backup.json",
				FileName: "auths/" + tc.name + "-stream-free-backup.json",
				Provider: "codex",
				Attributes: map[string]string{
					"path": "/tmp/" + tc.name + "-stream-free-backup.json",
				},
				Metadata: map[string]any{
					"type":      "codex",
					"plan_type": "free",
				},
			}

			exec := &selectiveRevokedRefreshExecutor{
				id: "codex",
				streamFirstErrors: map[string]error{
					paidAuth.ID: &Error{
						HTTPStatus: http.StatusUnauthorized,
						Message:    `{"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","code":"token_invalidated"},"status":401}`,
					},
				},
				streamPayloads: map[string]string{
					paidAuth.ID: `{"auth":"paid"}`,
					freeAuth.ID: `{"auth":"free"}`,
				},
			}
			mgr.RegisterExecutor(exec)

			if _, err := mgr.Register(WithSkipPersist(context.Background()), paidAuth); err != nil {
				t.Fatalf("register paid auth: %v", err)
			}
			if _, err := mgr.Register(WithSkipPersist(context.Background()), freeAuth); err != nil {
				t.Fatalf("register free auth: %v", err)
			}

			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(paidAuth.ID, "codex", []*registry.ModelInfo{{ID: tc.model}})
			reg.RegisterClient(freeAuth.ID, "codex", []*registry.ModelInfo{{ID: tc.model}})
			mgr.RefreshSchedulerEntry(paidAuth.ID)
			mgr.RefreshSchedulerEntry(freeAuth.ID)
			t.Cleanup(func() {
				reg.UnregisterClient(paidAuth.ID)
				reg.UnregisterClient(freeAuth.ID)
			})

			streamResult, err := mgr.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: tc.model}, cliproxyexecutor.Options{})
			if err != nil {
				t.Fatalf("ExecuteStream returned error: %v", err)
			}
			if payload := drainStreamPayload(t, streamResult); payload != `{"auth":"paid"}` {
				t.Fatalf("unexpected payload: %s", payload)
			}

			if _, ok := mgr.GetByID(paidAuth.ID); !ok {
				t.Fatalf("expected paid auth to remain registered after refresh success")
			}
			if _, ok := mgr.GetByID(freeAuth.ID); !ok {
				t.Fatalf("expected free auth to remain registered")
			}
			if deleted := store.Deleted(); len(deleted) != 0 {
				t.Fatalf("expected no deleted auths, got %v", deleted)
			}
			if got := exec.RefreshCalls(paidAuth.ID); got != 1 {
				t.Fatalf("paid refresh calls = %d, want 1", got)
			}
			if got := exec.StreamCalls(paidAuth.ID); got != 2 {
				t.Fatalf("paid stream calls = %d, want 2", got)
			}
			if got := exec.StreamCalls(freeAuth.ID); got != 0 {
				t.Fatalf("free stream calls = %d, want 0", got)
			}
		})
	}
}

func TestManager_ExecuteStream_CodexPaidRevokedRefreshFailureFallsBackToFree(t *testing.T) {
	testCases := []struct {
		name  string
		model string
	}{
		{name: "GPT53Codex", model: "gpt-5.3-codex"},
		{name: "GPT54", model: "gpt-5.4"},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			store := &deletingStore{}
			mgr := NewManager(store, nil, nil)

			paidAuth := &Auth{
				ID:       "auths/" + tc.name + "-stream-paid-revoked.json",
				FileName: "auths/" + tc.name + "-stream-paid-revoked.json",
				Provider: "codex",
				Attributes: map[string]string{
					"path": "/tmp/" + tc.name + "-stream-paid-revoked.json",
				},
				Metadata: map[string]any{
					"type":          "codex",
					"plan_type":     "team",
					"refresh_token": "rt-paid",
				},
			}
			freeAuth := &Auth{
				ID:       "auths/" + tc.name + "-stream-free-fallback.json",
				FileName: "auths/" + tc.name + "-stream-free-fallback.json",
				Provider: "codex",
				Attributes: map[string]string{
					"path": "/tmp/" + tc.name + "-stream-free-fallback.json",
				},
				Metadata: map[string]any{
					"type":      "codex",
					"plan_type": "free",
				},
			}

			exec := &selectiveRevokedRefreshExecutor{
				id: "codex",
				streamFirstErrors: map[string]error{
					paidAuth.ID: &Error{
						HTTPStatus: http.StatusUnauthorized,
						Message:    `{"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","code":"token_invalidated"},"status":401}`,
					},
				},
				refreshErrs: map[string]error{
					paidAuth.ID: &Error{
						HTTPStatus: http.StatusUnauthorized,
						Message:    `{"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","code":"token_invalidated"},"status":401}`,
					},
				},
				streamPayloads: map[string]string{
					paidAuth.ID: `{"auth":"paid"}`,
					freeAuth.ID: `{"auth":"free"}`,
				},
			}
			mgr.RegisterExecutor(exec)

			if _, err := mgr.Register(WithSkipPersist(context.Background()), paidAuth); err != nil {
				t.Fatalf("register paid auth: %v", err)
			}
			if _, err := mgr.Register(WithSkipPersist(context.Background()), freeAuth); err != nil {
				t.Fatalf("register free auth: %v", err)
			}

			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(paidAuth.ID, "codex", []*registry.ModelInfo{{ID: tc.model}})
			reg.RegisterClient(freeAuth.ID, "codex", []*registry.ModelInfo{{ID: tc.model}})
			mgr.RefreshSchedulerEntry(paidAuth.ID)
			mgr.RefreshSchedulerEntry(freeAuth.ID)
			t.Cleanup(func() {
				reg.UnregisterClient(paidAuth.ID)
				reg.UnregisterClient(freeAuth.ID)
			})

			streamResult, err := mgr.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: tc.model}, cliproxyexecutor.Options{})
			if err != nil {
				t.Fatalf("ExecuteStream returned error: %v", err)
			}
			if payload := drainStreamPayload(t, streamResult); payload != `{"auth":"free"}` {
				t.Fatalf("unexpected payload: %s", payload)
			}

			waitForDeletedIDs(t, store, []string{paidAuth.ID})
			if _, ok := mgr.GetByID(paidAuth.ID); ok {
				t.Fatalf("expected paid auth to be removed after refresh failure")
			}
			if _, ok := mgr.GetByID(freeAuth.ID); !ok {
				t.Fatalf("expected free auth to remain registered")
			}
			if models := reg.GetModelsForClient(paidAuth.ID); len(models) != 0 {
				t.Fatalf("expected paid auth models to be unregistered, got %d", len(models))
			}
			if got := exec.RefreshCalls(paidAuth.ID); got != 1 {
				t.Fatalf("paid refresh calls = %d, want 1", got)
			}
			if got := exec.StreamCalls(paidAuth.ID); got != 1 {
				t.Fatalf("paid stream calls = %d, want 1", got)
			}
			if got := exec.StreamCalls(freeAuth.ID); got != 1 {
				t.Fatalf("free stream calls = %d, want 1", got)
			}
		})
	}
}
