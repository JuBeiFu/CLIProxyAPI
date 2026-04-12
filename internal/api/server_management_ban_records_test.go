package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestServer_ManagementAuthBanRecordsRoute_ReturnsTodayRecords(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-secret")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	recordDir := filepath.Join(authDir, ".system", "ban-records")
	if err := os.MkdirAll(recordDir, 0o700); err != nil {
		t.Fatalf("failed to create record dir: %v", err)
	}

	recordPath := filepath.Join(recordDir, "banned-auth-records-"+time.Now().Format("2006-01-02")+".jsonl")
	recordBody := map[string]any{
		"name":       "codex-banned.json",
		"account":    "route-test@example.com",
		"provider":   "codex",
		"source":     "request",
		"reason":     "authorization lost",
		"created_at": "2026-04-01T00:00:00Z",
		"banned_at":  time.Now().UTC().Format(time.RFC3339),
	}
	raw, err := json.Marshal(recordBody)
	if err != nil {
		t.Fatalf("failed to marshal record body: %v", err)
	}
	if err := os.WriteFile(recordPath, append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("failed to seed ban record file: %v", err)
	}

	server := NewServer(
		&config.Config{
			AuthDir: authDir,
			Port:    8317,
		},
		coreauth.NewManager(nil, nil, nil),
		nil,
		filepath.Join(authDir, "config.yaml"),
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/ban-records", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Authorization", "Bearer test-secret")
	server.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Records []map[string]any `json:"records"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(payload.Records) != 1 {
		t.Fatalf("expected exactly one ban record, got %d", len(payload.Records))
	}
	if got := payload.Records[0]["account"]; got != "route-test@example.com" {
		t.Fatalf("expected account route-test@example.com, got %#v", got)
	}
}
