package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type testRetryAfterStatusErr struct {
	status     int
	retryAfter time.Duration
}

func (e *testRetryAfterStatusErr) Error() string              { return "test status error" }
func (e *testRetryAfterStatusErr) StatusCode() int            { return e.status }
func (e *testRetryAfterStatusErr) RetryAfter() *time.Duration { return &e.retryAfter }

func TestWrapStreamResult_PropagatesRetryAfterFromStreamError(t *testing.T) {
	// Ensure quota cooldown is enabled so that Retry-After propagates into NextRetryAfter.
	SetQuotaCooldownDisabled(false)
	defer SetQuotaCooldownDisabled(false)

	ctx := context.Background()
	mgr := NewManager(nil, nil, nil)
	_, err := mgr.Register(ctx, &Auth{ID: "auth-1"})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	base := &testRetryAfterStatusErr{status: http.StatusTooManyRequests, retryAfter: 2 * time.Hour}
	wrapped := fmt.Errorf("wrapped: %w", base)

	remaining := make(chan cliproxyexecutor.StreamChunk, 1)
	remaining <- cliproxyexecutor.StreamChunk{Err: wrapped}
	close(remaining)

	stream := mgr.wrapStreamResult(ctx, &Auth{ID: "auth-1"}, "codex", "gpt-5", 0, http.Header{}, nil, remaining)
	for range stream.Chunks {
	}

	got, ok := mgr.GetByID("auth-1")
	if !ok || got == nil {
		t.Fatalf("expected auth to exist after MarkResult")
	}

	state := (*ModelState)(nil)
	if got.ModelStates != nil {
		state = got.ModelStates["gpt-5"]
	}
	if state == nil {
		t.Fatalf("expected model state to be created")
	}
	if !state.Quota.Exceeded {
		t.Fatalf("expected quota to be exceeded, got %+v", state.Quota)
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatalf("expected NextRetryAfter to be scheduled from Retry-After")
	}
	// Should be close to the provided retry-after (allow some elapsed time).
	if time.Until(state.NextRetryAfter) < time.Hour {
		t.Fatalf("expected NextRetryAfter >= ~1h in future, got %v", time.Until(state.NextRetryAfter))
	}
}
