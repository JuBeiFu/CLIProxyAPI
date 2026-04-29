package executor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestBuildCodexWebsocketRequestBodyPreservesPreviousResponseID(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-1"}]}`)

	wsReqBody := buildCodexWebsocketRequestBody(body)

	if got := gjson.GetBytes(wsReqBody, "type").String(); got != "response.create" {
		t.Fatalf("type = %s, want response.create", got)
	}
	if got := gjson.GetBytes(wsReqBody, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %s, want resp-1", got)
	}
	if gjson.GetBytes(wsReqBody, "input.0.id").String() != "msg-1" {
		t.Fatalf("input item id mismatch")
	}
	if got := gjson.GetBytes(wsReqBody, "type").String(); got == "response.append" {
		t.Fatalf("unexpected websocket request type: %s", got)
	}
}

func TestApplyCodexWebsocketHeadersDefaultsToCurrentResponsesBeta(t *testing.T) {
	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, nil, "", nil)

	if got := headers.Get("OpenAI-Beta"); got != codexResponsesWebsocketBetaHeaderValue {
		t.Fatalf("OpenAI-Beta = %s, want %s", got, codexResponsesWebsocketBetaHeaderValue)
	}
	if got := headers.Get("User-Agent"); got != "" {
		t.Fatalf("User-Agent = %s, want empty", got)
	}
	if got := headers.Get("Version"); got != "" {
		t.Fatalf("Version = %q, want empty", got)
	}
	if got := headers.Get("x-codex-beta-features"); got != "" {
		t.Fatalf("x-codex-beta-features = %q, want empty", got)
	}
	if got := headers.Get("X-Codex-Turn-Metadata"); got != "" {
		t.Fatalf("X-Codex-Turn-Metadata = %q, want empty", got)
	}
	if got := headers.Get("X-Client-Request-Id"); got != "" {
		t.Fatalf("X-Client-Request-Id = %q, want empty", got)
	}
}

func TestApplyCodexWebsocketHeadersPassesThroughClientIdentityHeaders(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"Originator":            "Codex Desktop",
		"Version":               "0.115.0-alpha.27",
		"X-Codex-Turn-Metadata": `{"turn_id":"turn-1"}`,
		"X-Client-Request-Id":   "019d2233-e240-7162-992d-38df0a2a0e0d",
	})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "", nil)

	if got := headers.Get("Originator"); got != "Codex Desktop" {
		t.Fatalf("Originator = %s, want %s", got, "Codex Desktop")
	}
	if got := headers.Get("Version"); got != "0.115.0-alpha.27" {
		t.Fatalf("Version = %s, want %s", got, "0.115.0-alpha.27")
	}
	if got := headers.Get("X-Codex-Turn-Metadata"); got != `{"turn_id":"turn-1"}` {
		t.Fatalf("X-Codex-Turn-Metadata = %s, want %s", got, `{"turn_id":"turn-1"}`)
	}
	if got := headers.Get("X-Client-Request-Id"); got != "019d2233-e240-7162-992d-38df0a2a0e0d" {
		t.Fatalf("X-Client-Request-Id = %s, want %s", got, "019d2233-e240-7162-992d-38df0a2a0e0d")
	}
}

func TestApplyCodexWebsocketHeadersUsesConfigDefaultsForOAuth(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "my-codex-client/1.0",
			BetaFeatures: "feature-a,feature-b",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}

	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "", cfg)

	if got := headers.Get("User-Agent"); got != "" {
		t.Fatalf("User-Agent = %s, want empty", got)
	}
	if got := headers.Get("x-codex-beta-features"); got != "feature-a,feature-b" {
		t.Fatalf("x-codex-beta-features = %s, want %s", got, "feature-a,feature-b")
	}
	if got := headers.Get("OpenAI-Beta"); got != codexResponsesWebsocketBetaHeaderValue {
		t.Fatalf("OpenAI-Beta = %s, want %s", got, codexResponsesWebsocketBetaHeaderValue)
	}
}

func TestApplyCodexWebsocketHeadersPrefersExistingHeadersOverClientAndConfig(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent":            "client-ua",
		"X-Codex-Beta-Features": "client-beta",
	})
	headers := http.Header{}
	headers.Set("User-Agent", "existing-ua")
	headers.Set("X-Codex-Beta-Features", "existing-beta")

	got := applyCodexWebsocketHeaders(ctx, headers, auth, "", cfg)

	if gotVal := got.Get("User-Agent"); gotVal != "" {
		t.Fatalf("User-Agent = %s, want empty", gotVal)
	}
	if gotVal := got.Get("x-codex-beta-features"); gotVal != "existing-beta" {
		t.Fatalf("x-codex-beta-features = %s, want %s", gotVal, "existing-beta")
	}
}

func TestApplyCodexWebsocketHeadersConfigUserAgentOverridesClientHeader(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent":            "client-ua",
		"X-Codex-Beta-Features": "client-beta",
	})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "", cfg)

	if got := headers.Get("User-Agent"); got != "" {
		t.Fatalf("User-Agent = %s, want empty", got)
	}
	if got := headers.Get("x-codex-beta-features"); got != "client-beta" {
		t.Fatalf("x-codex-beta-features = %s, want %s", got, "client-beta")
	}
}

func TestApplyCodexWebsocketHeadersIgnoresConfigForAPIKeyAuth(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"api_key": "sk-test"},
	}

	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "sk-test", cfg)

	if got := headers.Get("User-Agent"); got != "" {
		t.Fatalf("User-Agent = %s, want empty", got)
	}
	if got := headers.Get("x-codex-beta-features"); got != "" {
		t.Fatalf("x-codex-beta-features = %q, want empty", got)
	}
}

func TestApplyCodexHeadersUsesConfigUserAgentForOAuth(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"User-Agent": "client-ua",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, cfg)

	if got := req.Header.Get("User-Agent"); got != "config-ua" {
		t.Fatalf("User-Agent = %s, want %s", got, "config-ua")
	}
	if got := req.Header.Get("x-codex-beta-features"); got != "" {
		t.Fatalf("x-codex-beta-features = %q, want empty", got)
	}
}

func TestApplyCodexHeadersPassesThroughClientIdentityHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"Originator":            "Codex Desktop",
		"Version":               "0.115.0-alpha.27",
		"X-Codex-Turn-Metadata": `{"turn_id":"turn-1"}`,
		"X-Client-Request-Id":   "019d2233-e240-7162-992d-38df0a2a0e0d",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	if got := req.Header.Get("Originator"); got != "Codex Desktop" {
		t.Fatalf("Originator = %s, want %s", got, "Codex Desktop")
	}
	if got := req.Header.Get("Version"); got != "0.115.0-alpha.27" {
		t.Fatalf("Version = %s, want %s", got, "0.115.0-alpha.27")
	}
	if got := req.Header.Get("X-Codex-Turn-Metadata"); got != `{"turn_id":"turn-1"}` {
		t.Fatalf("X-Codex-Turn-Metadata = %s, want %s", got, `{"turn_id":"turn-1"}`)
	}
	if got := req.Header.Get("X-Client-Request-Id"); got != "019d2233-e240-7162-992d-38df0a2a0e0d" {
		t.Fatalf("X-Client-Request-Id = %s, want %s", got, "019d2233-e240-7162-992d-38df0a2a0e0d")
	}
}

func TestApplyCodexHeadersDoesNotInjectClientOnlyHeadersByDefault(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}

	applyCodexHeaders(req, nil, "oauth-token", true, nil)

	if got := req.Header.Get("Version"); got != "" {
		t.Fatalf("Version = %q, want empty", got)
	}
	if got := req.Header.Get("X-Codex-Turn-Metadata"); got != "" {
		t.Fatalf("X-Codex-Turn-Metadata = %q, want empty", got)
	}
	if got := req.Header.Get("X-Client-Request-Id"); got != "" {
		t.Fatalf("X-Client-Request-Id = %q, want empty", got)
	}
}

func contextWithGinHeaders(headers map[string]string) context.Context {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	ginCtx.Request.Header = make(http.Header, len(headers))
	for key, value := range headers {
		ginCtx.Request.Header.Set(key, value)
	}
	return context.WithValue(context.Background(), "gin", ginCtx)
}

func TestNewProxyAwareWebsocketDialerDirectDisablesProxy(t *testing.T) {
	t.Parallel()

	dialer := newProxyAwareWebsocketDialer(
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
	)

	if dialer.Proxy != nil {
		t.Fatal("expected websocket proxy function to be nil for direct mode")
	}
}

func TestEnrichCodexWebsocketBridgeFollowupRequestPreservesToolConfig(t *testing.T) {
	lastRequest := []byte(`{"model":"gpt-5.4","stream":true,"instructions":"be helpful","tools":[{"type":"function","name":"ping","description":"Return pong","parameters":{"type":"object","properties":{},"additionalProperties":false}}],"tool_choice":"auto","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"start"}]}]}`)
	body := []byte(`{"model":"gpt-5.4","stream":true,"previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-1","output":"pong"}]}`)

	enriched := enrichCodexWebsocketBridgeFollowupRequest(body, lastRequest)

	if got := gjson.GetBytes(enriched, "instructions").String(); got != "be helpful" {
		t.Fatalf("instructions = %q, want %q", got, "be helpful")
	}
	if got := gjson.GetBytes(enriched, "tools.0.name").String(); got != "ping" {
		t.Fatalf("tools[0].name = %q, want %q", got, "ping")
	}
	if got := gjson.GetBytes(enriched, "tool_choice").String(); got != "auto" {
		t.Fatalf("tool_choice = %q, want %q", got, "auto")
	}
}

func TestEnrichCodexWebsocketBridgeFollowupRequestRestoresBlankInstructions(t *testing.T) {
	lastRequest := []byte(`{"model":"gpt-5.4","stream":true,"instructions":"keep going until DONE","tools":[{"type":"function","name":"ping","description":"Return pong","parameters":{"type":"object","properties":{},"additionalProperties":false}}],"tool_choice":"auto","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"start"}]}]}`)
	body := []byte(`{"model":"gpt-5.4","stream":true,"previous_response_id":"resp-1","instructions":"","input":[{"type":"function_call_output","call_id":"call-1","output":"pong"}]}`)

	enriched := enrichCodexWebsocketBridgeFollowupRequest(body, lastRequest)

	if got := gjson.GetBytes(enriched, "instructions").String(); got != "keep going until DONE" {
		t.Fatalf("instructions = %q, want %q", got, "keep going until DONE")
	}
}

func TestCodexWebsocketExecuteHandshakeCapacityReturnsRetryableRateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Selected model is at capacity. Please try a different model."}}`))
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hello"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}

	_, err := exec.Execute(context.Background(), auth, req, opts)
	if err == nil {
		t.Fatal("expected handshake error")
	}

	statusCoder, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("expected status coder, got %T", err)
	}
	if got := statusCoder.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", got, http.StatusTooManyRequests)
	}
}

func TestCodexWebsocketExecuteStreamHandshakeCapacityReturnsRetryableRateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Selected model is at capacity. Please try a different model."}}`))
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hello"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}

	_, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err == nil {
		t.Fatal("expected handshake error")
	}

	statusCoder, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("expected status coder, got %T", err)
	}
	if got := statusCoder.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", got, http.StatusTooManyRequests)
	}
}

func TestCodexWebsocketExecuteHandshakeUsageLimitReturnsRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":45}}`))
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hello"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}

	_, err := exec.Execute(context.Background(), auth, req, opts)
	if err == nil {
		t.Fatal("expected handshake error")
	}

	statusErr, ok := err.(interface {
		StatusCode() int
		RetryAfter() *time.Duration
	})
	if !ok {
		t.Fatalf("expected retryable status error, got %T", err)
	}
	if got := statusErr.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", got, http.StatusTooManyRequests)
	}
	if statusErr.RetryAfter() == nil {
		t.Fatal("expected RetryAfter, got nil")
	}
	if got := *statusErr.RetryAfter(); got != 45*time.Second {
		t.Fatalf("RetryAfter = %v, want %v", got, 45*time.Second)
	}
}

func TestCodexWebsocketExecuteHandshakeRetryAfterHeaderFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"request throttled"}}`))
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hello"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}

	_, err := exec.Execute(context.Background(), auth, req, opts)
	if err == nil {
		t.Fatal("expected handshake error")
	}

	statusErr, ok := err.(interface {
		StatusCode() int
		RetryAfter() *time.Duration
	})
	if !ok {
		t.Fatalf("expected retryable status error, got %T", err)
	}
	if got := statusErr.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", got, http.StatusTooManyRequests)
	}
	if statusErr.RetryAfter() == nil {
		t.Fatal("expected RetryAfter, got nil")
	}
	if got := *statusErr.RetryAfter(); got != 30*time.Second {
		t.Fatalf("RetryAfter = %v, want %v", got, 30*time.Second)
	}
}

func TestCodexWebsocketExecuteNonStreamSynthesizesChatCompletionContent(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			http.Error(w, "upgrade required", http.StatusUpgradeRequired)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		defer conn.Close()

		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("ReadMessage() error = %v", err)
			return
		}

		frames := [][]byte{
			[]byte(`{"type":"response.output_text.delta","delta":"hello world"}`),
			[]byte(`{"type":"response.done","response":{"id":"resp-chat","created_at":1700000000,"model":"gpt-5.4","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":13,"total_tokens":20}}}`),
		}
		for _, frame := range frames {
			if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
				t.Errorf("WriteMessage() error = %v", err)
				return
			}
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":    "sk-test",
			"base_url":   server.URL,
			"websockets": "true",
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}`),
	}
	opts := cliproxyexecutor.Options{
		Stream:       false,
		SourceFormat: sdktranslator.FromString("openai"),
	}

	resp, err := exec.Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "hello world" {
		t.Fatalf("choices[0].message.content = %q, want %q; payload=%s", got, "hello world", resp.Payload)
	}
}

func TestCodexWebsocketExecuteStreamZeroUsageCompletionForwardsCompleted(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			http.Error(w, "upgrade required", http.StatusUpgradeRequired)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		defer conn.Close()

		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("ReadMessage() error = %v", err)
			return
		}

		// Simulate the "clean-but-empty" upstream: response.created followed by
		// a reasoning delta (which must NOT count as user content) and then a
		// response.completed with usage.total_tokens=0 and no actual output.
		frames := [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp-empty"}}`),
			[]byte(`{"type":"response.reasoning_summary_text.delta","delta":"thinking..."}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp-empty","model":"gpt-5.4","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`),
		}
		for _, frame := range frames {
			if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
				t.Errorf("WriteMessage() error = %v", err)
				return
			}
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "auth-zero",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":    "sk-test",
			"base_url":   server.URL,
			"websockets": "true",
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hi"}]}`),
	}
	opts := cliproxyexecutor.Options{
		Stream:       true,
		SourceFormat: sdktranslator.FromString("openai-response"),
	}

	result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if result == nil || result.Chunks == nil {
		t.Fatalf("expected stream result with chunks")
	}

	sawCompleted := false
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		if len(chunk.Payload) > 0 {
			if gjson.GetBytes(chunk.Payload, "type").String() == "response.completed" {
				sawCompleted = true
			}
		}
	}
	if !sawCompleted {
		t.Fatalf("response.completed chunk was not forwarded to client")
	}
}

func TestCodexWebsocketExecuteStreamCyberPolicyReturnsRetryableSanitizedError(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			http.Error(w, "upgrade required", http.StatusUpgradeRequired)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		defer conn.Close()

		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("ReadMessage() error = %v", err)
			return
		}

		frames := [][]byte{
			[]byte(`{"type":"response.output_text.delta","delta":"partial"}`),
			[]byte(`{"type":"response.failed","response":{"error":{"code":"cyber_policy","message":"This content was flagged for possible cybersecurity risk. To get authorized for security work, join the Trusted Access for Cyber program: https://chatgpt.com/cyber"}}}`),
		}
		for _, frame := range frames {
			if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
				t.Errorf("WriteMessage() error = %v", err)
				return
			}
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "auth-cyber",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":    "sk-test",
			"base_url":   server.URL,
			"websockets": "true",
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":"hi"}]}`),
	}
	opts := cliproxyexecutor.Options{
		Stream:       true,
		SourceFormat: sdktranslator.FromString("openai-response"),
	}

	result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if result == nil || result.Chunks == nil {
		t.Fatalf("expected stream result with chunks")
	}

	var gotErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			gotErr = chunk.Err
		}
	}
	if gotErr == nil {
		t.Fatal("expected cyber policy error chunk")
	}
	statusCoder, ok := gotErr.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("expected status coder error, got %T: %v", gotErr, gotErr)
	}
	if got := statusCoder.StatusCode(); got != http.StatusServiceUnavailable {
		t.Fatalf("StatusCode = %d, want %d", got, http.StatusServiceUnavailable)
	}
	if bytes.Contains([]byte(gotErr.Error()), []byte("Trusted Access")) || bytes.Contains([]byte(gotErr.Error()), []byte("chatgpt.com/cyber")) {
		t.Fatalf("cyber policy error leaked upstream message: %s", gotErr.Error())
	}
	if got := gjson.Get(gotErr.Error(), "error.message").String(); got != "upstream cyber policy retryable failure" {
		t.Fatalf("error.message = %q", got)
	}
}

func TestCodexWebsocketExecuteNonStreamZeroUsageCompletionReturnsResponse(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			http.Error(w, "upgrade required", http.StatusUpgradeRequired)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		defer conn.Close()

		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("ReadMessage() error = %v", err)
			return
		}

		frames := [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp-empty"}}`),
			[]byte(`{"type":"response.reasoning_summary_text.delta","delta":"thinking..."}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp-empty","model":"gpt-5.4","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`),
		}
		for _, frame := range frames {
			if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
				t.Errorf("WriteMessage() error = %v", err)
				return
			}
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "auth-zero-nonstream",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":    "sk-test",
			"base_url":   server.URL,
			"websockets": "true",
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hi"}]}`),
	}
	opts := cliproxyexecutor.Options{
		Stream:       false,
		SourceFormat: sdktranslator.FromString("openai-response"),
	}

	resp, err := exec.Execute(context.Background(), auth, req, opts)

	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "status").String(); got != "completed" {
		t.Fatalf("status = %q, want completed; payload=%s", got, resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "usage.total_tokens").Int(); got != 0 {
		t.Fatalf("usage.total_tokens = %d, want 0; payload=%s", got, resp.Payload)
	}
}

func TestCodexWebsocketExecuteNonStreamSynthesizesResponsesFunctionCallOutput(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			http.Error(w, "upgrade required", http.StatusUpgradeRequired)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		defer conn.Close()

		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("ReadMessage() error = %v", err)
			return
		}

		frames := [][]byte{
			[]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"ping"}}`),
			[]byte(`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{\"city\":"}`),
			[]byte(`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"\"Tokyo\"}"}`),
			[]byte(`{"type":"response.done","response":{"id":"resp-resp","created_at":1700000000,"model":"gpt-5.4","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":13,"total_tokens":20}}}`),
		}
		for _, frame := range frames {
			if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
				t.Errorf("WriteMessage() error = %v", err)
				return
			}
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":    "sk-test",
			"base_url":   server.URL,
			"websockets": "true",
		},
	}
	req := cliproxyexecutor.Request{
		Model: "gpt-5.4",
		Payload: []byte(`{
			"model":"gpt-5.4",
			"input":"hello",
			"tools":[{"type":"function","function":{"name":"ping","description":"Return pong","parameters":{"type":"object","properties":{},"additionalProperties":false}}}]
		}`),
	}
	opts := cliproxyexecutor.Options{
		Stream:       false,
		SourceFormat: sdktranslator.FromString("openai-response"),
	}

	resp, err := exec.Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if got := gjson.GetBytes(resp.Payload, "output.0.type").String(); got != "function_call" {
		t.Fatalf("output[0].type = %q, want %q; payload=%s", got, "function_call", resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.call_id").String(); got != "call_1" {
		t.Fatalf("output[0].call_id = %q, want %q; payload=%s", got, "call_1", resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.name").String(); got != "ping" {
		t.Fatalf("output[0].name = %q, want %q; payload=%s", got, "ping", resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.arguments").String(); got != `{"city":"Tokyo"}` {
		t.Fatalf("output[0].arguments = %q, want %q; payload=%s", got, `{"city":"Tokyo"}`, resp.Payload)
	}
}
