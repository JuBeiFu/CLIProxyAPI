package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type serverBenchmarkExecutor struct{}

func (serverBenchmarkExecutor) Identifier() string { return "codex" }

func (serverBenchmarkExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{
		Payload: []byte(`{"id":"resp_server_bench","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`),
	}, nil
}

func (serverBenchmarkExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")}
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_server_bench\",\"object\":\"response\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Headers: http.Header{}, Chunks: chunks}, nil
}

func (serverBenchmarkExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (serverBenchmarkExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (serverBenchmarkExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func benchmarkServerSetup(b *testing.B, total int) *Server {
	b.Helper()

	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(serverBenchmarkExecutor{})

	modelRegistry := registry.GetGlobalRegistry()
	model := "gpt-5.4"
	for index := 0; index < total; index++ {
		authID := "bench-server-codex-" + strconv.Itoa(index)
		auth := &coreauth.Auth{
			ID:       authID,
			Provider: "codex",
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			b.Fatalf("Register(%s) error = %v", authID, err)
		}
		modelRegistry.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
	}
	b.Cleanup(func() {
		for index := 0; index < total; index++ {
			modelRegistry.UnregisterClient("bench-server-codex-" + strconv.Itoa(index))
		}
	})

	cfg := &config.Config{}
	return NewServer(cfg, manager, sdkaccess.NewManager(), "bench-config.yaml")
}

func BenchmarkServerResponsesHTTP16(b *testing.B) {
	benchmarkServerResponsesHTTP(b, 16, false)
}

func BenchmarkServerResponsesHTTP256(b *testing.B) {
	benchmarkServerResponsesHTTP(b, 256, false)
}

func BenchmarkServerResponsesStreamHTTP16(b *testing.B) {
	benchmarkServerResponsesHTTP(b, 16, true)
}

func BenchmarkServerResponsesStreamHTTP256(b *testing.B) {
	benchmarkServerResponsesHTTP(b, 256, true)
}

func benchmarkServerResponsesHTTP(b *testing.B, total int, stream bool) {
	server := benchmarkServerSetup(b, total)
	body := []byte(`{"model":"gpt-5.4","input":"hello","stream":false}`)
	if stream {
		body = []byte(`{"model":"gpt-5.4","input":"hello","stream":true}`)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		server.engine.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			b.Fatalf("unexpected status = %d, body=%s", recorder.Code, recorder.Body.String())
		}
		if recorder.Body.Len() == 0 {
			b.Fatal("empty response body")
		}
	}
}
