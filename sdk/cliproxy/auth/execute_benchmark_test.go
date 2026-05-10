package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type executeBenchmarkExecutor struct {
	id string
}

func (e executeBenchmarkExecutor) Identifier() string { return e.id }

func (e executeBenchmarkExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e executeBenchmarkExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Headers: http.Header{}, Chunks: chunks}, nil
}

func (e executeBenchmarkExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e executeBenchmarkExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e executeBenchmarkExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func benchmarkExecuteManagerSetup(b *testing.B, total int) (*Manager, []string, cliproxyexecutor.Request) {
	b.Helper()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["codex"] = executeBenchmarkExecutor{id: "codex"}

	reg := registry.GetGlobalRegistry()
	model := "gpt-5.4"
	for index := 0; index < total; index++ {
		auth := &Auth{
			ID:       fmt.Sprintf("bench-codex-%04d", index),
			Provider: "codex",
		}
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			b.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
		reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: model}})
	}
	manager.syncScheduler()
	b.Cleanup(func() {
		for index := 0; index < total; index++ {
			reg.UnregisterClient(fmt.Sprintf("bench-codex-%04d", index))
		}
	})

	return manager, []string{"codex"}, cliproxyexecutor.Request{Model: model}
}

func benchmarkDrainStream(b *testing.B, result *cliproxyexecutor.StreamResult) {
	b.Helper()
	if result == nil {
		b.Fatal("stream result is nil")
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			b.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}
}

func BenchmarkManagerExecuteImmediate16(b *testing.B) {
	manager, providers, req := benchmarkExecuteManagerSetup(b, 16)
	ctx := context.Background()
	opts := cliproxyexecutor.Options{}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := manager.Execute(ctx, providers, req, opts); err != nil {
			b.Fatalf("Execute error = %v", err)
		}
	}
}

func BenchmarkManagerExecuteImmediate256(b *testing.B) {
	manager, providers, req := benchmarkExecuteManagerSetup(b, 256)
	ctx := context.Background()
	opts := cliproxyexecutor.Options{}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := manager.Execute(ctx, providers, req, opts); err != nil {
			b.Fatalf("Execute error = %v", err)
		}
	}
}

func BenchmarkManagerExecuteStreamImmediate16(b *testing.B) {
	manager, providers, req := benchmarkExecuteManagerSetup(b, 16)
	ctx := context.Background()
	opts := cliproxyexecutor.Options{}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := manager.ExecuteStream(ctx, providers, req, opts)
		if err != nil {
			b.Fatalf("ExecuteStream error = %v", err)
		}
		benchmarkDrainStream(b, result)
	}
}

func BenchmarkManagerExecuteStreamImmediate256(b *testing.B) {
	manager, providers, req := benchmarkExecuteManagerSetup(b, 256)
	ctx := context.Background()
	opts := cliproxyexecutor.Options{}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := manager.ExecuteStream(ctx, providers, req, opts)
		if err != nil {
			b.Fatalf("ExecuteStream error = %v", err)
		}
		benchmarkDrainStream(b, result)
	}
}
