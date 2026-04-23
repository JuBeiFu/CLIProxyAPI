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

func TestFileTokenStoreListSkipsRevokedTombstonedAuth(t *testing.T) {
	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)

	authPath := filepath.Join(dir, "codex-skip.json")
	payload := map[string]any{
		"type":  "codex",
		"email": "skip@example.com",
	}
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		t.Fatalf("marshal payload: %v", errMarshal)
	}
	if errWrite := os.WriteFile(authPath, raw, 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	cliproxyauth.RegisterRevokedAuthTombstone(&cliproxyauth.Auth{
		ID:       "codex-skip.json",
		FileName: "codex-skip.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": authPath,
		},
	}, "revoked: refresh_token_reused", time.Now())

	auths, errList := store.List(context.Background())
	if errList != nil {
		t.Fatalf("List returned error: %v", errList)
	}
	if len(auths) != 0 {
		t.Fatalf("expected tombstoned auth file to be skipped, got %d auths", len(auths))
	}
}
