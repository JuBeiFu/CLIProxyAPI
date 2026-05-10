package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func newCodexExecutorBenchmarkServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_bench\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_bench\",\"object\":\"response\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
}

func benchmarkCodexExecutorAuths(serverURL string, total int) []*cliproxyauth.Auth {
	auths := make([]*cliproxyauth.Auth, 0, total)
	for index := 0; index < total; index++ {
		auths = append(auths, &cliproxyauth.Auth{
			ID:       "bench-codex-auth-" + strconv.Itoa(index),
			Provider: "codex",
			Attributes: map[string]string{
				"base_url": serverURL,
				"api_key":  "bench-key",
			},
		})
	}
	return auths
}

func BenchmarkCodexExecutorExecuteRoundRobin16(b *testing.B) {
	benchmarkCodexExecutorExecuteRoundRobin(b, 16)
}

func BenchmarkCodexExecutorExecuteRoundRobin256(b *testing.B) {
	benchmarkCodexExecutorExecuteRoundRobin(b, 256)
}

func BenchmarkCodexExecutorExecuteStreamRoundRobin16(b *testing.B) {
	benchmarkCodexExecutorExecuteStreamRoundRobin(b, 16)
}

func BenchmarkCodexExecutorExecuteStreamRoundRobin256(b *testing.B) {
	benchmarkCodexExecutorExecuteStreamRoundRobin(b, 256)
}

func benchmarkCodexExecutorExecuteRoundRobin(b *testing.B, total int) {
	server := newCodexExecutorBenchmarkServer()
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auths := benchmarkCodexExecutorAuths(server.URL, total)
	ctx := context.Background()
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"Say ok"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       false,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		auth := auths[i%len(auths)]
		if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
			b.Fatalf("Execute error = %v", err)
		}
	}
}

func benchmarkCodexExecutorExecuteStreamRoundRobin(b *testing.B, total int) {
	server := newCodexExecutorBenchmarkServer()
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auths := benchmarkCodexExecutorAuths(server.URL, total)
	ctx := context.Background()
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello","stream":true}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		auth := auths[i%len(auths)]
		result, err := executor.ExecuteStream(ctx, auth, req, opts)
		if err != nil {
			b.Fatalf("ExecuteStream error = %v", err)
		}
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				b.Fatalf("stream chunk error = %v", chunk.Err)
			}
		}
	}
}
