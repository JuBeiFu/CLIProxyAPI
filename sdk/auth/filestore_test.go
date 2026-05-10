package auth

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

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

func TestFileTokenStoreList_CodexExtractsPlanTypeFromIDToken(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)

	path := filepath.Join(baseDir, "codex-user@example.com-plus.json")
	idToken := testCodexJWTForFileStore(t, "plus", "acct_plus_123")
	raw := []byte(`{"type":"codex","email":"codex-user@example.com","id_token":"` + idToken + `"}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	items, err := store.List(t.Context())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("List returned %d items, want 1", len(items))
	}
	if got := strings.TrimSpace(items[0].Attributes["plan_type"]); got != "plus" {
		t.Fatalf("attributes.plan_type = %q, want %q", got, "plus")
	}
}

func TestFileTokenStoreList_CodexSessionUsesTopLevelPlanAndAccountID(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)

	path := filepath.Join(baseDir, "codex-session-user@example.com-plus.json")
	raw := []byte(`{"type":"codex_session","email":"codex-session-user@example.com","plan_type":"plus","account_id":"acct_session_123"}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	items, err := store.List(t.Context())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("List returned %d items, want 1", len(items))
	}
	if got := strings.TrimSpace(items[0].Provider); got != "codex" {
		t.Fatalf("provider = %q, want %q", got, "codex")
	}
	if got := strings.TrimSpace(items[0].Attributes["plan_type"]); got != "plus" {
		t.Fatalf("attributes.plan_type = %q, want %q", got, "plus")
	}
	if got, _ := items[0].Metadata["account_id"].(string); got != "acct_session_123" {
		t.Fatalf("metadata.account_id = %q, want %q", got, "acct_session_123")
	}
	if got, _ := items[0].Metadata["chatgpt_account_id"].(string); got != "acct_session_123" {
		t.Fatalf("metadata.chatgpt_account_id = %q, want %q", got, "acct_session_123")
	}
}

func TestFileTokenStoreList_CodexCarriesBaseURLAttr(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)

	path := filepath.Join(baseDir, "codex-base-url.json")
	raw := []byte(`{"type":"codex","base_url":"http://127.0.0.1:18080","headers":{"X-Mock-Auth-ID":"auth-0001"}}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	items, err := store.List(t.Context())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("List returned %d items, want 1", len(items))
	}
	if got := strings.TrimSpace(items[0].Attributes["base_url"]); got != "http://127.0.0.1:18080" {
		t.Fatalf("attributes.base_url = %q, want %q", got, "http://127.0.0.1:18080")
	}
	if got := strings.TrimSpace(items[0].Attributes["header:X-Mock-Auth-ID"]); got != "auth-0001" {
		t.Fatalf("attributes.header:X-Mock-Auth-ID = %q, want %q", got, "auth-0001")
	}
}

func testCodexJWTForFileStore(t *testing.T, planType, accountID string) string {
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

func TestReadAuthFile_ParsesQuotaState(t *testing.T) {
	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)

	futureTime := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	authData := map[string]any{
		"type":  "codex",
		"token": "test-token",
		"quota_state": map[string]any{
			"exceeded":        true,
			"reason":          "usage_limit",
			"next_recover_at": futureTime.Format(time.RFC3339),
			"backoff_level":   float64(3),
		},
	}
	raw, _ := json.Marshal(authData)
	path := filepath.Join(dir, "test-auth.json")
	os.WriteFile(path, raw, 0o600)

	auth, err := store.readAuthFile(path, dir)
	if err != nil {
		t.Fatalf("readAuthFile error: %v", err)
	}
	if !auth.Quota.Exceeded {
		t.Error("expected Quota.Exceeded = true")
	}
	if auth.Quota.Reason != "usage_limit" {
		t.Errorf("expected reason=usage_limit, got %q", auth.Quota.Reason)
	}
	if auth.Quota.BackoffLevel != 3 {
		t.Errorf("expected backoff_level=3, got %d", auth.Quota.BackoffLevel)
	}
	if auth.Quota.NextRecoverAt.IsZero() {
		t.Error("expected NextRecoverAt to be set")
	}
	if !auth.Unavailable {
		t.Error("expected auth.Unavailable = true")
	}
}

func TestReadAuthFile_IgnoresExpiredQuotaState(t *testing.T) {
	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)

	pastTime := time.Now().Add(-1 * time.Hour).UTC().Truncate(time.Second)
	authData := map[string]any{
		"type":  "codex",
		"token": "test-token",
		"quota_state": map[string]any{
			"exceeded":        true,
			"reason":          "usage_limit",
			"next_recover_at": pastTime.Format(time.RFC3339),
			"backoff_level":   float64(2),
		},
	}
	raw, _ := json.Marshal(authData)
	path := filepath.Join(dir, "test-expired.json")
	os.WriteFile(path, raw, 0o600)

	auth, err := store.readAuthFile(path, dir)
	if err != nil {
		t.Fatalf("readAuthFile error: %v", err)
	}
	if auth.Quota.Exceeded {
		t.Error("expected Quota.Exceeded = false for expired quota")
	}
}

func TestFileTokenStore_PersistQuotaState(t *testing.T) {
	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)

	// Write initial auth file
	authData := map[string]any{
		"type":  "codex",
		"token": "test-token",
		"email": "test@example.com",
	}
	raw, _ := json.Marshal(authData)
	path := filepath.Join(dir, "test-persist.json")
	os.WriteFile(path, raw, 0o600)

	// Read to get the auth ID
	auth, err := store.readAuthFile(path, dir)
	if err != nil {
		t.Fatalf("readAuthFile error: %v", err)
	}

	quota := cliproxyauth.QuotaState{
		Exceeded:      true,
		Reason:        "usage_limit",
		NextRecoverAt: time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second),
		BackoffLevel:  3,
		UpdatedAt:     time.Now().UTC().Truncate(time.Second),
	}

	err = store.PersistQuotaState(auth.ID, quota)
	if err != nil {
		t.Fatalf("PersistQuotaState error: %v", err)
	}

	// Re-read and verify quota was persisted
	reloaded, err := store.readAuthFile(path, dir)
	if err != nil {
		t.Fatalf("readAuthFile after persist: %v", err)
	}
	if !reloaded.Quota.Exceeded {
		t.Error("expected Exceeded = true after persist")
	}
	if reloaded.Quota.Reason != "usage_limit" {
		t.Errorf("expected reason=usage_limit, got %q", reloaded.Quota.Reason)
	}

	// Verify original fields preserved
	data2, _ := os.ReadFile(path)
	var check map[string]any
	json.Unmarshal(data2, &check)
	if check["token"] != "test-token" {
		t.Error("expected original token to be preserved")
	}
	if check["email"] != "test@example.com" {
		t.Error("expected original email to be preserved")
	}
}

func TestFileTokenStore_SavePersistsRuntimeQuotaState(t *testing.T) {
	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)

	recoverAt := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	updatedAt := time.Now().UTC().Truncate(time.Second)
	auth := &cliproxyauth.Auth{
		ID:       "codex-quota.json",
		FileName: "codex-quota.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": "token",
			"email":        "quota@example.com",
		},
		Quota: cliproxyauth.QuotaState{
			Exceeded:      true,
			Reason:        "usage_limit",
			NextRecoverAt: recoverAt,
			BackoffLevel:  2,
			UpdatedAt:     updatedAt,
		},
	}

	if _, err := store.Save(t.Context(), auth); err != nil {
		t.Fatalf("Save error: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "codex-quota.json"))
	if err != nil {
		t.Fatalf("read saved auth: %v", err)
	}
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("unmarshal saved auth: %v", err)
	}
	quotaRaw, ok := saved["quota_state"].(map[string]any)
	if !ok {
		t.Fatalf("quota_state missing from saved auth: %s", string(raw))
	}
	if quotaRaw["reason"] != "usage_limit" {
		t.Fatalf("quota_state.reason = %v, want usage_limit", quotaRaw["reason"])
	}
	if quotaRaw["next_recover_at"] != recoverAt.Format(time.RFC3339) {
		t.Fatalf("quota_state.next_recover_at = %v, want %s", quotaRaw["next_recover_at"], recoverAt.Format(time.RFC3339))
	}
}
