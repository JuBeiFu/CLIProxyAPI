package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// asyncReloginTestExecutor signals on `entered` when Refresh begins, then blocks
// on `release` until the test lets it finish. The block lets the test prove
// kickAsyncRelogin is non-blocking without relying on wall-clock timing.
type asyncReloginTestExecutor struct {
	id      string
	entered chan string
	release chan struct{}
}

func (e *asyncReloginTestExecutor) Identifier() string { return e.id }
func (e *asyncReloginTestExecutor) Execute(ctx context.Context, a *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *asyncReloginTestExecutor) ExecuteStream(ctx context.Context, a *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}
func (e *asyncReloginTestExecutor) CountTokens(ctx context.Context, a *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *asyncReloginTestExecutor) HttpRequest(ctx context.Context, a *Auth, r *http.Request) (*http.Response, error) {
	return nil, nil
}
func (e *asyncReloginTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	select {
	case e.entered <- auth.ID:
	default:
	}
	<-e.release
	return auth, nil
}

// kickAsyncRelogin must (1) be non-blocking — never make the caller wait on the
// multi-second web login — and (2) run the login on a context detached from the
// triggering request, so a request that completes/fails over (cancelling its
// ctx) does not abort the recovery. Refresh blocks until the test releases it:
// reaching the assertion below while Refresh is still blocked proves the kick
// returned without waiting, and `entered` firing under an already-cancelled ctx
// proves the login runs detached.
func TestKickAsyncReloginIsNonBlockingAndDetached(t *testing.T) {
	m := NewManager(nil, nil, nil)
	exec := &asyncReloginTestExecutor{
		id:      "codex",
		entered: make(chan string, 1),
		release: make(chan struct{}),
	}
	m.RegisterExecutor(exec)
	defer close(exec.release) // always unblock the background Refresh

	a := mustAuthKeyed("acc1", "codex", "plus", "plus", false)
	a.Metadata["email"] = "a@x.com"
	a.Metadata["openai_password"] = "p"
	a.Metadata["totp_secret"] = "s"
	m.auths = map[string]*Auth{"acc1": a}

	// Already-cancelled request context: a non-detached implementation would
	// abort and never reach Refresh.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Non-blocking: this returns even though the background Refresh is parked on
	// exec.release. If it blocked, control would never reach the select below.
	m.kickAsyncRelogin(ctx, a, &Error{HTTPStatus: 401, Message: "unauthorized"})

	select {
	case id := <-exec.entered:
		if id != "acc1" {
			t.Fatalf("background relogin refreshed %q, want acc1", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("background relogin did not start within 10s (not kicked, or request-ctx cancellation aborted it)")
	}
}
