package auth

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type blockingFreeUseExecutor struct {
	id string

	mu      sync.Mutex
	calls   int
	started chan struct{}
}

func (e *blockingFreeUseExecutor) Identifier() string { return e.id }

func (e *blockingFreeUseExecutor) Execute(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.calls++
	started := e.started
	e.mu.Unlock()
	if started != nil {
		select {
		case <-started:
		default:
			close(started)
		}
	}
	<-ctx.Done()
	return cliproxyexecutor.Response{}, ctx.Err()
}

func (e *blockingFreeUseExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *blockingFreeUseExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *blockingFreeUseExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *blockingFreeUseExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *blockingFreeUseExecutor) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func scheduledFreeAuthExpiryAt(t *testing.T, mgr *Manager, authID string) time.Time {
	t.Helper()
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	item := mgr.freeAuthExpiryIndex[authID]
	if item == nil {
		t.Fatalf("expected free auth expiry entry for %s", authID)
	}
	return item.expireAt
}

func waitForDeletedCount(t *testing.T, store *deletingStore, want int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := store.Deleted()
		if len(got) == want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected %d deleted ids, got %d (%v)", want, len(got), got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestManager_Execute_NotesFreeCodexUseBeforeResultWriteback(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	exec := &blockingFreeUseExecutor{id: "codex", started: make(chan struct{})}
	mgr.RegisterExecutor(exec)

	auth := newPersistedFreeCodexAuth("auths/free-start-accounting.json")
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := mgr.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
		done <- err
	}()

	select {
	case <-exec.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream execute start")
	}

	cancel()

	select {
	case err := <-done:
		if err == nil || err != context.Canceled {
			t.Fatalf("Execute error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Execute did not return after context cancellation")
	}

	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected free auth to remain registered after canceled attempt")
	}
	if got, ok := stored.Metadata["cliproxy_free_call_count"]; ok {
		t.Fatalf("free call count metadata should remain unset, got %v", got)
	}
	firstUsedAt, ok := freeCodexFirstUsedAt(stored)
	if !ok {
		t.Fatal("expected first use timestamp to be recorded before result writeback")
	}
	expireAt := scheduledFreeAuthExpiryAt(t, mgr, auth.ID)
	if !expireAt.Equal(firstUsedAt.Add(freeCodexAuthTTL)) {
		t.Fatalf("expiry time = %s, want %s", expireAt.Format(time.RFC3339Nano), firstUsedAt.Add(freeCodexAuthTTL).Format(time.RFC3339Nano))
	}
	if deleted := store.Deleted(); len(deleted) != 0 {
		t.Fatalf("expected no deleted auths, got %v", deleted)
	}
}

func TestManager_Execute_DoesNotDeleteFreeCodexAuthWhenLegacyCallCountMetadataIsPresent(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	exec := &quotaProbeTestExecutor{
		id: "codex",
		execute: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			return cliproxyexecutor.Response{}, nil
		},
	}
	mgr.RegisterExecutor(exec)

	auth := newPersistedFreeCodexAuth("auths/free-start-delete-limit.json")
	auth.Metadata["cliproxy_free_call_count"] = 9999
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	resp, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if len(resp.Payload) != 0 {
		t.Fatalf("expected empty payload, got %q", string(resp.Payload))
	}
	if exec.attempts != 1 {
		t.Fatalf("expected upstream execute to run once, got %d attempts", exec.attempts)
	}
	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected free auth to remain registered")
	}
	if got := stored.Metadata["cliproxy_free_call_count"]; got != 9999 {
		t.Fatalf("legacy free call count metadata = %v, want unchanged 9999", got)
	}
	if deleted := store.Deleted(); len(deleted) != 0 {
		t.Fatalf("expected no deleted auths, got %v", deleted)
	}
}

type listingDeletingStore struct {
	deletingStore
	items []*Auth
}

func (s *listingDeletingStore) List(context.Context) ([]*Auth, error) {
	out := make([]*Auth, 0, len(s.items))
	for _, item := range s.items {
		out = append(out, item.Clone())
	}
	return out, nil
}

func TestManager_Load_ExpiredFreeCodexAuthsUseBoundedWorkerCleanup(t *testing.T) {
	const authCount = 6
	store := &listingDeletingStore{}
	for i := 0; i < authCount; i++ {
		auth := newPersistedFreeCodexAuth("auths/free-expired-" + strconv.Itoa(i) + ".json")
		auth.Metadata[metadataFreeFirstUsedAtKey] = time.Now().Add(-2 * freeCodexAuthTTL).Format(time.RFC3339Nano)
		store.items = append(store.items, auth)
	}

	prevAfterFunc := freeAuthExpiryAfterFunc
	freeAuthExpiryAfterFunc = func(time.Duration, func()) *time.Timer {
		t.Fatal("free auth cleanup should not schedule per-auth timers during load")
		return nil
	}
	t.Cleanup(func() { freeAuthExpiryAfterFunc = prevAfterFunc })

	mgr := NewManager(store, nil, nil)
	if err := mgr.Load(context.Background()); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	deleted := waitForDeletedCount(t, &store.deletingStore, authCount)
	sort.Strings(deleted)
	for i := 0; i < authCount; i++ {
		want := "auths/free-expired-" + strconv.Itoa(i) + ".json"
		if deleted[i] != want {
			t.Fatalf("deleted[%d] = %q, want %q", i, deleted[i], want)
		}
	}
	if got := len(mgr.List()); got != 0 {
		t.Fatalf("expected all expired free auths removed from runtime, got %d", got)
	}
}

func TestManager_MarkResult_DeletesFreeCodexAuthOnStringOnlyCapacityMessage(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	auth := newPersistedFreeCodexAuth("auths/free-string-capacity.json")
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
		Error:    &Error{HTTPStatus: 500, Message: "Selected model is at capacity"},
	})

	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatal("expected free auth to be removed on capacity message without relying on status code")
	}
	waitForDeletedIDs(t, store, []string{auth.ID})
}

func TestManager_MarkResult_DeletesFreeCodexAuthOnStringOnlyUnauthorizedMessage(t *testing.T) {
	store := &deletingStore{}
	mgr := NewManager(store, nil, nil)
	auth := newPersistedFreeCodexAuth("auths/free-string-401.json")
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
		Error:    &Error{HTTPStatus: http.StatusOK, Message: "Unauthorized"},
	})

	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatal("expected free auth to be removed on unauthorized message without relying on status code")
	}
	waitForDeletedIDs(t, store, []string{auth.ID})
}
