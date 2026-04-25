package openai

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/tidwall/gjson"
)

type imageCaptureExecutor struct {
	payload      []byte
	model        string
	sourceFormat string
	metadata     map[string]any
	deadline     time.Time
	hasDeadline  bool
}

func (e *imageCaptureExecutor) Identifier() string { return "codex" }

func (e *imageCaptureExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.payload = append(e.payload[:0], req.Payload...)
	e.model = req.Model
	e.sourceFormat = opts.SourceFormat.String()
	e.metadata = opts.Metadata
	e.deadline, e.hasDeadline = ctx.Deadline()
	return coreexecutor.Response{Payload: []byte(`{"id":"resp_1","object":"response","created_at":1775555723,"model":"gpt-5.4-mini","output":[{"type":"image_generation_call","result":"ZmFrZS1pbWFnZQ==","output_format":"png"}],"usage":{"input_tokens":11,"output_tokens":22,"total_tokens":33}}`)}, nil
}

func (e *imageCaptureExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e *imageCaptureExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *imageCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *imageCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func TestImagesGenerationsConvertsOpenAIRequestToResponsesAndReturnsImage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &imageCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "image-auth", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: defaultImagesMainModel}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/generations", h.ImagesGenerations)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1","prompt":"draw a cat","size":"1024x1536","quality":"high","response_format":"b64_json"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if executor.model != defaultImagesMainModel {
		t.Fatalf("executor model = %q, want %s", executor.model, defaultImagesMainModel)
	}
	if got := executor.metadata[coreexecutor.RequestedModelMetadataKey]; got != defaultImagesMainModel {
		t.Fatalf("requested model metadata = %#v, want %s", got, defaultImagesMainModel)
	}
	if got := executor.metadata[coreexecutor.ImageGenerationRequestMetadataKey]; got != true {
		t.Fatalf("image generation metadata = %#v, want true", got)
	}
	if !executor.hasDeadline {
		t.Fatal("expected image generation context to have a deadline")
	}
	remaining := time.Until(executor.deadline)
	if remaining < 9*time.Minute || remaining > 10*time.Minute {
		t.Fatalf("image generation timeout remaining = %v, want about 10m", remaining)
	}
	if executor.sourceFormat != "openai-response" {
		t.Fatalf("source format = %q, want openai-response", executor.sourceFormat)
	}
	if got := gjson.GetBytes(executor.payload, "input.0.content.0.text").String(); got != "draw a cat" {
		t.Fatalf("payload input text = %q, want prompt; payload=%s", got, string(executor.payload))
	}
	if got := gjson.GetBytes(executor.payload, "model").String(); got != defaultImagesMainModel {
		t.Fatalf("payload model = %q, want %s; payload=%s", got, defaultImagesMainModel, string(executor.payload))
	}
	if got := gjson.GetBytes(executor.payload, "tools.0.model").String(); got != "gpt-image-1" {
		t.Fatalf("tools.0.model = %q, want gpt-image-1; payload=%s", got, string(executor.payload))
	}
	if got := gjson.GetBytes(executor.payload, "tools.0.action").String(); got != "generate" {
		t.Fatalf("tools.0.action = %q, want generate; payload=%s", got, string(executor.payload))
	}
	if got := gjson.GetBytes(executor.payload, "tools.0.size").String(); got != "1024x1536" {
		t.Fatalf("tools.0.size = %q, want 1024x1536; payload=%s", got, string(executor.payload))
	}
	if got := gjson.GetBytes(executor.payload, "tools.0.quality").String(); got != "high" {
		t.Fatalf("tools.0.quality = %q, want high; payload=%s", got, string(executor.payload))
	}
	if got := gjson.GetBytes(executor.payload, "tool_choice.type").String(); got != "image_generation" {
		t.Fatalf("tool_choice.type = %q, want image_generation; payload=%s", got, string(executor.payload))
	}
	if got := gjson.Get(resp.Body.String(), "data.0.b64_json").String(); got != "ZmFrZS1pbWFnZQ==" {
		t.Fatalf("b64_json = %q, want generated image; body=%s", got, resp.Body.String())
	}
	if got := gjson.Get(resp.Body.String(), "usage.total_tokens").Int(); got != 33 {
		t.Fatalf("usage.total_tokens = %d, want 33; body=%s", got, resp.Body.String())
	}
}

func TestImagesEditsAcceptsMultipartRequestFromNewAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &imageCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "image-edit-auth", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: defaultImagesMainModel}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/edits", h.ImagesEdits)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "gpt-image-2"); err != nil {
		t.Fatalf("WriteField model: %v", err)
	}
	if err := writer.WriteField("prompt", "add a red hat"); err != nil {
		t.Fatalf("WriteField prompt: %v", err)
	}
	if err := writer.WriteField("size", "2048x1152"); err != nil {
		t.Fatalf("WriteField size: %v", err)
	}
	part, err := writer.CreateFormFile("image[]", "cat.png")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}); err != nil {
		t.Fatalf("write image: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if got := gjson.GetBytes(executor.payload, "model").String(); got != defaultImagesMainModel {
		t.Fatalf("payload model = %q, want %s; payload=%s", got, defaultImagesMainModel, string(executor.payload))
	}
	if got := gjson.GetBytes(executor.payload, "tools.0.model").String(); got != defaultImagesToolModel {
		t.Fatalf("tools.0.model = %q, want %s; payload=%s", got, defaultImagesToolModel, string(executor.payload))
	}
	if got := gjson.GetBytes(executor.payload, "tools.0.action").String(); got != "edit" {
		t.Fatalf("tools.0.action = %q, want edit; payload=%s", got, string(executor.payload))
	}
	if got := gjson.GetBytes(executor.payload, "tools.0.size").String(); got != "2048x1152" {
		t.Fatalf("tools.0.size = %q, want 2048x1152; payload=%s", got, string(executor.payload))
	}
	imageURL := gjson.GetBytes(executor.payload, "input.0.content.1.image_url").String()
	if !strings.HasPrefix(imageURL, "data:image/png;base64,") {
		t.Fatalf("input image URL = %q, want png data URL; payload=%s", imageURL, string(executor.payload))
	}
}

func TestImagesGenerationsAcceptsSquareGptImage2Size(t *testing.T) {
	raw, routeModelName, requestedModelName, err := convertImagesGenerationRequestToResponses([]byte(`{"model":"gpt-image-2","prompt":"draw a poster","size":"2048x2048"}`))
	if err != nil {
		t.Fatalf("convertImagesGenerationRequestToResponses returned error: %v", err)
	}
	if routeModelName != defaultImagesMainModel {
		t.Fatalf("routeModelName = %q, want %s", routeModelName, defaultImagesMainModel)
	}
	if requestedModelName != defaultImagesMainModel {
		t.Fatalf("requestedModelName = %q, want %s", requestedModelName, defaultImagesMainModel)
	}
	if got := gjson.GetBytes(raw, "model").String(); got != defaultImagesMainModel {
		t.Fatalf("payload model = %q, want %s; payload=%s", got, defaultImagesMainModel, string(raw))
	}
	if got := gjson.GetBytes(raw, "tools.0.model").String(); got != defaultImagesToolModel {
		t.Fatalf("tools.0.model = %q, want %s; payload=%s", got, defaultImagesToolModel, string(raw))
	}
	if got := gjson.GetBytes(raw, "tools.0.size").String(); got != "2048x2048" {
		t.Fatalf("tools.0.size = %q, want 2048x2048; payload=%s", got, string(raw))
	}
}

func TestImagesGenerationsDefaultsToAutoSize(t *testing.T) {
	raw, routeModelName, requestedModelName, err := convertImagesGenerationRequestToResponses([]byte(`{"model":"gpt-image-2","prompt":"draw a poster"}`))
	if err != nil {
		t.Fatalf("convertImagesGenerationRequestToResponses returned error: %v", err)
	}
	if routeModelName != defaultImagesMainModel {
		t.Fatalf("routeModelName = %q, want %s", routeModelName, defaultImagesMainModel)
	}
	if requestedModelName != defaultImagesMainModel {
		t.Fatalf("requestedModelName = %q, want %s", requestedModelName, defaultImagesMainModel)
	}
	if got := gjson.GetBytes(raw, "model").String(); got != defaultImagesMainModel {
		t.Fatalf("payload model = %q, want %s; payload=%s", got, defaultImagesMainModel, string(raw))
	}
	if gjson.GetBytes(raw, "tools.0.size").Exists() {
		t.Fatalf("default image_generation tool should omit size; payload=%s", string(raw))
	}
}

func TestImagesGenerationsNormalizesLandscapeGptImage2Size(t *testing.T) {
	raw, routeModelName, requestedModelName, err := convertImagesGenerationRequestToResponses([]byte(`{"model":"gpt-image-2","prompt":"draw a poster","size":"3840x2160"}`))
	if err != nil {
		t.Fatalf("convertImagesGenerationRequestToResponses returned error: %v", err)
	}
	if routeModelName != defaultImagesMainModel {
		t.Fatalf("routeModelName = %q, want %s", routeModelName, defaultImagesMainModel)
	}
	if requestedModelName != defaultImagesMainModel {
		t.Fatalf("requestedModelName = %q, want %s", requestedModelName, defaultImagesMainModel)
	}
	if got := gjson.GetBytes(raw, "model").String(); got != defaultImagesMainModel {
		t.Fatalf("payload model = %q, want %s; payload=%s", got, defaultImagesMainModel, string(raw))
	}
	if got := gjson.GetBytes(raw, "tools.0.size").String(); got != "3840x2160" {
		t.Fatalf("tools.0.size = %q, want 3840x2160; payload=%s", got, string(raw))
	}
}

func TestImagesGenerationsNormalizesPortraitGptImage2Size(t *testing.T) {
	raw, routeModelName, requestedModelName, err := convertImagesGenerationRequestToResponses([]byte(`{"model":"gpt-image-2","prompt":"draw a poster","size":"2160x3840"}`))
	if err != nil {
		t.Fatalf("convertImagesGenerationRequestToResponses returned error: %v", err)
	}
	if routeModelName != defaultImagesMainModel {
		t.Fatalf("routeModelName = %q, want %s", routeModelName, defaultImagesMainModel)
	}
	if requestedModelName != defaultImagesMainModel {
		t.Fatalf("requestedModelName = %q, want %s", requestedModelName, defaultImagesMainModel)
	}
	if got := gjson.GetBytes(raw, "model").String(); got != defaultImagesMainModel {
		t.Fatalf("payload model = %q, want %s; payload=%s", got, defaultImagesMainModel, string(raw))
	}
	if got := gjson.GetBytes(raw, "tools.0.size").String(); got != "2160x3840" {
		t.Fatalf("tools.0.size = %q, want 2160x3840; payload=%s", got, string(raw))
	}
}

func TestImagesEditsConvertsJSONImageReferencesToResponses(t *testing.T) {
	raw, routeModelName, requestedModelName, err := convertImagesEditRequestToResponses([]byte(`{"model":"gpt-image-2","prompt":"add a red hat","size":"2048x1152","image":["data:image/png;base64,Zm9v","https://example.test/cat.png"]}`))
	if err != nil {
		t.Fatalf("convertImagesEditRequestToResponses returned error: %v", err)
	}
	if routeModelName != defaultImagesMainModel {
		t.Fatalf("routeModelName = %q, want %s", routeModelName, defaultImagesMainModel)
	}
	if requestedModelName != defaultImagesMainModel {
		t.Fatalf("requestedModelName = %q, want %s", requestedModelName, defaultImagesMainModel)
	}
	if got := gjson.GetBytes(raw, "tools.0.model").String(); got != defaultImagesToolModel {
		t.Fatalf("tools.0.model = %q, want %s; payload=%s", got, defaultImagesToolModel, string(raw))
	}
	if got := gjson.GetBytes(raw, "tools.0.action").String(); got != "edit" {
		t.Fatalf("tools.0.action = %q, want edit; payload=%s", got, string(raw))
	}
	if got := gjson.GetBytes(raw, "tools.0.size").String(); got != "2048x1152" {
		t.Fatalf("tools.0.size = %q, want 2048x1152; payload=%s", got, string(raw))
	}
	content := gjson.GetBytes(raw, "input.0.content")
	if got := content.Get("0.type").String(); got != "input_text" {
		t.Fatalf("content.0.type = %q, want input_text; payload=%s", got, string(raw))
	}
	if got := content.Get("1.image_url").String(); got != "data:image/png;base64,Zm9v" {
		t.Fatalf("content.1.image_url = %q, want first image; payload=%s", got, string(raw))
	}
	if got := content.Get("2.image_url").String(); got != "https://example.test/cat.png" {
		t.Fatalf("content.2.image_url = %q, want second image; payload=%s", got, string(raw))
	}
}

func TestImagesGenerationsInvalidSizeReturnsError(t *testing.T) {
	_, _, _, err := convertImagesGenerationRequestToResponses([]byte(`{"model":"gpt-image-2","prompt":"draw a poster","size":"4096"}`))
	if err == nil {
		t.Fatal("expected invalid size to return error")
	}
}

func TestImagesGenerationsOversizedSquareReturnsError(t *testing.T) {
	_, _, _, err := convertImagesGenerationRequestToResponses([]byte(`{"model":"gpt-image-2","prompt":"draw a poster","size":"4096x4096"}`))
	if err == nil {
		t.Fatal("expected oversized square to return error")
	}
}
