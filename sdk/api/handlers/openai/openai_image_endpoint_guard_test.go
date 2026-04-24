package openai

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/tidwall/gjson"
)

func TestResponsesRejectsImageGenerationModelWithEndpointHint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses", h.Responses)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-image-2","input":"draw a cat"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if got := gjson.Get(resp.Body.String(), "error.code").String(); got != "endpoint_not_supported" {
		t.Fatalf("error.code = %q, want endpoint_not_supported; body=%s", got, resp.Body.String())
	}
	if got := gjson.Get(resp.Body.String(), "error.message").String(); !strings.Contains(got, "/v1/images/generations") {
		t.Fatalf("error.message = %q, want image endpoint hint", got)
	}
}

func TestChatCompletionsRejectsImageGenerationModelWithEndpointHint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	h := NewOpenAIAPIHandler(base)
	router := gin.New()
	router.POST("/v1/chat/completions", h.ChatCompletions)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-image-2","messages":[{"role":"user","content":"draw a cat"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if got := gjson.Get(resp.Body.String(), "error.code").String(); got != "endpoint_not_supported" {
		t.Fatalf("error.code = %q, want endpoint_not_supported; body=%s", got, resp.Body.String())
	}
	if got := gjson.Get(resp.Body.String(), "error.message").String(); !strings.Contains(got, "/v1/images/generations") {
		t.Fatalf("error.message = %q, want image endpoint hint", got)
	}
}
