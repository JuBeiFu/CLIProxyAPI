package executor

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestParseCodexRetryAfter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	t.Run("resets_in_seconds", func(t *testing.T) {
		body := []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":123}}`)
		retryAfter := parseCodexRetryAfter(http.StatusTooManyRequests, body, now)
		if retryAfter == nil {
			t.Fatalf("expected retryAfter, got nil")
		}
		if *retryAfter != 123*time.Second {
			t.Fatalf("retryAfter = %v, want %v", *retryAfter, 123*time.Second)
		}
	})

	t.Run("prefers resets_at", func(t *testing.T) {
		resetAt := now.Add(5 * time.Minute).Unix()
		body := []byte(`{"error":{"type":"usage_limit_reached","resets_at":` + itoa(resetAt) + `,"resets_in_seconds":1}}`)
		retryAfter := parseCodexRetryAfter(http.StatusTooManyRequests, body, now)
		if retryAfter == nil {
			t.Fatalf("expected retryAfter, got nil")
		}
		if *retryAfter != 5*time.Minute {
			t.Fatalf("retryAfter = %v, want %v", *retryAfter, 5*time.Minute)
		}
	})

	t.Run("fallback when resets_at is past", func(t *testing.T) {
		resetAt := now.Add(-1 * time.Minute).Unix()
		body := []byte(`{"error":{"type":"usage_limit_reached","resets_at":` + itoa(resetAt) + `,"resets_in_seconds":77}}`)
		retryAfter := parseCodexRetryAfter(http.StatusTooManyRequests, body, now)
		if retryAfter == nil {
			t.Fatalf("expected retryAfter, got nil")
		}
		if *retryAfter != 77*time.Second {
			t.Fatalf("retryAfter = %v, want %v", *retryAfter, 77*time.Second)
		}
	})

	t.Run("non-429 status code", func(t *testing.T) {
		body := []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":30}}`)
		if got := parseCodexRetryAfter(http.StatusBadRequest, body, now); got != nil {
			t.Fatalf("expected nil for non-429, got %v", *got)
		}
	})

	t.Run("non usage_limit_reached error type", func(t *testing.T) {
		body := []byte(`{"error":{"type":"server_error","resets_in_seconds":30}}`)
		if got := parseCodexRetryAfter(http.StatusTooManyRequests, body, now); got != nil {
			t.Fatalf("expected nil for non-usage_limit_reached, got %v", *got)
		}
	})

	t.Run("plain rate limit exceeded detail defaults to 60 seconds", func(t *testing.T) {
		body := []byte(`{"detail":"Rate limit exceeded"}`)
		retryAfter := parseCodexRetryAfter(http.StatusTooManyRequests, body, now)
		if retryAfter == nil {
			t.Fatalf("expected retryAfter, got nil")
		}
		if *retryAfter != 60*time.Second {
			t.Fatalf("retryAfter = %v, want %v", *retryAfter, 60*time.Second)
		}
	})

	t.Run("plain too many requests message defaults to 60 seconds", func(t *testing.T) {
		body := []byte(`{"error":{"message":"Too many requests, please slow down.","type":"rate_limit_exceeded"}}`)
		retryAfter := parseCodexRetryAfter(http.StatusTooManyRequests, body, now)
		if retryAfter == nil {
			t.Fatalf("expected retryAfter, got nil")
		}
		if *retryAfter != 60*time.Second {
			t.Fatalf("retryAfter = %v, want %v", *retryAfter, 60*time.Second)
		}
	})
}

func TestNewCodexStatusErrTreatsCapacityAsRetryableRateLimit(t *testing.T) {
	body := []byte(`{"error":{"message":"Selected model is at capacity. Please try a different model."}}`)

	err := newCodexStatusErr(http.StatusBadRequest, body)

	if got := err.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", got, http.StatusTooManyRequests)
	}
	if err.RetryAfter() == nil {
		t.Fatal("expected 5m default retryAfter for capacity fallback, got nil")
	}
	if *err.RetryAfter() != 5*time.Minute {
		t.Fatalf("expected 5m default, got %v", *err.RetryAfter())
	}
}

func TestNewCodexStatusErrTreatsCurrentModelUnavailableAsRetryableRateLimit(t *testing.T) {
	body := []byte(`{"error":{"message":"The requested model is currently unavailable. Please switch model."}}`)

	err := newCodexStatusErr(http.StatusBadRequest, body)

	if got := err.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", got, http.StatusTooManyRequests)
	}
	if err.RetryAfter() == nil {
		t.Fatal("expected 5m default retryAfter for unavailable-model fallback, got nil")
	}
	if *err.RetryAfter() != 5*time.Minute {
		t.Fatalf("expected 5m default, got %v", *err.RetryAfter())
	}
}

func TestCodexResponsesEventCyberPolicyErrorBody(t *testing.T) {
	event := []byte(`{"type":"response.failed","response":{"error":{"code":"cyber_policy","message":"This chat was flagged for possible cybersecurity risk"}}}`)

	body, ok := codexResponsesEventCyberPolicyErrorBody(event)
	if !ok {
		t.Fatal("expected cyber policy event to be detected")
	}
	if got := gjson.GetBytes(body, "error.code").String(); got != "cyber_policy" {
		t.Fatalf("error code = %q, want cyber_policy", got)
	}
	if got := gjson.GetBytes(body, "error.message").String(); got == "" {
		t.Fatalf("expected error message, got empty")
	}
}

func TestParseCodexWebsocketErrorTreatsCapacityAsRetryableRateLimit(t *testing.T) {
	payload := []byte(`{"type":"error","status":400,"error":{"message":"Selected model is at capacity. Please try a different model."}}`)

	err, ok := parseCodexWebsocketError(payload)
	if !ok {
		t.Fatal("expected websocket error to be parsed")
	}

	statusCoder, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("expected status coder, got %T", err)
	}
	if got := statusCoder.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", got, http.StatusTooManyRequests)
	}
}

func TestParseCodexWebsocketErrorTreatsCurrentModelUnavailableAsRetryableRateLimit(t *testing.T) {
	payload := []byte(`{"type":"error","status":400,"error":{"message":"The requested model is currently unavailable. Please switch model."}}`)

	err, ok := parseCodexWebsocketError(payload)
	if !ok {
		t.Fatal("expected websocket error to be parsed")
	}

	statusCoder, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("expected status coder, got %T", err)
	}
	if got := statusCoder.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", got, http.StatusTooManyRequests)
	}
}

func TestParseCodexWebsocketErrorUsesRetryAfterHeader(t *testing.T) {
	payload := []byte(`{"type":"error","status":429,"error":{"message":"request throttled"},"headers":{"Retry-After":"30"}}`)

	err, ok := parseCodexWebsocketError(payload)
	if !ok {
		t.Fatal("expected websocket error to be parsed")
	}

	statusErr, ok := err.(interface {
		StatusCode() int
		RetryAfter() *time.Duration
	})
	if !ok {
		t.Fatalf("expected retryable status error, got %T", err)
	}
	if got := statusErr.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", got, http.StatusTooManyRequests)
	}
	if statusErr.RetryAfter() == nil {
		t.Fatal("expected RetryAfter, got nil")
	}
	if got := *statusErr.RetryAfter(); got != 30*time.Second {
		t.Fatalf("RetryAfter = %v, want %v", got, 30*time.Second)
	}
}

func TestNewCodexStatusErr_CapacityErrorDefaultRetryAfter(t *testing.T) {
	body := []byte(`{"error":{"message":"The selected model is at capacity"}}`)
	err := newCodexStatusErr(200, body)
	if err.code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", err.code)
	}
	if err.retryAfter == nil {
		t.Fatal("expected default retryAfter for capacity error, got nil")
	}
	if *err.retryAfter != 5*time.Minute {
		t.Errorf("expected 5m default, got %v", *err.retryAfter)
	}
}

func TestNewCodexStatusErr_UsageLimitPreservesUpstreamRetryAfter(t *testing.T) {
	body := []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":7200}}`)
	err := newCodexStatusErr(429, body)
	if err.retryAfter == nil {
		t.Fatal("expected retryAfter from upstream, got nil")
	}
	if *err.retryAfter != 2*time.Hour {
		t.Errorf("expected 2h from upstream, got %v", *err.retryAfter)
	}
}

func TestApplyCodexSupportedModelsClearsStaleModelsOnEmptyProbe(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "auth-stale-supported-models",
		Provider: "codex",
		Attributes: map[string]string{
			"supported_models":         "gpt-5.4,gpt-5.5",
			"supported_models_source":  "codex_entitlements",
			"supported_models_updated": "2026-04-24T00:00:00Z",
		},
	}

	applyCodexSupportedModels(auth, nil, time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC))

	if got := auth.Attributes["supported_models"]; got != "" {
		t.Fatalf("supported_models = %q, want empty", got)
	}
	if got := auth.Attributes["supported_models_source"]; got != "codex_entitlements" {
		t.Fatalf("supported_models_source = %q, want codex_entitlements", got)
	}
	if got := auth.Attributes["supported_models_updated"]; got != "2026-04-25T00:00:00Z" {
		t.Fatalf("supported_models_updated = %q, want 2026-04-25T00:00:00Z", got)
	}
}

func TestApplyRefreshedCodexTokenState_UpdatesMetadataAndStorage(t *testing.T) {
	storage := &codexauth.CodexTokenStorage{
		IDToken:      "old-id",
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		AccountID:    "old-account",
		LastRefresh:  "old-last-refresh",
		Email:        "old@example.com",
		Type:         "codex",
		Expire:       "old-expire",
	}
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
		Storage:  storage,
	}
	td := &codexauth.CodexTokenData{
		IDToken:      "new-id",
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
		AccountID:    "new-account",
		Email:        "new@example.com",
		Expire:       "new-expire",
	}
	now := time.Date(2026, 4, 22, 10, 11, 12, 0, time.UTC)

	applyRefreshedCodexTokenState(auth, storage, td, now)

	if got := auth.Metadata["id_token"]; got != td.IDToken {
		t.Fatalf("metadata id_token = %v, want %q", got, td.IDToken)
	}
	if got := auth.Metadata["access_token"]; got != td.AccessToken {
		t.Fatalf("metadata access_token = %v, want %q", got, td.AccessToken)
	}
	if got := auth.Metadata["refresh_token"]; got != td.RefreshToken {
		t.Fatalf("metadata refresh_token = %v, want %q", got, td.RefreshToken)
	}
	if got := auth.Metadata["account_id"]; got != td.AccountID {
		t.Fatalf("metadata account_id = %v, want %q", got, td.AccountID)
	}
	if got := auth.Metadata["email"]; got != td.Email {
		t.Fatalf("metadata email = %v, want %q", got, td.Email)
	}
	if got := auth.Metadata["expired"]; got != td.Expire {
		t.Fatalf("metadata expired = %v, want %q", got, td.Expire)
	}
	if got := auth.Metadata["type"]; got != "codex" {
		t.Fatalf("metadata type = %v, want codex", got)
	}
	if got := auth.Metadata["last_refresh"]; got != now.Format(time.RFC3339) {
		t.Fatalf("metadata last_refresh = %v, want %q", got, now.Format(time.RFC3339))
	}
	if storage.IDToken != td.IDToken {
		t.Fatalf("storage id_token = %q, want %q", storage.IDToken, td.IDToken)
	}
	if storage.AccessToken != td.AccessToken {
		t.Fatalf("storage access_token = %q, want %q", storage.AccessToken, td.AccessToken)
	}
	if storage.RefreshToken != td.RefreshToken {
		t.Fatalf("storage refresh_token = %q, want %q", storage.RefreshToken, td.RefreshToken)
	}
	if storage.AccountID != td.AccountID {
		t.Fatalf("storage account_id = %q, want %q", storage.AccountID, td.AccountID)
	}
	if storage.Email != td.Email {
		t.Fatalf("storage email = %q, want %q", storage.Email, td.Email)
	}
	if storage.Expire != td.Expire {
		t.Fatalf("storage expired = %q, want %q", storage.Expire, td.Expire)
	}
	if storage.LastRefresh != now.Format(time.RFC3339) {
		t.Fatalf("storage last_refresh = %q, want %q", storage.LastRefresh, now.Format(time.RFC3339))
	}
}

func TestApplyRefreshedCodexTokenState_PreservesExistingRefreshTokenWhenUpstreamOmitted(t *testing.T) {
	storage := &codexauth.CodexTokenStorage{
		RefreshToken: "old-refresh",
		Type:         "codex",
	}
	auth := &cliproxyauth.Auth{
		ID:       "auth-2",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex", "refresh_token": "old-refresh"},
		Storage:  storage,
	}
	td := &codexauth.CodexTokenData{
		IDToken:     "new-id",
		AccessToken: "new-access",
		AccountID:   "new-account",
		Email:       "new@example.com",
		Expire:      "new-expire",
	}
	now := time.Date(2026, 4, 22, 10, 12, 13, 0, time.UTC)

	applyRefreshedCodexTokenState(auth, storage, td, now)

	if got := auth.Metadata["refresh_token"]; got != "old-refresh" {
		t.Fatalf("metadata refresh_token = %v, want %q", got, "old-refresh")
	}
	if storage.RefreshToken != "old-refresh" {
		t.Fatalf("storage refresh_token = %q, want %q", storage.RefreshToken, "old-refresh")
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
