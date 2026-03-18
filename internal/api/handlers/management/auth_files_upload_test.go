package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestUploadAuthFile_CodexSameAccountOverridesLegacyPathIDCredential(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	oldName := "legacy-codex-free.json"
	oldPath := filepath.Join(authDir, oldName)
	oldData := `{"type":"codex","email":"same@example.com","plan_type":"free","refresh_token":"old-token"}`
	if err := os.WriteFile(oldPath, []byte(oldData), 0o600); err != nil {
		t.Fatalf("failed to write old auth file: %v", err)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	oldID := oldName
	if strings.EqualFold(os.Getenv("OS"), "Windows_NT") {
		oldID = strings.ToLower(oldID)
	}
	oldAuth := &coreauth.Auth{
		ID:       oldID,
		Provider: "codex",
		FileName: oldName,
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": oldPath,
		},
		Metadata: map[string]any{
			"type":      "codex",
			"email":     "same@example.com",
			"plan_type": "free",
		},
	}
	if _, err := manager.Register(context.Background(), oldAuth); err != nil {
		t.Fatalf("failed to register old auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	newData := `{"type":"codex","email":"same@example.com","plan_type":"team","refresh_token":"new-token"}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files?name=imported.json", strings.NewReader(newData))
	ctx.Request = req

	h.UploadAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected upload status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expected old file to be removed, stat err: %v", err)
	}

	stored, ok := manager.GetByID("codex:same@example.com")
	if !ok {
		t.Fatal("expected codex auth to exist by account-based id")
	}
	if got, _ := stored.Metadata["plan_type"].(string); got != "team" {
		t.Fatalf("stored plan_type = %q, want team", got)
	}
}

func TestUploadAuthFile_CodexSameAccountOverridesExistingCredential(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	oldName := "legacy-codex-free.json"
	oldPath := filepath.Join(authDir, oldName)
	oldData := `{"type":"codex","email":"same@example.com","plan_type":"free","refresh_token":"old-token"}`
	if err := os.WriteFile(oldPath, []byte(oldData), 0o600); err != nil {
		t.Fatalf("failed to write old auth file: %v", err)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	oldAuth := &coreauth.Auth{
		ID:       "codex:same@example.com",
		Provider: "codex",
		FileName: oldName,
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": oldPath,
		},
		Metadata: map[string]any{
			"type":      "codex",
			"email":     "same@example.com",
			"plan_type": "free",
		},
	}
	if _, err := manager.Register(context.Background(), oldAuth); err != nil {
		t.Fatalf("failed to register old auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	newData := `{"type":"codex","email":"same@example.com","plan_type":"team","refresh_token":"new-token"}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files?name=imported.json", strings.NewReader(newData))
	ctx.Request = req

	h.UploadAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected upload status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expected old file to be removed, stat err: %v", err)
	}

	expectedName := codex.CredentialFileNameFromMetadata(map[string]any{
		"email":     "same@example.com",
		"plan_type": "team",
	}, true)
	newPath := filepath.Join(authDir, expectedName)
	content, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("failed to read new auth file: %v", err)
	}
	if !strings.Contains(string(content), `"new-token"`) {
		t.Fatalf("expected new auth file content to be written, got %s", string(content))
	}

	stored, ok := manager.GetByID("codex:same@example.com")
	if !ok {
		t.Fatal("expected codex auth to exist by account-based id")
	}
	if stored.FileName != expectedName {
		t.Fatalf("stored filename = %q, want %q", stored.FileName, expectedName)
	}
	if got, _ := stored.Metadata["plan_type"].(string); got != "team" {
		t.Fatalf("stored plan_type = %q, want team", got)
	}

	listRec := httptest.NewRecorder()
	listCtx, _ := gin.CreateTestContext(listRec)
	listReq := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files?provider=codex&plan=team", nil)
	listCtx.Request = listReq
	h.ListAuthFiles(listCtx)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d with body %s", http.StatusOK, listRec.Code, listRec.Body.String())
	}
	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to unmarshal list response: %v", err)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("expected 1 filtered file, got %d", len(payload.Files))
	}
	if got, _ := payload.Files[0]["name"].(string); got != expectedName {
		t.Fatalf("listed file name = %q, want %q", got, expectedName)
	}
}
