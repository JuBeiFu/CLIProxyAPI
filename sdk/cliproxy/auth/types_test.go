package auth

import (
	"testing"
	"time"
)

func TestResolveCreatedAt(t *testing.T) {
	fallback := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	persisted := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	md := map[string]any{"created_at": persisted.Format(time.RFC3339)}
	if got := ResolveCreatedAt(md, fallback); !got.Equal(persisted) {
		t.Fatalf("persisted: got %s want %s", got, persisted)
	}
	if got := ResolveCreatedAt(map[string]any{}, fallback); !got.Equal(fallback) {
		t.Fatalf("absent: got %s want %s", got, fallback)
	}
	if got := ResolveCreatedAt(map[string]any{"created_at": "not-a-time"}, fallback); !got.Equal(fallback) {
		t.Fatalf("bad: got %s want %s", got, fallback)
	}
	if got := ResolveCreatedAt(nil, fallback); !got.Equal(fallback) {
		t.Fatalf("nil: got %s want %s", got, fallback)
	}
}

func TestStampCreatedAt(t *testing.T) {
	md := map[string]any{}
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	StampCreatedAt(md, ts)
	if md["created_at"] != ts.UTC().Format(time.RFC3339) {
		t.Fatalf("stamp: got %v", md["created_at"])
	}
	md2 := map[string]any{}
	StampCreatedAt(md2, time.Time{})
	if _, ok := md2["created_at"]; ok {
		t.Fatalf("zero time must not write created_at")
	}
	StampCreatedAt(nil, ts) // must not panic
}

func TestToolPrefixDisabled(t *testing.T) {
	var a *Auth
	if a.ToolPrefixDisabled() {
		t.Error("nil auth should return false")
	}

	a = &Auth{}
	if a.ToolPrefixDisabled() {
		t.Error("empty auth should return false")
	}

	a = &Auth{Metadata: map[string]any{"tool_prefix_disabled": true}}
	if !a.ToolPrefixDisabled() {
		t.Error("should return true when set to true")
	}

	a = &Auth{Metadata: map[string]any{"tool_prefix_disabled": "true"}}
	if !a.ToolPrefixDisabled() {
		t.Error("should return true when set to string 'true'")
	}

	a = &Auth{Metadata: map[string]any{"tool-prefix-disabled": true}}
	if !a.ToolPrefixDisabled() {
		t.Error("should return true with kebab-case key")
	}

	a = &Auth{Metadata: map[string]any{"tool_prefix_disabled": false}}
	if a.ToolPrefixDisabled() {
		t.Error("should return false when set to false")
	}
}

func TestEnsureIndexUsesCredentialIdentity(t *testing.T) {
	t.Parallel()

	geminiAuth := &Auth{
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key": "shared-key",
			"source":  "config:gemini[abc123]",
		},
	}
	compatAuth := &Auth{
		Provider: "bohe",
		Attributes: map[string]string{
			"api_key":      "shared-key",
			"compat_name":  "bohe",
			"provider_key": "bohe",
			"source":       "config:bohe[def456]",
		},
	}
	geminiAltBase := &Auth{
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key":  "shared-key",
			"base_url": "https://alt.example.com",
			"source":   "config:gemini[ghi789]",
		},
	}
	geminiDuplicate := &Auth{
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key": "shared-key",
			"source":  "config:gemini[abc123-1]",
		},
	}

	geminiIndex := geminiAuth.EnsureIndex()
	compatIndex := compatAuth.EnsureIndex()
	altBaseIndex := geminiAltBase.EnsureIndex()
	duplicateIndex := geminiDuplicate.EnsureIndex()

	if geminiIndex == "" {
		t.Fatal("gemini index should not be empty")
	}
	if compatIndex == "" {
		t.Fatal("compat index should not be empty")
	}
	if altBaseIndex == "" {
		t.Fatal("alt base index should not be empty")
	}
	if duplicateIndex == "" {
		t.Fatal("duplicate index should not be empty")
	}
	if geminiIndex == compatIndex {
		t.Fatalf("shared api key produced duplicate auth_index %q", geminiIndex)
	}
	if geminiIndex == altBaseIndex {
		t.Fatalf("same provider/key with different base_url produced duplicate auth_index %q", geminiIndex)
	}
	if geminiIndex == duplicateIndex {
		t.Fatalf("duplicate config entries should be separated by source-derived seed, got %q", geminiIndex)
	}
}

func TestWebsocketsEnabledDefaultsToTrueForCodexOAuth(t *testing.T) {
	t.Parallel()

	auth := &Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email": "user@example.com",
		},
	}

	if !auth.WebsocketsEnabled() {
		t.Fatal("expected codex oauth auth to default to websocket enabled")
	}
}

func TestWebsocketsEnabledRespectsExplicitDisableForCodexOAuth(t *testing.T) {
	t.Parallel()

	auth := &Auth{
		Provider:   "codex",
		Attributes: map[string]string{"websockets": "false"},
		Metadata: map[string]any{
			"email": "user@example.com",
		},
	}

	if auth.WebsocketsEnabled() {
		t.Fatal("expected explicit websockets=false to disable codex oauth websocket support")
	}
}
