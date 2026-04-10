package executor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
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
func TestCodexWebsocketBridgePayloadPreservesPreviousResponseID(t *testing.T) {
	ctx := contextWithGinHeaders(map[string]string{
		"X-NewAPI-Downstream-Transport": "websocket",
	})
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"websockets": "true"},
	}
	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp-1","prompt_cache_retention":"retained","safety_identifier":"safe","input":[{"type":"function_call_output","call_id":"call-1","output":"ok"}]}`)

	sanitized := stripCodexUnsupportedResponseFields(body, shouldPreserveCodexPreviousResponseID(ctx, auth, sdktranslator.FromString("openai-response"), body))
	wsReqBody := buildCodexWebsocketRequestBody(sanitized)

	if got := gjson.GetBytes(wsReqBody, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %q, want %q", got, "resp-1")
	}
	if gjson.GetBytes(wsReqBody, "prompt_cache_retention").Exists() {
		t.Fatalf("prompt_cache_retention should be removed: %s", wsReqBody)
	}
	if gjson.GetBytes(wsReqBody, "safety_identifier").Exists() {
		t.Fatalf("safety_identifier should be removed: %s", wsReqBody)
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

func TestShouldEnrichCodexWebsocketBridgeFollowupRequestRequiresSession(t *testing.T) {
	httpBridgeCtx := contextWithGinHeaders(map[string]string{
		"X-NewAPI-Downstream-Transport": "websocket",
	})
	if !shouldEnrichCodexWebsocketBridgeFollowupRequest(httpBridgeCtx, "sess-1") {
		t.Fatal("expected HTTP websocket bridge follow-up to require enrichment")
	}

	websocketCtx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	if !shouldEnrichCodexWebsocketBridgeFollowupRequest(websocketCtx, "sess-1") {
		t.Fatal("expected downstream websocket marker to require enrichment")
	}

	if shouldEnrichCodexWebsocketBridgeFollowupRequest(httpBridgeCtx, "") {
		t.Fatal("expected empty execution session id to skip enrichment")
	}
}

func TestCodexWebsocketExecuteStreamTranslatesOpenAIResponsesRequest(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	requests := make(chan []byte, 1)
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

		msgType, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("ReadMessage() error = %v", err)
			return
		}
		if msgType != websocket.TextMessage {
			t.Errorf("unexpected message type %d", msgType)
			return
		}
		requests <- bytes.Clone(payload)

		response := []byte(`{"type":"response.completed","response":{"id":"resp-test","output":[{"type":"message","id":"msg-1","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}}`)
		if err := conn.WriteMessage(websocket.TextMessage, response); err != nil {
			t.Errorf("WriteMessage() error = %v", err)
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
			"tools":[
				{
					"type":"function",
					"function":{
						"name":"ping",
						"description":"Return pong",
						"parameters":{"type":"object","properties":{},"additionalProperties":false},
						"strict":true
					}
				},
				{
					"type":"tool_search",
					"execution":{"mode":"remote"}
				}
			],
			"tool_choice":{
				"type":"function",
				"function":{
					"name":"ping",
					"parameters":{"type":"object","properties":{}}
				}
			}
		}`),
	}
	opts := cliproxyexecutor.Options{
		Stream:       true,
		SourceFormat: sdktranslator.FromString("openai-response"),
	}

	stream, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	select {
	case payload := <-requests:
		if payload == nil {
			t.Fatal("captured websocket payload is nil")
		}
		if got := gjson.GetBytes(payload, "type").String(); got != "response.create" {
			t.Fatalf("type = %q, want response.create", got)
		}
		if got := gjson.GetBytes(payload, "stream").Bool(); !got {
			t.Fatalf("stream = %v, want true; payload=%s", got, payload)
		}
		if got := gjson.GetBytes(payload, "input.0.type").String(); got != "message" {
			t.Fatalf("input[0].type = %q, want message; payload=%s", got, payload)
		}
		if got := gjson.GetBytes(payload, "input.0.content.0.type").String(); got != "input_text" {
			t.Fatalf("input[0].content[0].type = %q, want input_text; payload=%s", got, payload)
		}
		if got := gjson.GetBytes(payload, "tools.0.name").String(); got != "ping" {
			t.Fatalf("tools[0].name = %q, want ping; payload=%s", got, payload)
		}
		if gjson.GetBytes(payload, "tools.0.function").Exists() {
			t.Fatalf("tools[0].function should be flattened: %s", payload)
		}
		if got := gjson.GetBytes(payload, "tools.0.strict").Bool(); !got {
			t.Fatalf("tools[0].strict = %v, want true", got)
		}
		if got := gjson.GetBytes(payload, "tools.1.type").String(); got != "function" {
			t.Fatalf("tools[1].type = %q, want function", got)
		}
		if got := gjson.GetBytes(payload, "tools.1.name").String(); got != "tool_search" {
			t.Fatalf("tools[1].name = %q, want tool_search", got)
		}
		if gjson.GetBytes(payload, "tools.1.execution").Exists() {
			t.Fatalf("tools[1].execution should be removed: %s", payload)
		}
		if got := gjson.GetBytes(payload, "tool_choice.name").String(); got != "ping" {
			t.Fatalf("tool_choice.name = %q, want ping", got)
		}
		if gjson.GetBytes(payload, "tool_choice.function").Exists() {
			t.Fatalf("tool_choice.function should be flattened: %s", payload)
		}
	case <-context.Background().Done():
		t.Fatal("unexpected context cancellation")
	}

	if stream == nil {
		t.Fatal("ExecuteStream() returned nil stream")
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		if strings.Contains(string(chunk.Payload), "response.completed") {
			return
		}
	}
	t.Fatal("stream completed without response.completed payload")
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

func TestCodexWebsocketExecuteNonStreamDoesNotDuplicateMessageOutput(t *testing.T) {
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
			[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello world"}]}}`),
			[]byte(`{"type":"response.done","response":{"id":"resp-msg","created_at":1700000000,"model":"gpt-5.4","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":13,"total_tokens":20}}}`),
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
			"input":"hello"
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

	if got := len(gjson.GetBytes(resp.Payload, "output").Array()); got != 1 {
		t.Fatalf("output len = %d, want 1; payload=%s", got, resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.content.0.text").String(); got != "hello world" {
		t.Fatalf("output[0].content[0].text = %q, want %q; payload=%s", got, "hello world", resp.Payload)
	}
}
