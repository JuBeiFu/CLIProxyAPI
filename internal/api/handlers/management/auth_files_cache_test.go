package management

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type codexQuotaRefreshHTTPExecutor struct{}

func (codexQuotaRefreshHTTPExecutor) Identifier() string { return "codex" }

func (codexQuotaRefreshHTTPExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (codexQuotaRefreshHTTPExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (codexQuotaRefreshHTTPExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (codexQuotaRefreshHTTPExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (codexQuotaRefreshHTTPExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body: ioNopCloser(`{
			"plan_type":"team",
			"rate_limit":{
				"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":600},
				"secondary_window":{"used_percent":70,"limit_window_seconds":604800,"reset_after_seconds":3600}
			}
		}`),
	}, nil
}

func ioNopCloser(body string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(body))
}

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

func TestRefreshAuthFileQuota_ReturnsUpdatedEntry(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	manager.RegisterExecutor(codexQuotaRefreshHTTPExecutor{})
	auth := &coreauth.Auth{
		ID:       "codex-refresh-auth",
		FileName: "codex-refresh-auth.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":       "codex",
			"account_id": "acct-refresh-auth",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{authManager: manager}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/quota-refresh", strings.NewReader(`{"name":"codex-refresh-auth.json"}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.RefreshAuthFileQuota(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"quota_cache"`) {
		t.Fatalf("expected response body to include quota_cache, got %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"disabled":true`) {
		t.Fatalf("expected refreshed auth file to be disabled in response, got %s", rec.Body.String())
	}
}
