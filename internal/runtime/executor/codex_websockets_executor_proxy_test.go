package executor

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestDialCodexWebsocket_AllInvalidProxiesReturnsError(t *testing.T) {
	exec := NewCodexWebsocketsExecutor(&config.Config{
		SDKConfig: config.SDKConfig{
			ProxyURL: "bad://proxy:1,also-bad://proxy:2",
		},
	})

	conn, resp, err := exec.dialCodexWebsocket(context.Background(), nil, "wss://example.invalid", http.Header{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if conn != nil {
		t.Fatalf("expected nil conn, got %v", conn)
	}
	if resp != nil {
		t.Fatalf("expected nil resp, got %v", resp.Status)
	}
}

