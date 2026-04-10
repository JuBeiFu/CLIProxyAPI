package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	context "golang.org/x/net/context"
)

func TestRequestExecutionMetadata_UsesSessionIDForNewAPIWebsocketBridge(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request.Header.Set("X-NewAPI-Downstream-Transport", "websocket")
	ginCtx.Request.Header.Set("Session_id", "sess-123")

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	meta := requestExecutionMetadata(ctx)

	got, ok := meta[coreexecutor.ExecutionSessionMetadataKey].(string)
	if !ok {
		t.Fatalf("expected execution session metadata to be a string, got %#v", meta[coreexecutor.ExecutionSessionMetadataKey])
	}
	if got != "sess-123" {
		t.Fatalf("execution_session_id = %q, want %q", got, "sess-123")
	}
}

func TestRequestExecutionMetadata_DoesNotUseSessionIDForNewAPISSEBridge(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request.Header.Set("X-NewAPI-Downstream-Transport", "sse")
	ginCtx.Request.Header.Set("Session_id", "sess-123")

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	meta := requestExecutionMetadata(ctx)

	if _, exists := meta[coreexecutor.ExecutionSessionMetadataKey]; exists {
		t.Fatalf("execution_session_id should not be set for sse bridge: %#v", meta)
	}
}

func TestRequestExecutionMetadata_PrefersExplicitExecutionSessionIDOverBridgeSessionID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request.Header.Set("X-NewAPI-Downstream-Transport", "websocket")
	ginCtx.Request.Header.Set("Session_id", "sess-123")

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	ctx = WithExecutionSessionID(ctx, "explicit-session")
	meta := requestExecutionMetadata(ctx)

	got, ok := meta[coreexecutor.ExecutionSessionMetadataKey].(string)
	if !ok {
		t.Fatalf("expected execution session metadata to be a string, got %#v", meta[coreexecutor.ExecutionSessionMetadataKey])
	}
	if got != "explicit-session" {
		t.Fatalf("execution_session_id = %q, want %q", got, "explicit-session")
	}
}
