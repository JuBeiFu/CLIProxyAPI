package management

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestBuildAuthFromFileData_CodexExtractsPlanTypeAttribute(t *testing.T) {
	t.Parallel()

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	authPath := filepath.Join(t.TempDir(), "codex-user@example.com-plus.json")
	idToken := testCodexJWT(t, "plus", "acct_plus_123")
	data := []byte(`{"type":"codex","email":"codex-user@example.com","id_token":"` + idToken + `"}`)

	auth, err := h.buildAuthFromFileData(authPath, data)
	if err != nil {
		t.Fatalf("buildAuthFromFileData returned error: %v", err)
	}
	if auth == nil {
		t.Fatal("expected auth")
	}
	if got := strings.TrimSpace(auth.Attributes["plan_type"]); got != "plus" {
		t.Fatalf("attributes.plan_type = %q, want %q", got, "plus")
	}
}

func TestBuildAuthFileEntry_CodexPromotesPlanTypeToTopLevel(t *testing.T) {
	t.Parallel()

	auth := &coreauth.Auth{
		ID:       "codex-user@example.com-plus.json",
		FileName: "codex-user@example.com-plus.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path":      `C:\auths\codex-user@example.com-plus.json`,
			"plan_type": "plus",
		},
		Metadata: map[string]any{
			"email":    "codex-user@example.com",
			"id_token": testCodexJWT(t, "plus", "acct_plus_123"),
		},
	}

	h := &Handler{}
	entry := h.buildAuthFileEntry(auth)
	if entry == nil {
		t.Fatal("expected entry")
	}
	if got, _ := entry["plan_type"].(string); got != "plus" {
		t.Fatalf("entry.plan_type = %q, want %q", got, "plus")
	}
	idTokenRaw := entry["id_token"]
	var idToken map[string]any
	switch typed := idTokenRaw.(type) {
	case map[string]any:
		idToken = typed
	case gin.H:
		idToken = map[string]any(typed)
	default:
		t.Fatalf("entry.id_token type = %T, want map-like object", idTokenRaw)
	}
	if got, _ := idToken["plan_type"].(string); got != "plus" {
		t.Fatalf("entry.id_token.plan_type = %q, want %q", got, "plus")
	}
}

func TestSaveTokenRecord_CodexMetadataRoundTripPreservesPlanType(t *testing.T) {
	t.Parallel()

	authDir := t.TempDir()
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = sdkAuthStoreForTests(authDir)

	idToken := testCodexJWT(t, "plus", "acct_plus_123")
	record := &coreauth.Auth{
		ID:       "codex-user@example.com-plus.json",
		FileName: "codex-user@example.com-plus.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":       "codex",
			"email":      "codex-user@example.com",
			"id_token":   idToken,
			"account_id": "acct_plus_123",
		},
	}

	savedPath, err := h.saveTokenRecord(context.Background(), record)
	if err != nil {
		t.Fatalf("saveTokenRecord returned error: %v", err)
	}

	raw, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatalf("read saved auth file: %v", err)
	}

	rebuilt, err := h.buildAuthFromFileData(savedPath, raw)
	if err != nil {
		t.Fatalf("buildAuthFromFileData returned error: %v", err)
	}
	if rebuilt == nil {
		t.Fatal("expected rebuilt auth")
	}
	if got := strings.TrimSpace(rebuilt.Attributes["plan_type"]); got != "plus" {
		t.Fatalf("rebuilt attributes.plan_type = %q, want %q", got, "plus")
	}
	if got, _ := rebuilt.Metadata["account_id"].(string); got != "acct_plus_123" {
		t.Fatalf("rebuilt metadata.account_id = %q, want %q", got, "acct_plus_123")
	}
}

func sdkAuthStoreForTests(baseDir string) coreauth.Store {
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(baseDir)
	return store
}

func testCodexJWT(t *testing.T, planType, accountID string) string {
	t.Helper()

	headerBytes, err := json.Marshal(map[string]any{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	payloadBytes, err := json.Marshal(map[string]any{
		"email": "codex-user@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type":  planType,
			"chatgpt_account_id": accountID,
		},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	encode := func(raw []byte) string {
		return strings.TrimRight(base64.URLEncoding.EncodeToString(raw), "=")
	}

	return encode(headerBytes) + "." + encode(payloadBytes) + "."
}
