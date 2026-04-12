package executor

import (
	"context"
	"io"
	"net/http/httptest"
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

func TestShouldPreserveCodexPreviousResponseID_TrueForOpenAIResponsesFollowupOverHTTP(t *testing.T) {
	ctx := contextWithGinHeaders(map[string]string{
		"X-NewAPI-Downstream-Transport": "sse",
	})
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"websockets": "true"},
	}
	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-1","output":"ok"}]}`)

	if !shouldPreserveCodexPreviousResponseID(ctx, auth, sdktranslator.FromString("openai-response"), body) {
		t.Fatal("expected openai responses follow-up to preserve previous_response_id over HTTP")
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

func TestShouldPreserveCodexPreviousResponseID_FalseWhenAuthDisablesWebsockets(t *testing.T) {
	ctx := contextWithGinHeaders(map[string]string{
		"X-NewAPI-Downstream-Transport": "websocket",
	})
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"websockets": "false"},
	}

	if !shouldPreserveCodexPreviousResponseID(ctx, auth, sdktranslator.FromString("openai"), nil) {
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

	if !shouldPreserveCodexPreviousResponseID(ctx, auth, sdktranslator.FromString("openai"), nil) {
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
