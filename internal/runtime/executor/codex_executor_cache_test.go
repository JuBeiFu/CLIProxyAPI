package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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
	if gotConversation := httpReq.Header.Get("Conversation_id"); gotConversation != "" {
		t.Fatalf("Conversation_id = %q, want empty", gotConversation)
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

func TestCodexExecutorCacheHelper_LargePayloadPromptCacheKeyAllocationsStayBounded(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Set("apiKey", "test-api-key")

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	executor := &CodexExecutor{}
	rawJSON := buildLargeCacheHelperCodexBodyWithTools(64, 16*1024)
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: rawJSON,
	}
	url := "https://example.com/responses"
	_, _ = executor.cacheHelper(ctx, sdktranslator.FromString("openai"), url, req, rawJSON)

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	httpReq, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai"), url, req, rawJSON)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}
	body, errRead := io.ReadAll(httpReq.Body)
	if errRead != nil {
		t.Fatalf("read request body: %v", errRead)
	}
	runtime.KeepAlive(body)

	if !json.Valid(body) {
		t.Fatalf("request body JSON is invalid: %s", body)
	}
	if gotKey := gjson.GetBytes(body, "prompt_cache_key").String(); gotKey == "" {
		t.Fatalf("prompt_cache_key is empty: %s", body)
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	limit := uint64(len(rawJSON) + 8192)
	if allocated > limit {
		t.Fatalf("allocated %d bytes for %d-byte body, want <= %d", allocated, len(rawJSON), limit)
	}
}

func buildLargeCacheHelperCodexBodyWithTools(messageCount, messageSize int) []byte {
	text := strings.Repeat("x", messageSize)
	var b strings.Builder
	b.Grow(messageCount*messageSize + 256)
	b.WriteString(`{"model":"gpt-5.4","stream":true,"input":[`)
	for i := 0; i < messageCount; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"type":"message","role":"user","content":[{"type":"input_text","text":`)
		b.WriteString(strconv.Quote(text))
		b.WriteString(`}]}`)
	}
	b.WriteString(`],"tools":[{"type":"function","name":"noop","parameters":{"type":"object","properties":{}}}]}`)
	return []byte(b.String())
}

func TestShouldPreserveCodexPreviousResponseID_FalseForOpenAIResponsesFollowupOverHTTP(t *testing.T) {
	ctx := contextWithGinHeaders(map[string]string{
		"X-NewAPI-Downstream-Transport": "sse",
	})
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"websockets": "true"},
	}
	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-1","output":"ok"}]}`)

	if shouldPreserveCodexPreviousResponseID(ctx, auth, sdktranslator.FromString("openai-response"), body) {
		t.Fatal("expected HTTP/SSE upstream to strip previous_response_id after manager transcript expansion")
	}
}

func TestShouldPreserveCodexPreviousResponseID_FalseForNonResponsesSSEHeader(t *testing.T) {
	ctx := contextWithGinHeaders(map[string]string{
		"X-NewAPI-Downstream-Transport": "sse",
	})
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"websockets": "true"},
	}

	if shouldPreserveCodexPreviousResponseID(ctx, auth, sdktranslator.FromString("openai"), nil) {
		t.Fatal("expected sse downstream header to delete previous_response_id")
	}
}

func TestShouldPreserveCodexPreviousResponseID_FalseForNewAPIWebsocketHeader(t *testing.T) {
	ctx := contextWithGinHeaders(map[string]string{
		"X-NewAPI-Downstream-Transport": "websocket",
	})
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"websockets": "false"},
	}

	if shouldPreserveCodexPreviousResponseID(ctx, auth, sdktranslator.FromString("openai"), nil) {
		t.Fatal("expected new-api websocket header to still use SSE-safe transcript expansion")
	}
}

func TestShouldPreserveCodexPreviousResponseID_TrueForExplicitDownstreamWebsocketContext(t *testing.T) {
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email": "user@example.com",
		},
	}

	if !shouldPreserveCodexPreviousResponseID(ctx, auth, sdktranslator.FromString("openai"), nil) {
		t.Fatal("expected explicit downstream websocket context to preserve previous_response_id")
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
