package management

import (
	"context"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestBuildAuthFileEntry_IncludesCodexQuotaCache(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	now := time.Now().UTC()
	auth := &coreauth.Auth{
		ID:       "codex-cache-auth",
		FileName: "codex-cache-auth.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":                                "codex",
			"path":                                "/tmp/codex-cache-auth.json",
			"plan_type":                           "team",
			"cliproxy_codex_usage_payload":        `{"plan_type":"team","rate_limit":{"primary_window":{"used_percent":32,"limit_window_seconds":18000,"reset_after_seconds":600}}}`,
			"cliproxy_codex_usage_last":           now.Unix(),
			"cliproxy_codex_usage_after":          now.Add(5 * time.Minute).Unix(),
			"cliproxy_codex_usage_remaining_percent": 68.0,
			"cliproxy_auto_disabled_reason":       "quota_low_balance",
		},
		Attributes: map[string]string{
			"path": "/tmp/codex-cache-auth.json",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{authManager: manager}
	entry := h.buildAuthFileEntry(auth)
	if entry == nil {
		t.Fatalf("expected auth file entry")
	}

	rawCache, ok := entry["quota_cache"]
	if !ok {
		t.Fatalf("expected quota_cache to be present")
	}
	cache, ok := rawCache.(gin.H)
	if !ok {
		t.Fatalf("quota_cache type = %T, want gin.H", rawCache)
	}
	if cache["provider"] != "codex" {
		t.Fatalf("quota_cache.provider = %#v, want %q", cache["provider"], "codex")
	}
	if cache["remaining_percent"] != 68.0 {
		t.Fatalf("quota_cache.remaining_percent = %#v, want %v", cache["remaining_percent"], 68.0)
	}
	if cache["plan_type"] != "team" {
		t.Fatalf("quota_cache.plan_type = %#v, want %q", cache["plan_type"], "team")
	}
	if cache["auto_disabled"] != true {
		t.Fatalf("quota_cache.auto_disabled = %#v, want true", cache["auto_disabled"])
	}
	payload, ok := cache["payload"].(map[string]interface{})
	if !ok {
		if payloadGin, okGin := cache["payload"].(gin.H); okGin {
			payload = map[string]interface{}(payloadGin)
			ok = true
		}
	}
	if !ok {
		t.Fatalf("quota_cache.payload type = %T, want map[string]interface{}", cache["payload"])
	}
	if payload["plan_type"] != "team" {
		t.Fatalf("quota_cache.payload.plan_type = %#v, want %q", payload["plan_type"], "team")
	}
}
