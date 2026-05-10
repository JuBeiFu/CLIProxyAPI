package handlers

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	. "github.com/router-for-me/CLIProxyAPI/v6/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

type handlersBenchmarkExecutor struct{}

func (handlersBenchmarkExecutor) Identifier() string { return "codex" }

func (handlersBenchmarkExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (handlersBenchmarkExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Headers: http.Header{}, Chunks: chunks}, nil
}

func (handlersBenchmarkExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (handlersBenchmarkExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (handlersBenchmarkExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func benchmarkBaseAPIHandlerSetup(b *testing.B, total int) *BaseAPIHandler {
	b.Helper()

	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(handlersBenchmarkExecutor{})

	modelRegistry := registry.GetGlobalRegistry()
	model := "gpt-5.4"
	for index := 0; index < total; index++ {
		authID := "bench-handler-codex-" + strconv.Itoa(index)
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
			modelRegistry.UnregisterClient("bench-handler-codex-" + strconv.Itoa(index))
		}
	})

	return NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
}

func BenchmarkBaseAPIHandlerExecuteWithAuthManager16(b *testing.B) {
	benchmarkBaseAPIHandlerExecuteWithAuthManager(b, 16)
}

func BenchmarkBaseAPIHandlerExecuteWithAuthManager256(b *testing.B) {
	benchmarkBaseAPIHandlerExecuteWithAuthManager(b, 256)
}

func BenchmarkBaseAPIHandlerExecuteStreamWithAuthManager16(b *testing.B) {
	benchmarkBaseAPIHandlerExecuteStreamWithAuthManager(b, 16)
}

func BenchmarkBaseAPIHandlerExecuteStreamWithAuthManager256(b *testing.B) {
	benchmarkBaseAPIHandlerExecuteStreamWithAuthManager(b, 256)
}

func benchmarkBaseAPIHandlerExecuteWithAuthManager(b *testing.B, total int) {
	base := benchmarkBaseAPIHandlerSetup(b, total)
	ctx := context.Background()
	rawJSON := []byte(`{"model":"gpt-5.4","input":"hello","stream":false}`)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		payload, _, errMsg := base.ExecuteWithAuthManager(ctx, OpenaiResponse, "gpt-5.4", rawJSON, "")
		if errMsg != nil {
			b.Fatalf("ExecuteWithAuthManager error = %v", errMsg.Error)
		}
		if len(payload) == 0 {
			b.Fatal("empty payload")
		}
	}
}

func benchmarkBaseAPIHandlerExecuteStreamWithAuthManager(b *testing.B, total int) {
	base := benchmarkBaseAPIHandlerSetup(b, total)
	ctx := context.Background()
	rawJSON := []byte(`{"model":"gpt-5.4","input":"hello","stream":true}`)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dataChan, _, errChan := base.ExecuteStreamWithAuthManager(ctx, OpenaiResponse, "gpt-5.4", rawJSON, "")
		for chunk := range dataChan {
			if len(chunk) == 0 {
				b.Fatal("empty stream chunk")
			}
		}
		for errMsg := range errChan {
			if errMsg != nil {
				b.Fatalf("ExecuteStreamWithAuthManager error = %v", errMsg.Error)
			}
		}
	}
}
