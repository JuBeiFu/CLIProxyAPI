package openai

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/tidwall/gjson"
)

func newOpenAIImageTestRouter(t *testing.T, executor *imageCaptureExecutor) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: t.Name() + "-auth", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{
		{ID: defaultImagesMainModel},
		{ID: fallbackImagesMainModel},
	})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/generations", h.ImagesGenerations)
	router.POST("/v1/images/edits", h.ImagesEdits)
	router.POST("/v1/images/variations", h.ImagesVariations)
	return router
}

func TestImagesGenerationsStreamTranslatesResponsesEvents(t *testing.T) {
	executor := &imageCaptureExecutor{
		streamResults: []imageStreamExecutorResult{{
			chunks: []coreexecutor.StreamChunk{
				{Payload: []byte("event: response.created")},
				{Payload: []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"created_at\":1775555723}}\n\n")},
				{Payload: []byte("event: response.image_generation_call.partial_image\ndata: {\"type\":\"response.image_generation_call.partial_image\",\"partial_image_index\":0,\"partial_image_b64\":\"cGFydGlhbA==\"}\n\n")},
				{Payload: []byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"image_generation_call\",\"result\":\"ZmluYWw=\",\"output_format\":\"png\",\"revised_prompt\":\"refined prompt\"}}\n\n")},
				{Payload: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"created_at\":1775555723,\"tools\":[{\"type\":\"image_generation\",\"background\":\"transparent\",\"output_format\":\"png\",\"quality\":\"high\",\"size\":\"1536x1024\"}],\"usage\":{\"input_tokens\":12,\"output_tokens\":34,\"total_tokens\":46}}}\n\n")},
				{Payload: []byte("data: [DONE]\n\n")},
			},
		}},
	}
	router := newOpenAIImageTestRouter(t, executor)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"draw a poster","size":"1536x1024","quality":"high","background":"transparent","partial_images":1,"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if got := resp.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := gjson.GetBytes(executor.streamPayloads[0], "stream").Bool(); !got {
		t.Fatalf("payload stream = %v, want true; payload=%s", got, string(executor.streamPayloads[0]))
	}
	if got := gjson.GetBytes(executor.streamPayloads[0], "tools.0.partial_images").Int(); got != 1 {
		t.Fatalf("tools.0.partial_images = %d, want 1; payload=%s", got, string(executor.streamPayloads[0]))
	}
	body := resp.Body.String()
	if !strings.Contains(body, "event: image_generation.partial_image") {
		t.Fatalf("stream body missing partial image event: %s", body)
	}
	if !strings.Contains(body, "\"type\":\"image_generation.completed\"") {
		t.Fatalf("stream body missing completed event: %s", body)
	}
	if !strings.Contains(body, "\"b64_json\":\"ZmluYWw=\"") {
		t.Fatalf("stream body missing final image payload: %s", body)
	}
	if !strings.Contains(body, "\"total_tokens\":46") {
		t.Fatalf("stream body missing usage payload: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream body missing done marker: %s", body)
	}
}

func TestImagesEditsStreamTranslatesResponsesEvents(t *testing.T) {
	executor := &imageCaptureExecutor{
		streamResults: []imageStreamExecutorResult{{
			chunks: []coreexecutor.StreamChunk{
				{Payload: []byte("event: response.image_generation_call.partial_image\ndata: {\"type\":\"response.image_generation_call.partial_image\",\"partial_image_index\":0,\"partial_image_b64\":\"cGFydGlhbA==\"}\n\n")},
				{Payload: []byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"image_generation_call\",\"result\":\"ZWRpdGVk\",\"output_format\":\"png\"}}\n\n")},
				{Payload: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"created_at\":1775555723,\"tools\":[{\"type\":\"image_generation\",\"background\":\"opaque\",\"output_format\":\"png\",\"quality\":\"medium\",\"size\":\"1024x1024\"}],\"usage\":{\"input_tokens\":15,\"output_tokens\":25,\"total_tokens\":40}}}\n\n")},
				{Payload: []byte("data: [DONE]\n\n")},
			},
		}},
	}
	router := newOpenAIImageTestRouter(t, executor)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(`{"model":"gpt-image-2","prompt":"add a red hat","image":["data:image/png;base64,Zm9v"],"stream":true,"partial_images":1}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if got := gjson.GetBytes(executor.streamPayloads[0], "tools.0.action").String(); got != "edit" {
		t.Fatalf("tools.0.action = %q, want edit; payload=%s", got, string(executor.streamPayloads[0]))
	}
	body := resp.Body.String()
	if !strings.Contains(body, "event: image_edit.partial_image") {
		t.Fatalf("stream body missing image edit partial event: %s", body)
	}
	if !strings.Contains(body, "\"type\":\"image_edit.completed\"") {
		t.Fatalf("stream body missing image edit completed event: %s", body)
	}
	if !strings.Contains(body, "\"b64_json\":\"ZWRpdGVk\"") {
		t.Fatalf("stream body missing final edited image payload: %s", body)
	}
}

func TestImagesGenerationsStreamFallsBackToGPT54WhenGPT55Unavailable(t *testing.T) {
	executor := &imageCaptureExecutor{
		streamResults: []imageStreamExecutorResult{
			{
				chunks: []coreexecutor.StreamChunk{{
					Err: &coreauth.Error{
						Code:       "request_scoped_auth_unavailable",
						Message:    "The requested model is currently unavailable. Please switch model.",
						Retryable:  true,
						HTTPStatus: http.StatusServiceUnavailable,
					},
				}},
			},
			{
				chunks: []coreexecutor.StreamChunk{
					{Payload: []byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"image_generation_call\",\"result\":\"ZmFsbGJhY2s=\",\"output_format\":\"png\"}}\n\n")},
					{Payload: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"created_at\":1775555723,\"tools\":[{\"type\":\"image_generation\",\"output_format\":\"png\",\"size\":\"1024x1024\"}],\"usage\":{\"input_tokens\":10,\"output_tokens\":20,\"total_tokens\":30}}}\n\n")},
					{Payload: []byte("data: [DONE]\n\n")},
				},
			},
		},
	}
	router := newOpenAIImageTestRouter(t, executor)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"draw a cat","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if got := len(executor.streamModels); got != 2 {
		t.Fatalf("stream attempt count = %d, want 2", got)
	}
	if executor.streamModels[0] != defaultImagesMainModel || executor.streamModels[1] != fallbackImagesMainModel {
		t.Fatalf("stream models = %v, want [%s %s]", executor.streamModels, defaultImagesMainModel, fallbackImagesMainModel)
	}
	if got := gjson.GetBytes(executor.streamPayloads[1], "model").String(); got != fallbackImagesMainModel {
		t.Fatalf("fallback payload model = %q, want %s; payload=%s", got, fallbackImagesMainModel, string(executor.streamPayloads[1]))
	}
	if body := resp.Body.String(); !strings.Contains(body, "\"b64_json\":\"ZmFsbGJhY2s=\"") {
		t.Fatalf("stream body missing fallback final image: %s", body)
	}
}

func TestImagesVariationsAcceptsMultipartCompatibilityRequest(t *testing.T) {
	executor := &imageCaptureExecutor{}
	router := newOpenAIImageTestRouter(t, executor)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "dall-e-2"); err != nil {
		t.Fatalf("WriteField model: %v", err)
	}
	part, err := writer.CreateFormFile("image", "seed.png")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}); err != nil {
		t.Fatalf("write image: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/images/variations", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if got := gjson.GetBytes(executor.payload, "tools.0.action").String(); got != "edit" {
		t.Fatalf("tools.0.action = %q, want edit; payload=%s", got, string(executor.payload))
	}
	if got := gjson.GetBytes(executor.payload, "tools.0.model").String(); got != defaultImagesToolModel {
		t.Fatalf("tools.0.model = %q, want %s; payload=%s", got, defaultImagesToolModel, string(executor.payload))
	}
	if got := gjson.GetBytes(executor.payload, "tools.0.size").String(); got != defaultVariationSize {
		t.Fatalf("tools.0.size = %q, want %s; payload=%s", got, defaultVariationSize, string(executor.payload))
	}
	if got := gjson.GetBytes(executor.payload, "input.0.content.0.text").String(); !strings.Contains(strings.ToLower(got), "variation") {
		t.Fatalf("variation prompt = %q, want synthesized variation prompt; payload=%s", got, string(executor.payload))
	}
	if got := gjson.GetBytes(executor.payload, "input.0.content.1.image_url").String(); !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("variation image URL = %q, want png data URL; payload=%s", got, string(executor.payload))
	}
}

func TestImagesVariationsRejectsStreamRequests(t *testing.T) {
	executor := &imageCaptureExecutor{}
	router := newOpenAIImageTestRouter(t, executor)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/variations", strings.NewReader(`{"image":["data:image/png;base64,Zm9v"],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if got := gjson.Get(resp.Body.String(), "error.message").String(); !strings.Contains(got, openAIImageVarEndpoint) {
		t.Fatalf("error.message = %q, want variations endpoint hint", got)
	}
}
