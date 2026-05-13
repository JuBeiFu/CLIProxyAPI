package auth

import (
	"context"
	"errors"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestReadStreamBootstrapCapsReplayableBufferWithoutEndingRetryWindow(t *testing.T) {
	ch := make(chan cliproxyexecutor.StreamChunk, streamBootstrapMaxBufferedChunks+1)
	for i := 0; i < streamBootstrapMaxBufferedChunks+1; i++ {
		ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {}\n\n"), BootstrapReplayable: true}
	}
	close(ch)

	buffered, closed, err := readStreamBootstrap(context.Background(), ch)
	if err != nil {
		t.Fatalf("readStreamBootstrap returned error: %v", err)
	}
	if !closed {
		t.Fatal("closed = false, want true after replayable-only stream closes")
	}
	if got := len(buffered); got != streamBootstrapMaxBufferedChunks {
		t.Fatalf("buffered chunks = %d, want %d", got, streamBootstrapMaxBufferedChunks)
	}
}

func TestReadStreamBootstrapReturnsErrorAfterReplayableBufferCap(t *testing.T) {
	ch := make(chan cliproxyexecutor.StreamChunk, streamBootstrapMaxBufferedChunks+2)
	for i := 0; i < streamBootstrapMaxBufferedChunks+1; i++ {
		ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {}\n\n"), BootstrapReplayable: true}
	}
	wantErr := errors.New("upstream failed before visible output")
	ch <- cliproxyexecutor.StreamChunk{Err: wantErr}
	close(ch)

	buffered, closed, err := readStreamBootstrap(context.Background(), ch)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if closed {
		t.Fatal("closed = true, want false for error")
	}
	if len(buffered) != 0 {
		t.Fatalf("buffered chunks = %d, want 0 on bootstrap error", len(buffered))
	}
}

func TestReadStreamBootstrapStopsAtVisiblePayload(t *testing.T) {
	ch := make(chan cliproxyexecutor.StreamChunk, 3)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.created\"}\n\n"), BootstrapReplayable: true}
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")}
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.completed\"}\n\n")}
	close(ch)

	buffered, closed, err := readStreamBootstrap(context.Background(), ch)
	if err != nil {
		t.Fatalf("readStreamBootstrap returned error: %v", err)
	}
	if closed {
		t.Fatal("closed = true, want false")
	}
	if got := len(buffered); got != 2 {
		t.Fatalf("buffered chunks = %d, want 2", got)
	}
}
