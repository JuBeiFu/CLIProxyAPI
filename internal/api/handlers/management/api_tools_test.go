package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/gin-gonic/gin"
	codexauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// unwrapTransport extracts the underlying *http.Transport from a potentially
// wrapped RoundTripper (e.g. metadataRoundTripper from proxystats, or
// proxyPoolTransport from util). It recursively unwraps through "base" fields
// and proxyPoolTransport entries.
func unwrapTransport(t *testing.T, rt http.RoundTripper) *http.Transport {
	t.Helper()
	for depth := 0; depth < 5; depth++ {
		if transport, ok := rt.(*http.Transport); ok {
			return transport
		}
		value := reflect.ValueOf(rt)
		if value.Kind() != reflect.Ptr || value.IsNil() {
			t.Fatalf("unexpected round tripper type at depth %d: %T", depth, rt)
		}
		elem := value.Elem()
		// Try "base" field first (e.g. metadataRoundTripper)
		if field := elem.FieldByName("base"); field.IsValid() && field.CanAddr() {
			baseValue := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
			if base, ok := baseValue.Interface().(http.RoundTripper); ok {
				rt = base
				continue
			}
		}
		// Try "entries" field for proxyPoolTransport
		if entries := elem.FieldByName("entries"); entries.IsValid() && entries.Len() > 0 {
			entry := entries.Index(0)
			if transportField := entry.FieldByName("transport"); transportField.IsValid() {
				transportValue := reflect.NewAt(transportField.Type(), unsafe.Pointer(transportField.UnsafeAddr())).Elem()
				if inner, ok := transportValue.Interface().(*http.Transport); ok {
					return inner
				}
			}
		}
		t.Fatalf("cannot unwrap round tripper at depth %d: %T", depth, rt)
	}
	t.Fatalf("exceeded max unwrap depth for round tripper: %T", rt)
	return nil
}

func TestAPICallTransportDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}

	transport := h.apiCallTransport(&coreauth.Auth{ProxyURL: "direct"})
	httpTransport := unwrapTransport(t, transport)
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
	httpTransport := unwrapTransport(t, transport)

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
			httpTransport := unwrapTransport(t, transport)

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

func TestResolveTokenForAuth_Codex_RefreshesWhenStoredTokenMissing(t *testing.T) {
	originalRefresh := refreshCodexTokensWithRetry
	refreshCodexTokensWithRetry = func(ctx context.Context, h *Handler, refreshToken string, maxRetries int) (*codexauth.CodexTokenData, error) {
		_ = ctx
		_ = h
		if refreshToken != "rt" {
			t.Fatalf("unexpected refresh token: %s", refreshToken)
		}
		if maxRetries != 3 {
			t.Fatalf("unexpected max retries: %d", maxRetries)
		}
		return &codexauth.CodexTokenData{
			AccessToken:  "new-token",
			RefreshToken: "rt2",
			AccountID:    "acct-1",
			Email:        "user@example.com",
			Expire:       time.Now().Add(1 * time.Hour).Format(time.RFC3339),
		}, nil
	}
	t.Cleanup(func() { refreshCodexTokensWithRetry = originalRefresh })

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)

	auth := &coreauth.Auth{
		ID:       "codex-test.json",
		FileName: "codex-test.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":          "codex",
			"refresh_token": "rt",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{authManager: manager}
	token, err := h.resolveTokenForAuth(context.Background(), auth)
	if err != nil {
		t.Fatalf("resolveTokenForAuth: %v", err)
	}
	if token != "new-token" {
		t.Fatalf("expected refreshed token, got %q", token)
	}

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth in manager after update")
	}
	if got := tokenValueFromMetadata(updated.Metadata); got != "new-token" {
		t.Fatalf("expected manager metadata updated, got %q", got)
	}
	if got := stringValue(updated.Metadata, "refresh_token"); got != "rt2" {
		t.Fatalf("expected refresh token updated, got %q", got)
	}
	if got := stringValue(updated.Metadata, "account_id"); got != "acct-1" {
		t.Fatalf("expected account_id updated, got %q", got)
	}
	if got := stringValue(updated.Metadata, "email"); got != "user@example.com" {
		t.Fatalf("expected email updated, got %q", got)
	}
}

func TestAPICall_Codex401RefreshesAndRetries(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalRefresh := refreshCodexTokensWithRetry
	refreshCalls := 0
	refreshCodexTokensWithRetry = func(ctx context.Context, h *Handler, refreshToken string, maxRetries int) (*codexauth.CodexTokenData, error) {
		_ = ctx
		_ = h
		refreshCalls++
		if refreshToken != "rt" {
			t.Fatalf("unexpected refresh token: %s", refreshToken)
		}
		if maxRetries != 3 {
			t.Fatalf("unexpected max retries: %d", maxRetries)
		}
		return &codexauth.CodexTokenData{
			AccessToken:  "fresh-token",
			RefreshToken: "rt2",
			AccountID:    "acct-1",
			Email:        "user@example.com",
			Expire:       time.Now().Add(1 * time.Hour).Format(time.RFC3339),
		}, nil
	}
	t.Cleanup(func() { refreshCodexTokensWithRetry = originalRefresh })

	var authHeaders []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeaders = append(authHeaders, strings.TrimSpace(r.Header.Get("Authorization")))
		switch strings.TrimSpace(r.Header.Get("Authorization")) {
		case "Bearer old-token":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Unauthorized"}}`))
		case "Bearer fresh-token":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(target.Close)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-retry.json",
		FileName: "codex-retry.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "old-token",
			"refresh_token": "rt",
			"account_id":    "acct-old",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	stored, ok := manager.GetByID(auth.ID)
	if !ok || stored == nil {
		t.Fatalf("expected stored auth")
	}
	authIndex := stored.EnsureIndex()

	h := &Handler{authManager: manager}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	requestBody := `{"authIndex":"` + authIndex + `","method":"GET","url":"` + target.URL + `","header":{"Authorization":"Bearer $TOKEN$"}}`
	request := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	ctx.Request = request

	h.APICall(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var payload apiCallResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.StatusCode != http.StatusOK {
		t.Fatalf("expected upstream status 200 after retry, got %d", payload.StatusCode)
	}
	if !payload.AuthRefreshAttempted {
		t.Fatalf("expected auth_refresh_attempted to be true")
	}
	if !payload.AuthRefreshSucceeded {
		t.Fatalf("expected auth_refresh_succeeded to be true")
	}
	if payload.AuthRefreshError != "" {
		t.Fatalf("unexpected auth_refresh_error: %s", payload.AuthRefreshError)
	}
	if payload.Body != `{"ok":true}` {
		t.Fatalf("unexpected upstream body: %s", payload.Body)
	}
	if refreshCalls != 1 {
		t.Fatalf("expected 1 refresh call, got %d", refreshCalls)
	}
	if len(authHeaders) != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", len(authHeaders))
	}
	if authHeaders[0] != "Bearer old-token" || authHeaders[1] != "Bearer fresh-token" {
		t.Fatalf("unexpected auth headers: %v", authHeaders)
	}

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected updated auth in manager")
	}
	if got := tokenValueFromMetadata(updated.Metadata); got != "fresh-token" {
		t.Fatalf("expected refreshed token persisted, got %q", got)
	}
	if got := stringValue(updated.Metadata, "refresh_token"); got != "rt2" {
		t.Fatalf("expected refresh token persisted, got %q", got)
	}
}

func TestAPICall_Codex401DeactivatedDeletesAuthAndSkipsRefresh(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalRefresh := refreshCodexTokensWithRetry
	refreshCalls := 0
	refreshCodexTokensWithRetry = func(ctx context.Context, h *Handler, refreshToken string, maxRetries int) (*codexauth.CodexTokenData, error) {
		_ = ctx
		_ = h
		refreshCalls++
		return nil, fmt.Errorf("unexpected refresh call")
	}
	t.Cleanup(func() { refreshCodexTokensWithRetry = originalRefresh })

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Your OpenAI account has been deactivated, please check your email for more information.","type":"invalid_request_error","code":"account_deactivated","param":null},"status":401}`))
	}))
	t.Cleanup(target.Close)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-deactivated.json",
		FileName: "codex-deactivated.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "old-token",
			"refresh_token": "rt",
			"account_id":    "acct-old",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	stored, ok := manager.GetByID(auth.ID)
	if !ok || stored == nil {
		t.Fatalf("expected stored auth")
	}
	authIndex := stored.EnsureIndex()

	h := &Handler{authManager: manager}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	requestBody := `{"authIndex":"` + authIndex + `","method":"GET","url":"` + target.URL + `","header":{"Authorization":"Bearer $TOKEN$"}}`
	request := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	ctx.Request = request

	h.APICall(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var payload apiCallResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected upstream status 401, got %d", payload.StatusCode)
	}
	if !payload.AuthRevokedDeleted {
		t.Fatalf("expected auth_revoked_deleted to be true")
	}
	if refreshCalls != 0 {
		t.Fatalf("expected 0 refresh calls, got %d", refreshCalls)
	}
	if _, ok := manager.GetByID(auth.ID); ok {
		t.Fatalf("expected auth deleted from manager after deactivation response")
	}
}

func TestAPICall_CodexDeactivatedWorkspaceDeletesAuthAndSkipsRefresh(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalRefresh := refreshCodexTokensWithRetry
	refreshCalls := 0
	refreshCodexTokensWithRetry = func(ctx context.Context, h *Handler, refreshToken string, maxRetries int) (*codexauth.CodexTokenData, error) {
		_ = ctx
		_ = h
		refreshCalls++
		return nil, fmt.Errorf("unexpected refresh call")
	}
	t.Cleanup(func() { refreshCodexTokensWithRetry = originalRefresh })

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"detail":{"code":"deactivated_workspace"}}`))
	}))
	t.Cleanup(target.Close)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-deactivated-workspace.json",
		FileName: "codex-deactivated-workspace.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "old-token",
			"refresh_token": "rt",
			"account_id":    "acct-old",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	stored, ok := manager.GetByID(auth.ID)
	if !ok || stored == nil {
		t.Fatalf("expected stored auth")
	}
	authIndex := stored.EnsureIndex()

	h := &Handler{authManager: manager}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	requestBody := `{"authIndex":"` + authIndex + `","method":"GET","url":"` + target.URL + `","header":{"Authorization":"Bearer $TOKEN$"}}`
	request := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	ctx.Request = request

	h.APICall(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var payload apiCallResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.StatusCode != http.StatusForbidden {
		t.Fatalf("expected upstream status 403, got %d", payload.StatusCode)
	}
	if !payload.AuthRevokedDeleted {
		t.Fatalf("expected auth_revoked_deleted to be true")
	}
	if refreshCalls != 0 {
		t.Fatalf("expected 0 refresh calls, got %d", refreshCalls)
	}
	if _, ok := manager.GetByID(auth.ID); ok {
		t.Fatalf("expected auth deleted from manager after deactivated_workspace response")
	}
}
