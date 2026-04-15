package auth

import (
	"context"
	"sync"
	"testing"
	"time"
)

type mockPersister struct {
	mu      sync.Mutex
	written map[string]QuotaState
}

func (m *mockPersister) PersistQuotaState(authID string, quota QuotaState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.written == nil {
		m.written = make(map[string]QuotaState)
	}
	m.written[authID] = quota
	return nil
}

func (m *mockPersister) getWritten(authID string) (QuotaState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q, ok := m.written[authID]
	return q, ok
}

func TestQuotaFlusher_FlushesChangedQuota(t *testing.T) {
	persister := &mockPersister{}
	m := NewManager(nil, nil, nil)
	now := time.Now()

	auth := &Auth{
		ID:       "auth-flush",
		Provider: "codex",
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "usage_limit",
			NextRecoverAt: now.Add(2 * time.Hour),
			UpdatedAt:     now,
		},
	}
	_, _ = m.Register(nil, auth)

	flusher := newQuotaFlusher(m, persister)
	flusher.flushOnce()

	q, ok := persister.getWritten("auth-flush")
	if !ok {
		t.Fatal("expected auth-flush to be flushed")
	}
	if !q.Exceeded {
		t.Error("expected Exceeded = true in flushed state")
	}
	if q.Reason != "usage_limit" {
		t.Errorf("expected reason=usage_limit, got %q", q.Reason)
	}
}

func TestQuotaFlusher_SkipsUnchangedQuota(t *testing.T) {
	persister := &mockPersister{}
	m := NewManager(nil, nil, nil)

	auth := &Auth{
		ID:       "auth-unchanged",
		Provider: "codex",
		Quota:    QuotaState{},
	}
	_, _ = m.Register(nil, auth)

	flusher := newQuotaFlusher(m, persister)
	flusher.flushOnce()

	if _, ok := persister.getWritten("auth-unchanged"); ok {
		t.Error("expected unchanged (zero) quota to NOT be flushed")
	}
}

func TestQuotaFlusher_SkipsExpiredQuota(t *testing.T) {
	persister := &mockPersister{}
	m := NewManager(nil, nil, nil)

	auth := &Auth{
		ID:       "auth-expired",
		Provider: "codex",
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "usage_limit",
			NextRecoverAt: time.Now().Add(-1 * time.Hour),
			UpdatedAt:     time.Now().Add(-2 * time.Hour),
		},
	}
	_, _ = m.Register(nil, auth)

	flusher := newQuotaFlusher(m, persister)
	flusher.flushOnce()

	if _, ok := persister.getWritten("auth-expired"); ok {
		t.Error("expected expired quota to NOT be flushed")
	}
}

func TestQuotaFlusher_AdaptiveInterval(t *testing.T) {
	persister := &mockPersister{}
	m := NewManager(nil, nil, nil)
	flusher := newQuotaFlusher(m, persister)

	if flusher.currentInterval() != flusher.baseInterval {
		t.Errorf("expected base interval initially, got %v", flusher.currentInterval())
	}

	flusher.notifyActivity()
	time.Sleep(10 * time.Millisecond)
	flusher.notifyActivity()

	if flusher.currentInterval() != flusher.fastInterval {
		t.Errorf("expected fast interval after activity burst, got %v", flusher.currentInterval())
	}
}

func TestQuotaFlusher_SkipsDuplicateFlush(t *testing.T) {
	persister := &mockPersister{written: make(map[string]QuotaState)}
	m := NewManager(nil, nil, nil)
	now := time.Now()

	auth := &Auth{
		ID:       "auth-dup",
		Provider: "codex",
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "usage_limit",
			NextRecoverAt: now.Add(2 * time.Hour),
			UpdatedAt:     now,
		},
	}
	_, _ = m.Register(nil, auth)

	flusher := newQuotaFlusher(m, persister)
	flusher.flushOnce()

	if _, ok := persister.getWritten("auth-dup"); !ok {
		t.Fatal("expected first flush to write")
	}

	// Clear and flush again — should NOT write since snapshot matches
	persister.mu.Lock()
	delete(persister.written, "auth-dup")
	persister.mu.Unlock()

	flusher.flushOnce()

	if _, ok := persister.getWritten("auth-dup"); ok {
		t.Error("expected second flush to skip unchanged quota")
	}
}

func TestManager_StartQuotaFlusher(t *testing.T) {
	persister := &mockPersister{written: make(map[string]QuotaState)}
	m := NewManager(nil, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	m.StartQuotaFlusher(ctx, persister)

	now := time.Now()
	auth := &Auth{
		ID:       "auth-wired",
		Provider: "codex",
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "usage_limit",
			NextRecoverAt: now.Add(2 * time.Hour),
			UpdatedAt:     now,
		},
	}
	_, _ = m.Register(nil, auth)

	// Trigger manual flush
	if m.quotaFlusher != nil {
		m.quotaFlusher.flushOnce()
	}

	cancel()

	q, ok := persister.getWritten("auth-wired")
	if !ok {
		t.Fatal("expected auth to be flushed via manager")
	}
	if q.Reason != "usage_limit" {
		t.Errorf("expected reason=usage_limit, got %q", q.Reason)
	}
}
