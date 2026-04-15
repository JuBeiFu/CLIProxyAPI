package management

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestAPICallTransportDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}

	transport := h.apiCallTransport(&coreauth.Auth{ProxyURL: "direct"})
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", transport)
	}
	if httpTransport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestAPICallTransportInvalidAuthFallsBackToGlobalProxy(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}

	transport := h.apiCallTransport(&coreauth.Auth{ProxyURL: "bad-value"})
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", transport)
	}

	req, errRequest := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if errRequest != nil {
		t.Fatalf("http.NewRequest returned error: %v", errRequest)
	}

	proxyURL, errProxy := httpTransport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("httpTransport.Proxy returned error: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://global-proxy.example.com:8080" {
		t.Fatalf("proxy URL = %v, want http://global-proxy.example.com:8080", proxyURL)
	}
}

func TestAPICallTransportAPIKeyAuthFallsBackToConfigProxyURL(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
			GeminiKey: []config.GeminiKey{{
				APIKey:   "gemini-key",
				ProxyURL: "http://gemini-proxy.example.com:8080",
			}},
			ClaudeKey: []config.ClaudeKey{{
				APIKey:   "claude-key",
				ProxyURL: "http://claude-proxy.example.com:8080",
			}},
			CodexKey: []config.CodexKey{{
				APIKey:   "codex-key",
				ProxyURL: "http://codex-proxy.example.com:8080",
			}},
			OpenAICompatibility: []config.OpenAICompatibility{{
				Name:    "bohe",
				BaseURL: "https://bohe.example.com",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{
					APIKey:   "compat-key",
					ProxyURL: "http://compat-proxy.example.com:8080",
				}},
			}},
		},
	}

	cases := []struct {
		name      string
		auth      *coreauth.Auth
		wantProxy string
	}{
		{
			name: "gemini",
			auth: &coreauth.Auth{
				Provider:   "gemini",
				Attributes: map[string]string{"api_key": "gemini-key"},
			},
			wantProxy: "http://gemini-proxy.example.com:8080",
		},
		{
			name: "claude",
			auth: &coreauth.Auth{
				Provider:   "claude",
				Attributes: map[string]string{"api_key": "claude-key"},
			},
			wantProxy: "http://claude-proxy.example.com:8080",
		},
		{
			name: "codex",
			auth: &coreauth.Auth{
				Provider:   "codex",
				Attributes: map[string]string{"api_key": "codex-key"},
			},
			wantProxy: "http://codex-proxy.example.com:8080",
		},
		{
			name: "openai-compatibility",
			auth: &coreauth.Auth{
				Provider: "bohe",
				Attributes: map[string]string{
					"api_key":      "compat-key",
					"compat_name":  "bohe",
					"provider_key": "bohe",
				},
			},
			wantProxy: "http://compat-proxy.example.com:8080",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			transport := h.apiCallTransport(tc.auth)
			httpTransport, ok := transport.(*http.Transport)
			if !ok {
				t.Fatalf("transport type = %T, want *http.Transport", transport)
			}

			req, errRequest := http.NewRequest(http.MethodGet, "https://example.com", nil)
			if errRequest != nil {
				t.Fatalf("http.NewRequest returned error: %v", errRequest)
			}

			proxyURL, errProxy := httpTransport.Proxy(req)
			if errProxy != nil {
				t.Fatalf("httpTransport.Proxy returned error: %v", errProxy)
			}
			if proxyURL == nil || proxyURL.String() != tc.wantProxy {
				t.Fatalf("proxy URL = %v, want %s", proxyURL, tc.wantProxy)
			}
		})
	}
}

func TestAuthByIndexDistinguishesSharedAPIKeysAcrossProviders(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	geminiAuth := &coreauth.Auth{
		ID:       "gemini:apikey:123",
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key": "shared-key",
		},
	}
	compatAuth := &coreauth.Auth{
		ID:       "openai-compatibility:bohe:456",
		Provider: "bohe",
		Label:    "bohe",
		Attributes: map[string]string{
			"api_key":      "shared-key",
			"compat_name":  "bohe",
			"provider_key": "bohe",
		},
	}

	if _, errRegister := manager.Register(context.Background(), geminiAuth); errRegister != nil {
		t.Fatalf("register gemini auth: %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), compatAuth); errRegister != nil {
		t.Fatalf("register compat auth: %v", errRegister)
	}

	geminiIndex := geminiAuth.EnsureIndex()
	compatIndex := compatAuth.EnsureIndex()
	if geminiIndex == compatIndex {
		t.Fatalf("shared api key produced duplicate auth_index %q", geminiIndex)
	}

	h := &Handler{authManager: manager}

	gotGemini := h.authByIndex(geminiIndex)
	if gotGemini == nil {
		t.Fatal("expected gemini auth by index")
	}
	if gotGemini.ID != geminiAuth.ID {
		t.Fatalf("authByIndex(gemini) returned %q, want %q", gotGemini.ID, geminiAuth.ID)
	}

	gotCompat := h.authByIndex(compatIndex)
	if gotCompat == nil {
		t.Fatal("expected compat auth by index")
	}
	if gotCompat.ID != compatAuth.ID {
		t.Fatalf("authByIndex(compat) returned %q, want %q", gotCompat.ID, compatAuth.ID)
	}
}

func TestAPICallMarksQuotaExceededCooldown(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-test@example.com-plus.json",
		Provider: "codex",
		FileName: "codex-test@example.com-plus.json",
		Attributes: map[string]string{
			"path": "C:\\auths\\codex-test@example.com-plus.json",
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	h := &Handler{authManager: manager}
	resetAt := time.Now().Add(2 * time.Minute).Unix()
	body := []byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_at":` + strconv.FormatInt(resetAt, 10) + `,"resets_in_seconds":120}}`)

	h.markAPICallResponse(context.Background(), auth, http.StatusTooManyRequests, body)

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("expected auth to remain registered")
	}
	if !updated.Unavailable {
		t.Fatal("expected auth to be unavailable after quota response")
	}
	if updated.Status != coreauth.StatusError {
		t.Fatalf("status = %q, want %q", updated.Status, coreauth.StatusError)
	}
	if updated.StatusMessage != "quota exhausted" {
		t.Fatalf("status_message = %q, want %q", updated.StatusMessage, "quota exhausted")
	}
	if updated.NextRetryAfter.IsZero() {
		t.Fatal("expected next_retry_after to be set")
	}
	if !updated.Quota.Exceeded {
		t.Fatal("expected quota exceeded flag")
	}
}

func TestAPICallDeletesRevokedCodexAuthOn401(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-revoked@example.com-plus.json",
		Provider: "codex",
		FileName: "codex-revoked@example.com-plus.json",
		Attributes: map[string]string{
			"path":  "C:\\auths\\codex-revoked@example.com-plus.json",
			"email": "codex-revoked@example.com",
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Your authentication token has been invalidated. Please try signing in again."}}`))
	}))
	defer upstream.Close()

	h := &Handler{authManager: manager}
	bodyBytes, errMarshal := json.Marshal(apiCallRequest{
		AuthIndexSnake: strPtr(auth.EnsureIndex()),
		Method:         http.MethodGet,
		URL:            upstream.URL,
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(string(bodyBytes)))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.APICall(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if _, ok := manager.GetByID(auth.ID); ok {
		t.Fatal("expected revoked auth to be deleted from manager")
	}
}

func TestAPICallDeletesRevokedCodexAuthDisappearsFromList(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-revoked-list@example.com-plus.json",
		Provider: "codex",
		FileName: "codex-revoked-list@example.com-plus.json",
		Attributes: map[string]string{
			"path":  `C:\auths\codex-revoked-list@example.com-plus.json`,
			"email": "codex-revoked-list@example.com",
		},
		Metadata: map[string]any{
			"type":     "codex",
			"email":    "codex-revoked-list@example.com",
			"id_token": testCodexJWTForManagement(t, "plus", "acct_revoked_list"),
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Encountered invalidated oauth token for user, failing request","code":"token_revoked"}}`))
	}))
	defer upstream.Close()

	h := &Handler{authManager: manager}
	bodyBytes, errMarshal := json.Marshal(apiCallRequest{
		AuthIndexSnake: strPtr(auth.EnsureIndex()),
		Method:         http.MethodGet,
		URL:            upstream.URL,
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(string(bodyBytes)))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.APICall(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	listRecorder := httptest.NewRecorder()
	listCtx, _ := gin.CreateTestContext(listRecorder)
	listCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	h.ListAuthFiles(listCtx)

	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status = %d, want %d", listRecorder.Code, http.StatusOK)
	}
	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode list payload: %v", err)
	}
	for _, item := range payload.Files {
		if name, _ := item["name"].(string); name == auth.FileName {
			t.Fatalf("expected revoked auth %q to be absent from auth list", auth.FileName)
		}
	}
}

func TestBuildAuthFromFileData_ParsesQuotaState(t *testing.T) {
	dir := t.TempDir()
	h := &Handler{}
	// Set cfg with AuthDir so authIDForPath works
	h.cfg = &config.Config{AuthDir: dir}

	futureTime := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	data, _ := json.Marshal(map[string]any{
		"type":  "codex",
		"token": "test-token",
		"quota_state": map[string]any{
			"exceeded":        true,
			"reason":          "usage_limit",
			"next_recover_at": futureTime.Format(time.RFC3339),
			"backoff_level":   float64(3),
		},
	})

	path := filepath.Join(dir, "test-quota.json")
	os.WriteFile(path, data, 0o600)

	auth, err := h.buildAuthFromFileData(path, data)
	if err != nil {
		t.Fatalf("buildAuthFromFileData error: %v", err)
	}
	if !auth.Quota.Exceeded {
		t.Error("expected Quota.Exceeded = true")
	}
	if auth.Quota.Reason != "usage_limit" {
		t.Errorf("expected reason=usage_limit, got %q", auth.Quota.Reason)
	}
	if !auth.Unavailable {
		t.Error("expected Unavailable = true")
	}
}

func strPtr(value string) *string {
	return &value
}

func testCodexJWTForManagement(t *testing.T, planType, accountID string) string {
	t.Helper()

	headerBytes, err := json.Marshal(map[string]any{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	payloadBytes, err := json.Marshal(map[string]any{
		"email": "codex-test@example.com",
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

func TestBuildAuthFileEntry_ExposesQuota(t *testing.T) {
	h := &Handler{}
	now := time.Now()
	auth := &coreauth.Auth{
		ID:         "auth-entry",
		Provider:   "codex",
		FileName:   "test.json",
		Label:      "test",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"path": "/tmp/test.json"},
		Quota: coreauth.QuotaState{
			Exceeded:      true,
			Reason:        "usage_limit",
			NextRecoverAt: now.Add(2 * time.Hour),
			BackoffLevel:  3,
		},
		ModelStates: map[string]*coreauth.ModelState{
			"gpt-4":  {Unavailable: false},
			"o1":     {Unavailable: true, NextRetryAfter: now.Add(1 * time.Hour), Quota: coreauth.QuotaState{Exceeded: true}},
			"gpt-4o": {Status: coreauth.StatusDisabled},
		},
	}

	entry := h.buildAuthFileEntry(auth)
	if entry == nil {
		t.Fatal("expected non-nil entry")
	}

	// Check quota field exists
	quota, ok := entry["quota"]
	if !ok {
		t.Fatal("expected 'quota' field in entry")
	}
	quotaMap, ok := quota.(gin.H)
	if !ok {
		t.Fatalf("expected quota to be gin.H, got %T", quota)
	}
	if quotaMap["exceeded"] != true {
		t.Error("expected quota.exceeded = true")
	}
	if quotaMap["reason"] != "usage_limit" {
		t.Errorf("expected quota.reason = usage_limit, got %v", quotaMap["reason"])
	}

	// Check model_states_summary
	summary, ok := entry["model_states_summary"]
	if !ok {
		t.Fatal("expected 'model_states_summary' field")
	}
	summaryMap, ok := summary.(gin.H)
	if !ok {
		t.Fatalf("expected summary to be gin.H, got %T", summary)
	}
	if summaryMap["available"] != 1 {
		t.Errorf("expected 1 available, got %v", summaryMap["available"])
	}
	if summaryMap["cooldown"] != 1 {
		t.Errorf("expected 1 cooldown, got %v", summaryMap["cooldown"])
	}
	if summaryMap["disabled"] != 1 {
		t.Errorf("expected 1 disabled, got %v", summaryMap["disabled"])
	}
}
