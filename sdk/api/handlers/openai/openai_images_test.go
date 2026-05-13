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
	payload         []byte
	model           string
	sourceFormat    string
	metadata        map[string]any
	deadline        time.Time
	hasDeadline     bool
	payloads        [][]byte
	models          []string
	metadatas       []map[string]any
	results         []imageExecutorResult
	streamPayloads  [][]byte
	streamModels    []string
	streamMetadatas []map[string]any
	streamResults   []imageStreamExecutorResult
}

func (e *imageCaptureExecutor) Identifier() string { return "codex" }

func (e *imageCaptureExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.payload = append(e.payload[:0], req.Payload...)
	e.model = req.Model
	e.sourceFormat = opts.SourceFormat.String()
	e.metadata = cloneImageMetadata(opts.Metadata)
	e.deadline, e.hasDeadline = ctx.Deadline()
	e.payloads = append(e.payloads, append([]byte(nil), req.Payload...))
	e.models = append(e.models, req.Model)
	e.metadatas = append(e.metadatas, cloneImageMetadata(opts.Metadata))

	index := len(e.models) - 1
	if index < len(e.results) {
		result := e.results[index]
		if result.err != nil {
			return coreexecutor.Response{}, result.err
		}
		if len(result.payload) > 0 {
			return coreexecutor.Response{Payload: append([]byte(nil), result.payload...)}, nil
		}
	}
	return coreexecutor.Response{Payload: []byte(`{"id":"resp_1","object":"response","created_at":1775555723,"model":"gpt-5.5","output":[{"type":"image_generation_call","result":"ZmFrZS1pbWFnZQ==","output_format":"png"}],"usage":{"input_tokens":11,"output_tokens":22,"total_tokens":33}}`)}, nil
}

func (e *imageCaptureExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	_ = ctx
	_ = auth
	e.streamPayloads = append(e.streamPayloads, append([]byte(nil), req.Payload...))
	e.streamModels = append(e.streamModels, req.Model)
	e.streamMetadatas = append(e.streamMetadatas, cloneImageMetadata(opts.Metadata))

	index := len(e.streamModels) - 1
	if index < len(e.streamResults) {
		result := e.streamResults[index]
		if result.err != nil {
			return nil, result.err
		}
		ch := make(chan coreexecutor.StreamChunk, len(result.chunks))
		for _, chunk := range result.chunks {
			ch <- cloneImageStreamChunk(chunk)
		}
		close(ch)
		var headers http.Header
		if result.headers != nil {
			headers = result.headers.Clone()
		}
		return &coreexecutor.StreamResult{
			Headers: headers,
			Chunks:  ch,
		}, nil
	}

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

type imageExecutorResult struct {
	payload []byte
	err     error
}

type imageStreamExecutorResult struct {
	headers http.Header
	chunks  []coreexecutor.StreamChunk
	err     error
}

func cloneImageMetadata(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func cloneImageStreamChunk(src coreexecutor.StreamChunk) coreexecutor.StreamChunk {
	dst := src
	if len(src.Payload) > 0 {
		dst.Payload = append([]byte(nil), src.Payload...)
	}
	return dst
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

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1","prompt":"draw a cat","size":"1024x1536","quality":"high","response_format":"b64_json","n":1}`))
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
	if got := gjson.GetBytes(executor.payload, "tools.0.n").Exists(); got {
		t.Fatalf("tools.0.n unexpectedly present; payload=%s", string(executor.payload))
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

func TestImagesGenerationsRejectsMultipleOutputCount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &imageCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "image-count-auth", Provider: executor.Identifier(), Status: coreauth.StatusActive}
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

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"draw a cat","n":2}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if len(executor.payloads) != 0 {
		t.Fatalf("unexpected upstream execution for unsupported n; payloads=%d", len(executor.payloads))
	}
	if got := gjson.Get(resp.Body.String(), "error.message").String(); !strings.Contains(got, "n greater than 1") {
		t.Fatalf("error.message = %q, want unsupported n hint", got)
	}
}

func TestImagesGenerationsFallsBackToGPT54WhenGPT55Unavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &imageCaptureExecutor{
		results: []imageExecutorResult{
			{
				err: &coreauth.Error{
					Code:       "request_scoped_auth_unavailable",
					Message:    "The requested model is currently unavailable. Please switch model.",
					Retryable:  true,
					HTTPStatus: http.StatusServiceUnavailable,
				},
			},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "image-fallback-auth", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: defaultImagesMainModel}, {ID: fallbackImagesMainModel}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/generations", h.ImagesGenerations)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"draw a cat"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if got := len(executor.models); got != 2 {
		t.Fatalf("attempt count = %d, want 2", got)
	}
	if executor.models[0] != defaultImagesMainModel || executor.models[1] != fallbackImagesMainModel {
		t.Fatalf("models = %v, want [%s %s]", executor.models, defaultImagesMainModel, fallbackImagesMainModel)
	}
	if got := gjson.GetBytes(executor.payloads[0], "model").String(); got != defaultImagesMainModel {
		t.Fatalf("first payload model = %q, want %s; payload=%s", got, defaultImagesMainModel, string(executor.payloads[0]))
	}
	if got := gjson.GetBytes(executor.payloads[1], "model").String(); got != fallbackImagesMainModel {
		t.Fatalf("second payload model = %q, want %s; payload=%s", got, fallbackImagesMainModel, string(executor.payloads[1]))
	}
	if got := executor.metadatas[0][coreexecutor.RequestedModelMetadataKey]; got != defaultImagesMainModel {
		t.Fatalf("first requested model metadata = %#v, want %s", got, defaultImagesMainModel)
	}
	if got := executor.metadatas[1][coreexecutor.RequestedModelMetadataKey]; got != fallbackImagesMainModel {
		t.Fatalf("second requested model metadata = %#v, want %s", got, fallbackImagesMainModel)
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
	maskPart, err := writer.CreateFormFile("mask", "mask.png")
	if err != nil {
		t.Fatalf("CreateFormFile mask: %v", err)
	}
	if _, err := maskPart.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}); err != nil {
		t.Fatalf("write mask: %v", err)
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
	maskURL := gjson.GetBytes(executor.payload, "tools.0.input_image_mask.image_url").String()
	if !strings.HasPrefix(maskURL, "data:image/png;base64,") {
		t.Fatalf("mask image URL = %q, want png data URL; payload=%s", maskURL, string(executor.payload))
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

func TestConvertResponsesImageToOpenAIImageIncludesMetadataAndRevisedPrompt(t *testing.T) {
	raw := []byte(`{"created_at":1775555723,"tools":[{"type":"image_generation","background":"transparent","output_format":"png","quality":"high","size":"1536x1024"}],"output":[{"type":"image_generation_call","result":"ZmFrZS1pbWFnZQ==","revised_prompt":"refined prompt"}],"usage":{"input_tokens":11,"output_tokens":22,"total_tokens":33}}`)

	out, err := convertResponsesImageToOpenAIImage(raw)
	if err != nil {
		t.Fatalf("convertResponsesImageToOpenAIImage returned error: %v", err)
	}
	if got := gjson.GetBytes(out, "background").String(); got != "transparent" {
		t.Fatalf("background = %q, want transparent; body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "output_format").String(); got != "png" {
		t.Fatalf("output_format = %q, want png; body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "quality").String(); got != "high" {
		t.Fatalf("quality = %q, want high; body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "size").String(); got != "1536x1024" {
		t.Fatalf("size = %q, want 1536x1024; body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "data.0.revised_prompt").String(); got != "refined prompt" {
		t.Fatalf("revised_prompt = %q, want refined prompt; body=%s", got, string(out))
	}
}

func TestImagesMainModelUnavailableMessageTreatsServerOverloadedAsFallback(t *testing.T) {
	message := `{"error":{"message":"Our servers are currently overloaded. Please try again later.","type":"service_unavailable_error","code":"server_is_overloaded"}}`
	if !isImagesMainModelUnavailableMessage(message) {
		t.Fatal("expected server_is_overloaded to trigger image main model fallback")
	}
}
