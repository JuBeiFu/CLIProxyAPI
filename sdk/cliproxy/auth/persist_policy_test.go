package auth

import (
	"context"
	"sync/atomic"
	"testing"
)

type countingStore struct {
	saveCount   atomic.Int32
	deleteCount atomic.Int32
}

func (s *countingStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *countingStore) Save(context.Context, *Auth) (string, error) {
	s.saveCount.Add(1)
	return "", nil
}

func (s *countingStore) Delete(context.Context, string) error {
	s.deleteCount.Add(1)
	return nil
}

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

func TestWithSkipPersist_DisablesRemovePersistence(t *testing.T) {
	store := &countingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auth-remove",
		Provider: "codex",
		FileName: "auth-remove.json",
		Metadata: map[string]any{"type": "codex"},
	}

	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register(skipPersist) returned error: %v", err)
	}
	if _, err := mgr.Remove(WithSkipPersist(context.Background()), auth.ID); err != nil {
		t.Fatalf("Remove(skipPersist) returned error: %v", err)
	}
	if got := store.deleteCount.Load(); got != 0 {
		t.Fatalf("expected 0 Delete calls, got %d", got)
	}

	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("second Register(skipPersist) returned error: %v", err)
	}
	if _, err := mgr.Remove(context.Background(), auth.ID); err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	if got := store.deleteCount.Load(); got != 1 {
		t.Fatalf("expected 1 Delete call, got %d", got)
	}
}

func TestManager_MarkResult_SuccessWithoutPriorState_DoesNotPersistOrCreateModelState(t *testing.T) {
	store := &countingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auth-success-clean",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
	}

	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register(skipPersist) returned error: %v", err)
	}

	mgr.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5.4",
		Success:  true,
	})

	if got := store.saveCount.Load(); got != 0 {
		t.Fatalf("expected 0 Save calls for steady-state success, got %d", got)
	}

	updated, ok := mgr.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to remain present")
	}
	if len(updated.ModelStates) != 0 {
		t.Fatalf("expected no model state to be created for steady-state success, got %d entries", len(updated.ModelStates))
	}
}

func TestManager_MarkResult_SuccessAfterFailure_PersistsRecoveredState(t *testing.T) {
	store := &countingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auth-success-recovery",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
	}

	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register(skipPersist) returned error: %v", err)
	}

	mgr.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5.4",
		Success:  false,
		Error:    &Error{HTTPStatus: 429, Message: "quota"},
	})
	if got := store.saveCount.Load(); got != 1 {
		t.Fatalf("expected failure to persist state once, got %d saves", got)
	}

	mgr.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5.4",
		Success:  true,
	})
	if got := store.saveCount.Load(); got != 2 {
		t.Fatalf("expected recovery success to persist cleared state once, got %d saves", got)
	}
}
