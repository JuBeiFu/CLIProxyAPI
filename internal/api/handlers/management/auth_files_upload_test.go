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
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type recordingCodexManagerExecutor struct {
	authIDs []string
}

func (e *recordingCodexManagerExecutor) Identifier() string { return "codex" }

func (e *recordingCodexManagerExecutor) Execute(_ context.Context, auth *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.authIDs = append(e.authIDs, auth.ID)
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *recordingCodexManagerExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *recordingCodexManagerExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *recordingCodexManagerExecutor) CountTokens(_ context.Context, auth *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.authIDs = append(e.authIDs, auth.ID)
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *recordingCodexManagerExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

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

func TestUploadAuthFile_CodexPlanUpgradeAffectsManagerSelectionForGPT53Codex(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	oldName := "legacy-codex-free.json"
	oldPath := filepath.Join(authDir, oldName)
	oldData := `{"type":"codex","email":"same@example.com","plan_type":"free","refresh_token":"old-token"}`
	if err := os.WriteFile(oldPath, []byte(oldData), 0o600); err != nil {
		t.Fatalf("failed to write old auth file: %v", err)
	}

	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	exec := &recordingCodexManagerExecutor{}
	manager.RegisterExecutor(exec)

	upgradedAuth := &coreauth.Auth{
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
	if _, err := manager.Register(context.Background(), upgradedAuth); err != nil {
		t.Fatalf("failed to register original auth: %v", err)
	}

	otherAuth := &coreauth.Auth{
		ID:       "codex:other@example.com",
		Provider: "codex",
		FileName: "other-codex-free.json",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path":     filepath.Join(authDir, "other-codex-free.json"),
			"priority": "100",
		},
		Metadata: map[string]any{
			"type":      "codex",
			"email":     "other@example.com",
			"plan_type": "free",
		},
	}
	if _, err := manager.Register(context.Background(), otherAuth); err != nil {
		t.Fatalf("failed to register secondary auth: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(upgradedAuth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.3-codex"}})
	reg.RegisterClient(otherAuth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.3-codex"}})
	t.Cleanup(func() {
		reg.UnregisterClient(upgradedAuth.ID)
		reg.UnregisterClient(otherAuth.ID)
	})

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

	stored, ok := manager.GetByID(upgradedAuth.ID)
	if !ok || stored == nil {
		t.Fatal("expected upgraded auth to remain registered")
	}
	if got, _ := stored.Metadata["plan_type"].(string); got != "team" {
		t.Fatalf("stored plan_type = %q, want %q", got, "team")
	}

	_, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.3-codex"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("manager.Execute() error = %v", err)
	}
	if len(exec.authIDs) != 1 {
		t.Fatalf("execute auth count = %d, want 1", len(exec.authIDs))
	}
	if exec.authIDs[0] != upgradedAuth.ID {
		t.Fatalf("executed auth ID = %q, want %q", exec.authIDs[0], upgradedAuth.ID)
	}
}

func TestListAuthFilesAndPatchFields_ExposeAndUpdateNotePriority(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	authName := "gemini-note.json"
	authPath := filepath.Join(authDir, authName)
	authData := `{"type":"gemini","email":"note@example.com","priority":"7","note":"from disk"}`
	if err := os.WriteFile(authPath, []byte(authData), 0o600); err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       authName,
		Provider: "gemini-cli",
		FileName: authName,
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path":     authPath,
			"priority": "7",
			"note":     "from attrs",
		},
		Metadata: map[string]any{
			"type":     "gemini",
			"email":    "note@example.com",
			"priority": "7",
			"note":     "from metadata",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("failed to register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	listRec := httptest.NewRecorder()
	listCtx, _ := gin.CreateTestContext(listRec)
	listCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
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
		t.Fatalf("expected 1 file, got %d", len(payload.Files))
	}
	if got := payload.Files[0]["note"]; got != "from attrs" {
		t.Fatalf("list note = %v, want %q", got, "from attrs")
	}
	if got := payload.Files[0]["priority"]; int(got.(float64)) != 7 {
		t.Fatalf("list priority = %v, want 7", got)
	}

	patchRec := httptest.NewRecorder()
	patchCtx, _ := gin.CreateTestContext(patchRec)
	patchReq := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(`{"name":"gemini-note.json","priority":9,"note":"updated note"}`))
	patchReq.Header.Set("Content-Type", "application/json")
	patchCtx.Request = patchReq
	h.PatchAuthFileFields(patchCtx)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("expected patch status %d, got %d with body %s", http.StatusOK, patchRec.Code, patchRec.Body.String())
	}

	updated, ok := manager.GetByID(authName)
	if !ok || updated == nil {
		t.Fatal("expected updated auth")
	}
	if got := updated.Attributes["priority"]; got != "9" {
		t.Fatalf("updated priority attr = %q, want %q", got, "9")
	}
	if got := updated.Attributes["note"]; got != "updated note" {
		t.Fatalf("updated note attr = %q, want %q", got, "updated note")
	}
	if got, _ := updated.Metadata["priority"].(int); got != 9 {
		t.Fatalf("updated metadata priority = %v, want 9", updated.Metadata["priority"])
	}
	if got, _ := updated.Metadata["note"].(string); got != "updated note" {
		t.Fatalf("updated metadata note = %q, want %q", got, "updated note")
	}
}

func TestListAuthFilesFromDisk_IgnoresNonStringNoteFallback(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	authName := "gemini-note-non-string.json"
	authPath := filepath.Join(authDir, authName)
	authData := `{"type":"gemini","email":"nonstr@example.com","priority":"7","note":true}`
	if err := os.WriteFile(authPath, []byte(authData), 0o600); err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}

	h := &Handler{
		cfg: &config.Config{AuthDir: authDir},
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	ctx.Request = req

	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("files len = %d, want 1", len(payload.Files))
	}
	if got, exists := payload.Files[0]["note"]; exists {
		t.Fatalf("expected note to be omitted for non-string JSON value, got %v", got)
	}
}
