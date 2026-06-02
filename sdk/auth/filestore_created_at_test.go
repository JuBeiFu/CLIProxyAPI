package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// TestFileTokenStore_CreatedAtPersistsAndSurvivesMtimeBump proves the core
// invariant: created_at is stamped into the saved JSON and, on reload, is read
// from there rather than the file mtime — so a refresh that rewrites the file
// (bumping mtime) does not drift the account's creation marker.
func TestFileTokenStore_CreatedAtPersistsAndSurvivesMtimeBump(t *testing.T) {
	baseDir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	path := filepath.Join(baseDir, "codex-created.json")
	if err := os.WriteFile(path, []byte(`{"type":"codex","email":"c@example.com"}`), 0o600); err != nil {
		t.Fatalf("write initial auth: %v", err)
	}

	created := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	auth := &cliproxyauth.Auth{
		ID:         "codex-created.json",
		FileName:   "codex-created.json",
		Provider:   "codex",
		CreatedAt:  created,
		Attributes: map[string]string{"path": path},
		Metadata:   map[string]any{"type": "codex", "email": "c@example.com"},
	}

	if _, err := store.Save(context.Background(), auth); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	// The persisted JSON must carry created_at as RFC3339.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved auth: %v", err)
	}
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("decode saved auth: %v", err)
	}
	if saved["created_at"] != created.UTC().Format(time.RFC3339) {
		t.Fatalf("expected created_at %s in JSON, got %v (raw=%s)", created.UTC().Format(time.RFC3339), saved["created_at"], string(raw))
	}

	// Simulate a refresh rewrite bumping the file mtime far into the future.
	future := time.Now().Add(48 * time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	items, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("List returned %d items, want 1", len(items))
	}
	if !items[0].CreatedAt.Equal(created) {
		t.Fatalf("created_at drifted to mtime: got %s want %s", items[0].CreatedAt, created)
	}
}

// TestFileTokenStore_CreatedAtZeroFallsBackToMtime confirms a zero CreatedAt is
// never persisted, and load then falls back to the file mtime (legacy behavior).
func TestFileTokenStore_CreatedAtZeroFallsBackToMtime(t *testing.T) {
	baseDir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	path := filepath.Join(baseDir, "codex-zero.json")
	if err := os.WriteFile(path, []byte(`{"type":"codex","email":"z@example.com"}`), 0o600); err != nil {
		t.Fatalf("write initial auth: %v", err)
	}

	auth := &cliproxyauth.Auth{
		ID:         "codex-zero.json",
		FileName:   "codex-zero.json",
		Provider:   "codex",
		Attributes: map[string]string{"path": path},
		Metadata:   map[string]any{"type": "codex", "email": "z@example.com"},
	}
	if _, err := store.Save(context.Background(), auth); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var saved map[string]any
	_ = json.Unmarshal(raw, &saved)
	if _, ok := saved["created_at"]; ok {
		t.Fatalf("zero CreatedAt must not write created_at: %s", string(raw))
	}

	// Reload: CreatedAt falls back to the file mtime (non-zero).
	items, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].CreatedAt.IsZero() {
		t.Fatalf("expected mtime fallback (non-zero created_at), got %+v", items)
	}
}
