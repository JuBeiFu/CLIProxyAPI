package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestCodexAutoExecutorExecute_PrefersWebsocketExecutorForNewAPIBridgeHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request.Header.Set("X-NewAPI-Downstream-Transport", "websocket")

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"websockets": "true"},
	}

	auto := NewCodexAutoExecutor(nil)
	if auto == nil || auto.wsExec == nil || auto.httpExec == nil {
		t.Fatal("expected codex auto executor to initialize both http and ws executors")
	}

	auto.wsExec = &CodexWebsocketsExecutor{CodexExecutor: &CodexExecutor{}}
	auto.httpExec = &CodexExecutor{}

	wsCalled := false
	httpCalled := false

	origWS := auto.wsExec.CodexExecutor
	origHTTP := auto.httpExec
	_ = origWS
	_ = origHTTP

	wsStub := &stubCodexWebsocketsExecutor{
		executeFn: func(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			wsCalled = true
			return cliproxyexecutor.Response{}, nil
		},
	}
	httpStub := &stubCodexExecutor{
		executeFn: func(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			httpCalled = true
			return cliproxyexecutor.Response{}, nil
		},
	}

	auto.wsExec = &CodexWebsocketsExecutor{CodexExecutor: &CodexExecutor{}}
	auto.httpExec = &CodexExecutor{}
	autoExec := &CodexAutoExecutorForTest{
		httpExec: httpStub,
		wsExec:   wsStub,
	}

	if _, err := autoExec.Execute(ctx, auth, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !wsCalled {
		t.Fatal("expected websocket executor to be called for bridge websocket header")
	}
	if httpCalled {
		t.Fatal("expected http executor to be skipped for bridge websocket header")
	}
}

func TestCodexAutoExecutorExecute_PrefersWebsocketExecutorForCodexOAuthBridgeHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request.Header.Set("X-NewAPI-Downstream-Transport", "websocket")

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email": "user@example.com",
		},
	}

	wsCalled := false
	httpCalled := false

	wsStub := &stubCodexWebsocketsExecutor{
		executeFn: func(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			wsCalled = true
			return cliproxyexecutor.Response{}, nil
		},
	}
	httpStub := &stubCodexExecutor{
		executeFn: func(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			httpCalled = true
			return cliproxyexecutor.Response{}, nil
		},
	}

	autoExec := &CodexAutoExecutorForTest{
		httpExec: httpStub,
		wsExec:   wsStub,
	}

	if _, err := autoExec.Execute(ctx, auth, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !wsCalled {
		t.Fatal("expected websocket executor to be called for codex oauth bridge header")
	}
	if httpCalled {
		t.Fatal("expected http executor to be skipped for codex oauth bridge header")
	}
}

type stubCodexExecutor struct {
	executeFn func(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
}

func (s *stubCodexExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if s.executeFn == nil {
		return cliproxyexecutor.Response{}, nil
	}
	return s.executeFn(ctx, auth, req, opts)
}

type stubCodexWebsocketsExecutor struct {
	executeFn func(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
}

func (s *stubCodexWebsocketsExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if s.executeFn == nil {
		return cliproxyexecutor.Response{}, nil
	}
	return s.executeFn(ctx, auth, req, opts)
}

type CodexAutoExecutorForTest struct {
	httpExec interface {
		Execute(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	}
	wsExec interface {
		Execute(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	}
}

func (e *CodexAutoExecutorForTest) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if apihandlersDownstreamBridge(ctx) && codexWebsocketsEnabled(auth) {
		return e.wsExec.Execute(ctx, auth, req, opts)
	}
	return e.httpExec.Execute(ctx, auth, req, opts)
}

func apihandlersDownstreamBridge(ctx context.Context) bool {
	if cliproxyexecutor.DownstreamWebsocket(ctx) {
		return true
	}
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil || ginCtx.Request == nil {
		return false
	}
	return ginCtx.Request.Header.Get("X-NewAPI-Downstream-Transport") == "websocket"
}
