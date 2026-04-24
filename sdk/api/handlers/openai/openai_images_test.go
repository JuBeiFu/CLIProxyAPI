package openai

import (
	"context"
	"errors"
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

type imageCaptureExecutor struct {
	payload      []byte
	model        string
	sourceFormat string
	metadata     map[string]any
}

func (e *imageCaptureExecutor) Identifier() string { return "codex" }

func (e *imageCaptureExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.payload = append(e.payload[:0], req.Payload...)
	e.model = req.Model
	e.sourceFormat = opts.SourceFormat.String()
	e.metadata = opts.Metadata
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
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gpt-draw-1024x1024"}})
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
	if executor.model != "gpt-draw-1024x1024" {
		t.Fatalf("executor model = %q, want gpt-draw-1024x1024", executor.model)
	}
	if got := executor.metadata[coreexecutor.RequestedModelMetadataKey]; got != "gpt-draw-1024x1536" {
		t.Fatalf("requested model metadata = %#v, want gpt-draw-1024x1536", got)
	}
	if executor.sourceFormat != "openai-response" {
		t.Fatalf("source format = %q, want openai-response", executor.sourceFormat)
	}
	if got := gjson.GetBytes(executor.payload, "input").String(); got != "draw a cat" {
		t.Fatalf("payload input = %q, want prompt; payload=%s", got, string(executor.payload))
	}
	if got := gjson.GetBytes(executor.payload, "model").String(); got != "gpt-draw-1024x1536" {
		t.Fatalf("payload model = %q, want gpt-draw-1024x1536; payload=%s", got, string(executor.payload))
	}
	if got := gjson.Get(resp.Body.String(), "data.0.b64_json").String(); got != "ZmFrZS1pbWFnZQ==" {
		t.Fatalf("b64_json = %q, want generated image; body=%s", got, resp.Body.String())
	}
	if got := gjson.Get(resp.Body.String(), "usage.total_tokens").Int(); got != 33 {
		t.Fatalf("usage.total_tokens = %d, want 33; body=%s", got, resp.Body.String())
	}
}

func TestImagesGenerationsNormalizesSquareGptImage2Size(t *testing.T) {
	raw, routeModelName, requestedModelName, err := convertImagesGenerationRequestToResponses([]byte(`{"model":"gpt-image-2","prompt":"draw a poster","size":"4096x4096"}`))
	if err != nil {
		t.Fatalf("convertImagesGenerationRequestToResponses returned error: %v", err)
	}
	if routeModelName != "gpt-draw-1024x1024" {
		t.Fatalf("routeModelName = %q, want gpt-draw-1024x1024", routeModelName)
	}
	if requestedModelName != "gpt-draw-1024x1024" {
		t.Fatalf("requestedModelName = %q, want gpt-draw-1024x1024", requestedModelName)
	}
	if got := gjson.GetBytes(raw, "model").String(); got != "gpt-draw-1024x1024" {
		t.Fatalf("payload model = %q, want gpt-draw-1024x1024; payload=%s", got, string(raw))
	}
}

func TestImagesGenerationsNormalizesLandscapeGptImage2Size(t *testing.T) {
	raw, routeModelName, requestedModelName, err := convertImagesGenerationRequestToResponses([]byte(`{"model":"gpt-image-2","prompt":"draw a poster","size":"3072x1536"}`))
	if err != nil {
		t.Fatalf("convertImagesGenerationRequestToResponses returned error: %v", err)
	}
	if routeModelName != "gpt-draw-1024x1024" {
		t.Fatalf("routeModelName = %q, want gpt-draw-1024x1024", routeModelName)
	}
	if requestedModelName != "gpt-draw-1536x1024" {
		t.Fatalf("requestedModelName = %q, want gpt-draw-1536x1024", requestedModelName)
	}
	if got := gjson.GetBytes(raw, "model").String(); got != "gpt-draw-1536x1024" {
		t.Fatalf("payload model = %q, want gpt-draw-1536x1024; payload=%s", got, string(raw))
	}
}

func TestImagesGenerationsNormalizesPortraitGptImage2Size(t *testing.T) {
	raw, routeModelName, requestedModelName, err := convertImagesGenerationRequestToResponses([]byte(`{"model":"gpt-image-2","prompt":"draw a poster","size":"1536x3072"}`))
	if err != nil {
		t.Fatalf("convertImagesGenerationRequestToResponses returned error: %v", err)
	}
	if routeModelName != "gpt-draw-1024x1024" {
		t.Fatalf("routeModelName = %q, want gpt-draw-1024x1024", routeModelName)
	}
	if requestedModelName != "gpt-draw-1024x1536" {
		t.Fatalf("requestedModelName = %q, want gpt-draw-1024x1536", requestedModelName)
	}
	if got := gjson.GetBytes(raw, "model").String(); got != "gpt-draw-1024x1536" {
		t.Fatalf("payload model = %q, want gpt-draw-1024x1536; payload=%s", got, string(raw))
	}
}

func TestImagesGenerationsInvalidSizeFallsBackToDefault(t *testing.T) {
	raw, routeModelName, requestedModelName, err := convertImagesGenerationRequestToResponses([]byte(`{"model":"gpt-image-2","prompt":"draw a poster","size":"4096"}`))
	if err != nil {
		t.Fatalf("convertImagesGenerationRequestToResponses returned error: %v", err)
	}
	if routeModelName != "gpt-draw-1024x1024" {
		t.Fatalf("routeModelName = %q, want gpt-draw-1024x1024", routeModelName)
	}
	if requestedModelName != "gpt-draw-1024x1024" {
		t.Fatalf("requestedModelName = %q, want gpt-draw-1024x1024", requestedModelName)
	}
	if got := gjson.GetBytes(raw, "model").String(); got != "gpt-draw-1024x1024" {
		t.Fatalf("payload model = %q, want gpt-draw-1024x1024; payload=%s", got, string(raw))
	}
}
