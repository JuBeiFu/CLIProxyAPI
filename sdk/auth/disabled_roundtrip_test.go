package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestFileTokenStore_SaveAndListPreservesDisabledState(t *testing.T) {
	baseDir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	path := filepath.Join(baseDir, "codex-disabled.json")
	if err := os.WriteFile(path, []byte(`{"type":"codex","email":"disabled@example.com"}`), 0o600); err != nil {
		t.Fatalf("write initial auth: %v", err)
	}

	auth := &cliproxyauth.Auth{
		ID:       "codex-disabled.json",
		FileName: "codex-disabled.json",
		Provider: "codex",
		Disabled: true,
		Status:   cliproxyauth.StatusDisabled,
		Attributes: map[string]string{
			"path": path,
		},
		Metadata: map[string]any{
			"type":  "codex",
			"email": "disabled@example.com",
		},
	}

	if _, err := store.Save(context.Background(), auth); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved auth: %v", err)
	}
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("decode saved auth: %v", err)
	}
	if saved["disabled"] != true {
		t.Fatalf("expected disabled=true in saved auth JSON, got %s", string(raw))
	}

	items, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("List returned %d items, want 1", len(items))
	}
	if !items[0].Disabled || items[0].Status != cliproxyauth.StatusDisabled {
		t.Fatalf("expected disabled auth after reload, got disabled=%v status=%q", items[0].Disabled, items[0].Status)
	}
}
