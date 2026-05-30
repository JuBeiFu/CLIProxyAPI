package auth

import (
	"context"
	"testing"
	"time"
)

// kickAsyncRelogin must (1) return immediately — never block the caller on the
// multi-second web login — and (2) run the login on a context detached from the
// triggering request, so a request that completes/fails over (cancelling its
// ctx) does not abort the recovery. We pass an already-cancelled ctx and still
// expect the background Refresh to fire.
func TestKickAsyncReloginIsNonBlockingAndDetached(t *testing.T) {
	m := NewManager(nil, nil, nil)
	exec := &forcedRefreshTestExecutor{id: "codex", probeResults: map[string]string{}}
	m.RegisterExecutor(exec)

	a := mustAuthKeyed("acc1", "codex", "plus", "plus", false)
	a.Metadata["email"] = "a@x.com"
	a.Metadata["openai_password"] = "p"
	a.Metadata["totp_secret"] = "s"
	m.auths = map[string]*Auth{"acc1": a}

	// Already-cancelled request context: a non-detached implementation would
	// abort here and never call Refresh.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	m.kickAsyncRelogin(ctx, a, &Error{HTTPStatus: 401, Message: "unauthorized"})
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("kickAsyncRelogin blocked for %s; must return immediately", elapsed)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := exec.refreshed(); len(got) == 1 && got[0] == "acc1" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("background relogin did not call Refresh on acc1 within 2s; refreshed=%v", exec.refreshed())
}
