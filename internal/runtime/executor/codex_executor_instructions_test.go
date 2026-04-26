package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorExecuteNormalizesNullInstructions(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","instructions":null,"input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/responses" {
		t.Fatalf("path = %q, want %q", gotPath, "/responses")
	}
	if gjson.GetBytes(gotBody, "instructions").Type != gjson.String {
		t.Fatalf("instructions type = %v, want string", gjson.GetBytes(gotBody, "instructions").Type)
	}
	if gjson.GetBytes(gotBody, "instructions").String() != "" {
		t.Fatalf("instructions = %q, want empty string", gjson.GetBytes(gotBody, "instructions").String())
	}
}

func TestCodexExecutorExecuteMapsUnsupportedCountryResponseFailedToInternalError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.failed","response":{"status":"failed","error":{"code":"unsupported_country_region_territory","message":"Country, region, or territory not supported","param":null,"type":"request_forbidden"}}}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"draw"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err == nil {
		t.Fatal("expected Execute error, got nil")
	}
	statusProvider, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("expected status error, got %T", err)
	}
	if got := statusProvider.StatusCode(); got != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; err=%v", got, http.StatusInternalServerError, err)
	}
	if got := gjson.Get(err.Error(), "error.code").String(); got != "unsupported_country_region_territory" {
		t.Fatalf("error.code = %q, want unsupported_country_region_territory; err=%v", got, err)
	}
}

func TestCodexExecutorExecuteMapsUnsupportedCountryHTTPErrorToInternalError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"unsupported_country_region_territory","message":"Country, region, or territory not supported","param":null,"type":"request_forbidden"}}`))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"draw","tools":[{"type":"image_generation"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
		Metadata: map[string]any{
			cliproxyexecutor.ImageGenerationRequestMetadataKey: true,
		},
	})
	if err == nil {
		t.Fatal("expected Execute error, got nil")
	}
	statusProvider, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("expected status error, got %T", err)
	}
	if got := statusProvider.StatusCode(); got != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; err=%v", got, http.StatusInternalServerError, err)
	}
	if got := gjson.Get(err.Error(), "error.code").String(); got != "unsupported_country_region_territory" {
		t.Fatalf("error.code = %q, want unsupported_country_region_territory; err=%v", got, err)
	}
}

func TestCodexExecutorExecuteStreamNormalizesNullInstructions(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","instructions":null,"input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for range result.Chunks {
	}
	if gotPath != "/responses" {
		t.Fatalf("path = %q, want %q", gotPath, "/responses")
	}
	if gjson.GetBytes(gotBody, "instructions").Type != gjson.String {
		t.Fatalf("instructions type = %v, want string", gjson.GetBytes(gotBody, "instructions").Type)
	}
	if gjson.GetBytes(gotBody, "instructions").String() != "" {
		t.Fatalf("instructions = %q, want empty string", gjson.GetBytes(gotBody, "instructions").String())
	}
}

func TestCodexExecutorCountTokensTreatsNullInstructionsAsEmpty(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})

	nullResp, err := executor.CountTokens(context.Background(), nil, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","instructions":null,"input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("CountTokens(null) error: %v", err)
	}

	emptyResp, err := executor.CountTokens(context.Background(), nil, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","instructions":"","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("CountTokens(empty) error: %v", err)
	}

	if string(nullResp.Payload) != string(emptyResp.Payload) {
		t.Fatalf("token count payload mismatch:\nnull=%s\nempty=%s", string(nullResp.Payload), string(emptyResp.Payload))
	}
}
