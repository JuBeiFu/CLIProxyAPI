package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	codexauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type memoryAuthStore struct {
	mu    sync.Mutex
	items map[string]*coreauth.Auth
}

func (s *memoryAuthStore) List(ctx context.Context) ([]*coreauth.Auth, error) {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*coreauth.Auth, 0, len(s.items))
	for _, a := range s.items {
		out = append(out, a.Clone())
	}
	return out, nil
}

func (s *memoryAuthStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	_ = ctx
	if auth == nil {
		return "", nil
	}
	s.mu.Lock()
	if s.items == nil {
		s.items = make(map[string]*coreauth.Auth)
	}
	s.items[auth.ID] = auth.Clone()
	s.mu.Unlock()
	return auth.ID, nil
}

func (s *memoryAuthStore) Delete(ctx context.Context, id string) error {
	_ = ctx
	s.mu.Lock()
	delete(s.items, id)
	s.mu.Unlock()
	return nil
}

func TestResolveTokenForAuth_Antigravity_RefreshesExpiredToken(t *testing.T) {
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			t.Fatalf("unexpected content-type: %s", ct)
		}
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		values, err := url.ParseQuery(string(bodyBytes))
		if err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if values.Get("grant_type") != "refresh_token" {
			t.Fatalf("unexpected grant_type: %s", values.Get("grant_type"))
		}
		if values.Get("refresh_token") != "rt" {
			t.Fatalf("unexpected refresh_token: %s", values.Get("refresh_token"))
		}
		if values.Get("client_id") != antigravityOAuthClientID {
			t.Fatalf("unexpected client_id: %s", values.Get("client_id"))
		}
		if values.Get("client_secret") != antigravityOAuthClientSecret {
			t.Fatalf("unexpected client_secret")
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-token",
			"refresh_token": "rt2",
			"expires_in":    int64(3600),
			"token_type":    "Bearer",
		})
	}))
	t.Cleanup(srv.Close)

	originalURL := antigravityOAuthTokenURL
	antigravityOAuthTokenURL = srv.URL
	t.Cleanup(func() { antigravityOAuthTokenURL = originalURL })

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)

	auth := &coreauth.Auth{
		ID:       "antigravity-test.json",
		FileName: "antigravity-test.json",
		Provider: "antigravity",
		Metadata: map[string]any{
			"type":          "antigravity",
			"access_token":  "old-token",
			"refresh_token": "rt",
			"expires_in":    int64(3600),
			"timestamp":     time.Now().Add(-2 * time.Hour).UnixMilli(),
			"expired":       time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
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
	if callCount != 1 {
		t.Fatalf("expected 1 refresh call, got %d", callCount)
	}

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth in manager after update")
	}
	if got := tokenValueFromMetadata(updated.Metadata); got != "new-token" {
		t.Fatalf("expected manager metadata updated, got %q", got)
	}
}

func TestResolveTokenForAuth_Antigravity_SkipsRefreshWhenTokenValid(t *testing.T) {
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	originalURL := antigravityOAuthTokenURL
	antigravityOAuthTokenURL = srv.URL
	t.Cleanup(func() { antigravityOAuthTokenURL = originalURL })

	auth := &coreauth.Auth{
		ID:       "antigravity-valid.json",
		FileName: "antigravity-valid.json",
		Provider: "antigravity",
		Metadata: map[string]any{
			"type":         "antigravity",
			"access_token": "ok-token",
			"expired":      time.Now().Add(30 * time.Minute).Format(time.RFC3339),
		},
	}
	h := &Handler{}
	token, err := h.resolveTokenForAuth(context.Background(), auth)
	if err != nil {
		t.Fatalf("resolveTokenForAuth: %v", err)
	}
	if token != "ok-token" {
		t.Fatalf("expected existing token, got %q", token)
	}
	if callCount != 0 {
		t.Fatalf("expected no refresh calls, got %d", callCount)
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
