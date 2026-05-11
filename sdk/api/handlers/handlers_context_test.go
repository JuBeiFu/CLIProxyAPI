package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

type noopAPIHandler struct{}

func (noopAPIHandler) HandlerType() string      { return "noop" }
func (noopAPIHandler) Models() []map[string]any { return nil }

func TestGetContextWithCancel_DoesNotInjectDownstreamWebsocketForBridgeHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("X-NewAPI-Downstream-Transport", "websocket")

	base := NewBaseAPIHandlers(&sdkconfig.SDKConfig{RequestLog: true}, nil)
	ctx, cancel := base.GetContextWithCancel(noopAPIHandler{}, c, context.Background())
	cancel(nil)

	if coreexecutor.DownstreamWebsocket(ctx) {
		t.Fatal("expected bridge websocket header to keep upstream websocket disabled by default")
	}
	if !DownstreamWebsocketBridge(ctx) {
		t.Fatal("expected bridge websocket header to remain visible as downstream bridge metadata")
	}
}

func TestGetContextWithCancel_DoesNotInjectDownstreamWebsocketForSSEBridgeHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("X-NewAPI-Downstream-Transport", "sse")

	base := NewBaseAPIHandlers(&sdkconfig.SDKConfig{RequestLog: true}, nil)
	ctx, cancel := base.GetContextWithCancel(noopAPIHandler{}, c, context.Background())
	cancel(nil)

	if coreexecutor.DownstreamWebsocket(ctx) {
		t.Fatal("expected sse bridge header to keep logical downstream websocket disabled")
	}
}

func TestRequestExecutionMetadata_IncludesCLIProxyRetryAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("X-NewAPI-CLIProxy-Retry-Attempt", "3")

	ctx := context.WithValue(context.Background(), "gin", c)
	meta := requestExecutionMetadata(ctx)

	if got, ok := meta[coreexecutor.ExternalRetryAttemptMetadataKey].(int); !ok || got != 3 {
		t.Fatalf("retry attempt metadata = %#v, want int(3)", meta[coreexecutor.ExternalRetryAttemptMetadataKey])
	}
}

var _ interfaces.APIHandler = noopAPIHandler{}
