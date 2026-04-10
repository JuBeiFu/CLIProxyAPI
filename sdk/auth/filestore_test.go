package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileTokenStore_List_CodexUsesStableIDAndNewestDuplicate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)

	oldPath := filepath.Join(dir, "legacy-codex-free.json")
	newPath := filepath.Join(dir, "codex-same@example.com-team.json")
	oldData := []byte(`{"type":"codex","email":"same@example.com","plan_type":"free","refresh_token":"old-token"}`)
	newData := []byte(`{"type":"codex","email":"same@example.com","plan_type":"team","refresh_token":"new-token"}`)
	if err := os.WriteFile(oldPath, oldData, 0o600); err != nil {
		t.Fatalf("failed to write old auth file: %v", err)
	}
	if err := os.WriteFile(newPath, newData, 0o600); err != nil {
		t.Fatalf("failed to write new auth file: %v", err)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	newTime := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("failed to set old auth mtime: %v", err)
	}
	if err := os.Chtimes(newPath, newTime, newTime); err != nil {
		t.Fatalf("failed to set new auth mtime: %v", err)
	}

	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("len(auths) = %d, want 1", len(auths))
	}
	if auths[0].ID != "codex:same@example.com" {
		t.Fatalf("auth.ID = %q, want %q", auths[0].ID, "codex:same@example.com")
	}
	if auths[0].FileName != filepath.Base(newPath) {
		t.Fatalf("auth.FileName = %q, want %q", auths[0].FileName, filepath.Base(newPath))
	}
	if got, _ := auths[0].Metadata["plan_type"].(string); got != "team" {
		t.Fatalf("auth.Metadata[plan_type] = %q, want %q", got, "team")
	}
}

func TestFileTokenStore_ReadAuthFile_PreservesExcludedModelsMetadata(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)

	path := filepath.Join(dir, "codex-user@example.com.json")
	payload := `{"type":"codex","account":"user@example.com","plan_type":"free","excluded_models":["gpt-5.2"]}`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	auth, err := store.readAuthFile(path, dir)
	if err != nil {
		t.Fatalf("readAuthFile() error = %v", err)
	}
	if auth == nil {
		t.Fatal("readAuthFile() auth = nil")
	}
	if got := auth.Metadata["excluded_models"]; got == nil {
		t.Fatal("excluded_models metadata missing")
	}
	if got := auth.Attributes["path"]; got != path {
		t.Fatalf("path attribute = %q, want %q", got, path)
	}
}

func TestFileTokenStore_Delete_RemovesCodexFileByStableID(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)

	path := filepath.Join(dir, "codex-user@example.com-team.json")
	payload := `{"type":"codex","email":"user@example.com","plan_type":"team","refresh_token":"rt"}`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("len(auths) = %d, want 1", len(auths))
	}
	if auths[0].ID != "codex:user@example.com" {
		t.Fatalf("auth.ID = %q, want %q", auths[0].ID, "codex:user@example.com")
	}

	if err := store.Delete(context.Background(), auths[0].ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected auth file to be removed, stat err = %v", err)
	}
}

func TestExtractAccessToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		metadata map[string]any
		expected string
	}{
		{
			"antigravity top-level access_token",
			map[string]any{"access_token": "tok-abc"},
			"tok-abc",
		},
		{
			"gemini nested token.access_token",
			map[string]any{
				"token": map[string]any{"access_token": "tok-nested"},
			},
			"tok-nested",
		},
		{
			"top-level takes precedence over nested",
			map[string]any{
				"access_token": "tok-top",
				"token":        map[string]any{"access_token": "tok-nested"},
			},
			"tok-top",
		},
		{
			"empty metadata",
			map[string]any{},
			"",
		},
		{
			"whitespace-only access_token",
			map[string]any{"access_token": "   "},
			"",
		},
		{
			"wrong type access_token",
			map[string]any{"access_token": 12345},
			"",
		},
		{
			"token is not a map",
			map[string]any{"token": "not-a-map"},
			"",
		},
		{
			"nested whitespace-only",
			map[string]any{
				"token": map[string]any{"access_token": "  "},
			},
			"",
		},
		{
			"fallback to nested when top-level empty",
			map[string]any{
				"access_token": "",
				"token":        map[string]any{"access_token": "tok-fallback"},
			},
			"tok-fallback",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractAccessToken(tt.metadata)
			if got != tt.expected {
				t.Errorf("extractAccessToken() = %q, want %q", got, tt.expected)
			}
		})
	}
}
