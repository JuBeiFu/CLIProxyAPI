package weblogin

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// The login retry must give a flaky web login a few chances (ErrLoginTransient)
// while never retrying a terminal ban (ErrAccountBanned), and must abort on a
// cancelled context without burning an attempt.
func TestSessionLoginWithRetry(t *testing.T) {
	ctx := context.Background()
	ok := &SessionResult{AccessToken: "tok"}

	t.Run("success on first attempt", func(t *testing.T) {
		calls := 0
		got, err := sessionLoginWithRetry(ctx, 3, func(context.Context) (*SessionResult, error) {
			calls++
			return ok, nil
		})
		if err != nil || got != ok || calls != 1 {
			t.Fatalf("got=%v err=%v calls=%d, want ok in 1 call", got, err, calls)
		}
	})

	t.Run("transient twice then success", func(t *testing.T) {
		calls := 0
		got, err := sessionLoginWithRetry(ctx, 3, func(context.Context) (*SessionResult, error) {
			calls++
			if calls < 3 {
				return nil, fmt.Errorf("%w: cf gate", ErrLoginTransient)
			}
			return ok, nil
		})
		if err != nil || got != ok || calls != 3 {
			t.Fatalf("got=%v err=%v calls=%d, want ok on 3rd attempt", got, err, calls)
		}
	})

	t.Run("transient on every attempt exhausts after N", func(t *testing.T) {
		calls := 0
		_, err := sessionLoginWithRetry(ctx, 3, func(context.Context) (*SessionResult, error) {
			calls++
			return nil, fmt.Errorf("%w: cf gate", ErrLoginTransient)
		})
		if !errors.Is(err, ErrLoginTransient) || calls != 3 {
			t.Fatalf("err=%v calls=%d, want transient after exactly 3 attempts", err, calls)
		}
	})

	t.Run("banned is terminal and never retried", func(t *testing.T) {
		calls := 0
		_, err := sessionLoginWithRetry(ctx, 3, func(context.Context) (*SessionResult, error) {
			calls++
			return nil, ErrAccountBanned
		})
		if !errors.Is(err, ErrAccountBanned) || calls != 1 {
			t.Fatalf("err=%v calls=%d, want immediate ban in 1 call", err, calls)
		}
	})

	t.Run("cancelled context aborts without an attempt", func(t *testing.T) {
		cctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		_, err := sessionLoginWithRetry(cctx, 3, func(context.Context) (*SessionResult, error) {
			calls++
			return ok, nil
		})
		if !errors.Is(err, context.Canceled) || calls != 0 {
			t.Fatalf("err=%v calls=%d, want context.Canceled and 0 calls", err, calls)
		}
	})
}
