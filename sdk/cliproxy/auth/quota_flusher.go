package auth

import (
	"context"
	"sync"
	"time"
)

// QuotaPersister writes quota state to durable storage.
type QuotaPersister interface {
	PersistQuotaState(authID string, quota QuotaState) error
}

type quotaFlusher struct {
	manager        *Manager
	persister      QuotaPersister
	baseInterval   time.Duration
	fastInterval   time.Duration
	activityWindow time.Duration

	mu            sync.Mutex
	lastActivity  time.Time
	activityCount int
	lastSnapshot  map[string]QuotaState
}

func newQuotaFlusher(manager *Manager, persister QuotaPersister) *quotaFlusher {
	return &quotaFlusher{
		manager:        manager,
		persister:      persister,
		baseInterval:   5 * time.Minute,
		fastInterval:   1 * time.Minute,
		activityWindow: 2 * time.Minute,
		lastSnapshot:   make(map[string]QuotaState),
	}
}

// notifyActivity records a quota change event for adaptive interval calculation.
func (f *quotaFlusher) notifyActivity() {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	if !f.lastActivity.IsZero() && now.Sub(f.lastActivity) > f.activityWindow {
		f.activityCount = 0
	}
	f.lastActivity = now
	f.activityCount++
}

// currentInterval returns the flush interval based on recent activity.
func (f *quotaFlusher) currentInterval() time.Duration {
	if f == nil {
		return 5 * time.Minute
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.activityCount >= 2 && time.Since(f.lastActivity) < f.activityWindow {
		return f.fastInterval
	}
	return f.baseInterval
}

// Run starts the periodic flush loop. Blocks until ctx is cancelled.
func (f *quotaFlusher) Run(ctx context.Context) {
	if f == nil || f.persister == nil {
		return
	}
	timer := time.NewTimer(f.currentInterval())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			f.flushOnce()
			return
		case <-timer.C:
			f.flushOnce()
			timer.Reset(f.currentInterval())
		}
	}
}

// flushOnce iterates all auths and persists any quota state that changed since last flush.
func (f *quotaFlusher) flushOnce() {
	if f == nil || f.manager == nil || f.persister == nil {
		return
	}
	auths := f.manager.snapshotAuths()
	now := time.Now()
	for _, a := range auths {
		if a == nil {
			continue
		}
		quota := a.Quota
		if !f.shouldFlush(a.ID, quota, now) {
			continue
		}
		if err := f.persister.PersistQuotaState(a.ID, quota); err != nil {
			continue
		}
		f.mu.Lock()
		f.lastSnapshot[a.ID] = quota
		f.mu.Unlock()
	}
}

func (f *quotaFlusher) shouldFlush(authID string, quota QuotaState, now time.Time) bool {
	if !quota.Exceeded && quota.Reason == "" && quota.NextRecoverAt.IsZero() && quota.BackoffLevel == 0 {
		return false
	}
	if !quota.NextRecoverAt.IsZero() && quota.NextRecoverAt.Before(now) {
		return false
	}
	f.mu.Lock()
	prev, exists := f.lastSnapshot[authID]
	f.mu.Unlock()
	if !exists {
		return true
	}
	return prev.Exceeded != quota.Exceeded ||
		prev.Reason != quota.Reason ||
		!prev.NextRecoverAt.Equal(quota.NextRecoverAt) ||
		prev.BackoffLevel != quota.BackoffLevel
}
