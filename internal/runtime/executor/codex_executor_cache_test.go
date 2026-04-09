package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorCacheHelper_OpenAIChatCompletions_StablePromptCacheKeyFromAPIKey(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Set("apiKey", "test-api-key")

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	executor := &CodexExecutor{}
	rawJSON := []byte(`{"model":"gpt-5.3-codex","stream":true}`)
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.3-codex",
		Payload: []byte(`{"model":"gpt-5.3-codex"}`),
	}
	url := "https://example.com/responses"

	httpReq, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai"), url, req, rawJSON)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}

	body, errRead := io.ReadAll(httpReq.Body)
	if errRead != nil {
		t.Fatalf("read request body: %v", errRead)
	}

	expectedKey := uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:codex:prompt-cache:test-api-key")).String()
	gotKey := gjson.GetBytes(body, "prompt_cache_key").String()
	if gotKey != expectedKey {
		t.Fatalf("prompt_cache_key = %q, want %q", gotKey, expectedKey)
	}
	if gotConversation := httpReq.Header.Get("Conversation_id"); gotConversation != expectedKey {
		t.Fatalf("Conversation_id = %q, want %q", gotConversation, expectedKey)
	}
	if gotSession := httpReq.Header.Get("Session_id"); gotSession != expectedKey {
		t.Fatalf("Session_id = %q, want %q", gotSession, expectedKey)
	}

	httpReq2, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai"), url, req, rawJSON)
	if err != nil {
		t.Fatalf("cacheHelper error (second call): %v", err)
	}
	body2, errRead2 := io.ReadAll(httpReq2.Body)
	if errRead2 != nil {
		t.Fatalf("read request body (second call): %v", errRead2)
	}
	gotKey2 := gjson.GetBytes(body2, "prompt_cache_key").String()
	if gotKey2 != expectedKey {
		t.Fatalf("prompt_cache_key (second call) = %q, want %q", gotKey2, expectedKey)
	}
}

func TestCodexExecutorPreservesPreviousResponseIDForNewAPIWebsocketDownstream(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request.Header.Set("X-NewAPI-Downstream-Transport", "websocket")

	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	executor := &CodexExecutor{}
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"websockets": "true"},
	}
	rawJSON := []byte(`{"model":"gpt-5.4","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-1","output":"ok"}]}`)
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: rawJSON,
	}
	url := "https://example.com/responses"

	httpReq, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai-response"), url, req, rawJSON)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}

	body, errRead := io.ReadAll(httpReq.Body)
	if errRead != nil {
		t.Fatalf("read request body: %v", errRead)
	}

	body = stripCodexUnsupportedResponseFields(body, shouldPreserveCodexPreviousResponseID(ctx, auth))
	if got := gjson.GetBytes(body, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %q, want %q", got, "resp-1")
	}
}

func TestShouldPreserveCodexPreviousResponseID_TrueForNewAPIWebsocketHeader(t *testing.T) {
	ctx := contextWithGinHeaders(map[string]string{
		"X-NewAPI-Downstream-Transport": "websocket",
	})
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"websockets": "true"},
	}

	if !shouldPreserveCodexPreviousResponseID(ctx, auth) {
		t.Fatal("expected websocket downstream header to preserve previous_response_id")
	}
}

func TestShouldPreserveCodexPreviousResponseID_FalseForNewAPISSEHeader(t *testing.T) {
	ctx := contextWithGinHeaders(map[string]string{
		"X-NewAPI-Downstream-Transport": "sse",
	})
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"websockets": "true"},
	}

	if shouldPreserveCodexPreviousResponseID(ctx, auth) {
		t.Fatal("expected sse downstream header to delete previous_response_id")
	}
}

func TestShouldPreserveCodexPreviousResponseID_FalseWhenAuthDisablesWebsockets(t *testing.T) {
	ctx := contextWithGinHeaders(map[string]string{
		"X-NewAPI-Downstream-Transport": "websocket",
	})
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"websockets": "false"},
	}

	if !shouldPreserveCodexPreviousResponseID(ctx, auth) {
		t.Fatal("expected websocket bridge header to preserve previous_response_id even when selected auth uses HTTP upstream")
	}
}

func TestShouldPreserveCodexPreviousResponseID_TrueForCodexOAuthWithoutExplicitWebsocketFlag(t *testing.T) {
	ctx := contextWithGinHeaders(map[string]string{
		"X-NewAPI-Downstream-Transport": "websocket",
	})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email": "user@example.com",
		},
	}

	if !shouldPreserveCodexPreviousResponseID(ctx, auth) {
		t.Fatal("expected codex oauth auth to preserve previous_response_id for websocket bridge")
	}
}

func TestStripCodexUnsupportedResponseFields_PreservesPreviousResponseIDWhenRequested(t *testing.T) {
	body := []byte(`{"previous_response_id":"resp-1","prompt_cache_retention":"retained","safety_identifier":"safe","input":[]}`)

	got := stripCodexUnsupportedResponseFields(body, true)

	if prev := gjson.GetBytes(got, "previous_response_id").String(); prev != "resp-1" {
		t.Fatalf("previous_response_id = %q, want %q", prev, "resp-1")
	}
	if gjson.GetBytes(got, "prompt_cache_retention").Exists() {
		t.Fatalf("prompt_cache_retention should be removed: %s", got)
	}
	if gjson.GetBytes(got, "safety_identifier").Exists() {
		t.Fatalf("safety_identifier should be removed: %s", got)
	}
}

func TestStripCodexUnsupportedResponseFields_RemovesPreviousResponseIDWhenNotPreserved(t *testing.T) {
	body := []byte(`{"previous_response_id":"resp-1","prompt_cache_retention":"retained","safety_identifier":"safe","input":[]}`)

	got := stripCodexUnsupportedResponseFields(body, false)

	if gjson.GetBytes(got, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id should be removed: %s", got)
	}
	if gjson.GetBytes(got, "prompt_cache_retention").Exists() {
		t.Fatalf("prompt_cache_retention should be removed: %s", got)
	}
	if gjson.GetBytes(got, "safety_identifier").Exists() {
		t.Fatalf("safety_identifier should be removed: %s", got)
	}
}

func TestCodexExecutorExecuteNonStreamUsesAccumulatedOutputTextWhenCompletedOutputEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := io.WriteString(w,
			"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_123\",\"created_at\":1700000000,\"model\":\"gpt-5.4\"}}\n\n"+
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello world\"}\n\n"+
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_123\",\"created_at\":1700000000,\"model\":\"gpt-5.4\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":7,\"output_tokens\":13,\"total_tokens\":20}}}\n\n")
		if err != nil {
			t.Fatalf("WriteString() error = %v", err)
		}
	}))
	defer server.Close()

	executor := &CodexExecutor{}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	}

	resp, err := executor.Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "hello world" {
		t.Fatalf("message.content = %q, want hello world; payload=%s", got, strings.TrimSpace(string(resp.Payload)))
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.finish_reason").String(); got != "stop" {
		t.Fatalf("finish_reason = %q, want stop; payload=%s", got, strings.TrimSpace(string(resp.Payload)))
	}
}

func TestCodexExecutorExecuteNonStreamSynthesizesFunctionCallOutputWhenCompletedOutputEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := io.WriteString(w,
			"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"ping\"}}\n\n"+
				"data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"item_id\":\"fc_1\",\"delta\":\"{\\\"city\\\":\"}\n\n"+
				"data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"item_id\":\"fc_1\",\"delta\":\"\\\"Tokyo\\\"}\"}\n\n"+
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_123\",\"created_at\":1700000000,\"model\":\"gpt-5.4\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":7,\"output_tokens\":13,\"total_tokens\":20}}}\n\n")
		if err != nil {
			t.Fatalf("WriteString() error = %v", err)
		}
	}))
	defer server.Close()

	executor := &CodexExecutor{}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
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
		SourceFormat: sdktranslator.FromString("openai-response"),
	}

	resp, err := executor.Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if got := gjson.GetBytes(resp.Payload, "output.0.type").String(); got != "function_call" {
		t.Fatalf("output[0].type = %q, want function_call; payload=%s", got, strings.TrimSpace(string(resp.Payload)))
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.call_id").String(); got != "call_1" {
		t.Fatalf("output[0].call_id = %q, want call_1; payload=%s", got, strings.TrimSpace(string(resp.Payload)))
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.arguments").String(); got != `{"city":"Tokyo"}` {
		t.Fatalf("output[0].arguments = %q, want %q; payload=%s", got, `{"city":"Tokyo"}`, strings.TrimSpace(string(resp.Payload)))
	}
}
