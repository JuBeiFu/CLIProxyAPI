package auth

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type countingStore struct {
	saveCount atomic.Int32
}

func (s *countingStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *countingStore) Save(context.Context, *Auth) (string, error) {
	s.saveCount.Add(1)
	return "", nil
}

func (s *countingStore) Delete(context.Context, string) error { return nil }

func TestWithSkipPersist_DisablesUpdatePersistence(t *testing.T) {
	store := &countingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auth-1",
		Provider: "antigravity",
		Metadata: map[string]any{"type": "antigravity"},
	}

	if _, err := mgr.Update(context.Background(), auth); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if got := store.saveCount.Load(); got != 1 {
		t.Fatalf("expected 1 Save call, got %d", got)
	}

	ctxSkip := WithSkipPersist(context.Background())
	if _, err := mgr.Update(ctxSkip, auth); err != nil {
		t.Fatalf("Update(skipPersist) returned error: %v", err)
	}
	if got := store.saveCount.Load(); got != 1 {
		t.Fatalf("expected Save call count to remain 1, got %d", got)
	}
}

func TestWithSkipPersist_DisablesRegisterPersistence(t *testing.T) {
	store := &countingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auth-1",
		Provider: "antigravity",
		Metadata: map[string]any{"type": "antigravity"},
	}

	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register(skipPersist) returned error: %v", err)
	}
	if got := store.saveCount.Load(); got != 0 {
		t.Fatalf("expected 0 Save calls, got %d", got)
	}
}

type capturingStore struct {
	saveCount atomic.Int32
	lastAuth  atomic.Pointer[Auth]
}

func (s *capturingStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *capturingStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.saveCount.Add(1)
	if auth != nil {
		s.lastAuth.Store(auth.Clone())
	}
	return "", nil
}

func (s *capturingStore) Delete(context.Context, string) error { return nil }

func TestManager_Persist_StripsTransientMetadataKeys(t *testing.T) {
	store := &capturingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auth-1",
		Provider: "codex",
		Metadata: map[string]any{
			"type":                    "codex",
			metadataCooldownUntilKey:  time.Now().Add(time.Minute).Unix(),
			metadataCooldownReasonKey: "quota",
			metadataQuotaProbeLastKey: time.Now().Unix(),
			metadataQuotaProbeAfterKey: time.Now().
				Add(15 * time.Minute).
				Unix(),
			metadataAutoDisabledReasonKey: autoDisabledReasonQuotaExhausted,
		},
	}

	if _, err := mgr.Update(context.Background(), auth); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}

	saved := store.lastAuth.Load()
	if saved == nil {
		t.Fatal("expected captured auth")
	}
	if _, ok := saved.Metadata[metadataCooldownUntilKey]; ok {
		t.Fatalf("expected %q to be stripped before persistence", metadataCooldownUntilKey)
	}
	if _, ok := saved.Metadata[metadataCooldownReasonKey]; ok {
		t.Fatalf("expected %q to be stripped before persistence", metadataCooldownReasonKey)
	}
	if _, ok := saved.Metadata[metadataQuotaProbeLastKey]; ok {
		t.Fatalf("expected %q to be stripped before persistence", metadataQuotaProbeLastKey)
	}
	if _, ok := saved.Metadata[metadataQuotaProbeAfterKey]; ok {
		t.Fatalf("expected %q to be stripped before persistence", metadataQuotaProbeAfterKey)
	}
	if got := saved.Metadata[metadataAutoDisabledReasonKey]; got != autoDisabledReasonQuotaExhausted {
		t.Fatalf("expected durable auto-disabled reason to remain, got %#v", got)
	}
}

type blockingStore struct {
	saveStarted chan struct{}
	allowSave   chan struct{}
}

func (s *blockingStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *blockingStore) Save(context.Context, *Auth) (string, error) {
	select {
	case <-s.saveStarted:
	default:
		close(s.saveStarted)
	}
	<-s.allowSave
	return "", nil
}

func (s *blockingStore) Delete(context.Context, string) error { return nil }

func TestManager_MarkResult_DoesNotHoldManagerLockWhilePersisting(t *testing.T) {
	store := &blockingStore{saveStarted: make(chan struct{}), allowSave: make(chan struct{})}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auth-1",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	done := make(chan struct{})
	go func() {
		mgr.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-5", Success: true})
		close(done)
	}()

	select {
	case <-store.saveStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Save to start")
	}

	lookupDone := make(chan struct{})
	go func() {
		_, _ = mgr.GetByID(auth.ID)
		close(lookupDone)
	}()

	select {
	case <-lookupDone:
	case <-time.After(250 * time.Millisecond):
		close(store.allowSave)
		t.Fatal("GetByID blocked while Save was in progress")
	}

	close(store.allowSave)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("MarkResult did not finish after Save was released")
	}
}
