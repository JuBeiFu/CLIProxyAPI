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

func TestRequestAuthSessionIDExtractsOfficialHeaderSources(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name   string
		header string
		value  string
		want   string
	}{
		{name: "x-session-id", header: "X-Session-ID", value: "xsess-1", want: "header:xsess-1"},
		{name: "codex-session-id", header: "Session_id", value: "sess-1", want: "codex:sess-1"},
		{name: "amp-thread-id", header: "X-Amp-Thread-Id", value: "amp-1", want: "amp:amp-1"},
		{name: "client-request-id", header: "X-Client-Request-Id", value: "req-1", want: "clientreq:req-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Request.Header.Set(tt.header, tt.value)
			ctx := context.WithValue(context.Background(), "gin", c)

			if got := requestAuthSessionID(ctx, []byte(`{"model":"gpt-5.4"}`)); got != tt.want {
				t.Fatalf("requestAuthSessionID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRequestAuthSessionIDExtractsClaudeMetadataSession(t *testing.T) {
	rawJSON := []byte(`{"metadata":{"user_id":"{\"session_id\":\"claude-session-1\"}"}}`)

	if got := requestAuthSessionID(context.Background(), rawJSON); got != "claude:claude-session-1" {
		t.Fatalf("requestAuthSessionID() = %q, want claude session", got)
	}
}

func TestRequestAuthSessionIDExtractsBodyFallbacks(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "metadata-user", body: `{"metadata":{"user_id":"user-stable-1"}}`, want: "user:user-stable-1"},
		{name: "session-id", body: `{"session_id":"sess-body-1"}`, want: "codex:sess-body-1"},
		{name: "conversation-id", body: `{"conversation_id":"conv-body-1"}`, want: "conv:conv-body-1"},
		{name: "prompt-cache-key", body: `{"prompt_cache_key":"pc-body-1"}`, want: "codex:pc-body-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := requestAuthSessionID(context.Background(), []byte(tt.body)); got != tt.want {
				t.Fatalf("requestAuthSessionID() = %q, want %q", got, tt.want)
			}
		})
	}
}

var _ interfaces.APIHandler = noopAPIHandler{}
