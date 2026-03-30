package executor

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
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
		got := parseCodexRetryAfter(http.StatusBadRequest, body, now)
		if got == nil {
			t.Fatal("expected retryAfter for non-429 usage_limit_reached")
		}
		if *got != 30*time.Second {
			t.Fatalf("retryAfter = %v, want %v", *got, 30*time.Second)
		}
	})

	t.Run("non usage_limit_reached error type", func(t *testing.T) {
		body := []byte(`{"error":{"type":"server_error","resets_in_seconds":30}}`)
		if got := parseCodexRetryAfter(http.StatusTooManyRequests, body, now); got != nil {
			t.Fatalf("expected nil for non-usage_limit_reached, got %v", *got)
		}
	})

	t.Run("newCodexStatusErr normalizes usage_limit_reached to 429", func(t *testing.T) {
		body := []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":45}}`)
		err := newCodexStatusErr(http.StatusBadRequest, body)
		if err.StatusCode() != http.StatusTooManyRequests {
			t.Fatalf("statusCode = %d, want %d", err.StatusCode(), http.StatusTooManyRequests)
		}
		retryAfter := err.RetryAfter()
		if retryAfter == nil {
			t.Fatal("expected retryAfter, got nil")
		}
		if *retryAfter != 45*time.Second {
			t.Fatalf("retryAfter = %v, want %v", *retryAfter, 45*time.Second)
		}
	})

	t.Run("newCodexStatusErr normalizes capacity error to 429", func(t *testing.T) {
		body := []byte(`{"error":{"message":"Selected model is at capacity. Please try a different model.","type":"server_error"}}`)
		err := newCodexStatusErr(http.StatusInternalServerError, body)
		if err.StatusCode() != http.StatusTooManyRequests {
			t.Fatalf("statusCode = %d, want %d", err.StatusCode(), http.StatusTooManyRequests)
		}
		retryAfter := err.RetryAfter()
		if retryAfter == nil {
			t.Fatal("expected retryAfter, got nil")
		}
		if *retryAfter != 5*time.Second {
			t.Fatalf("retryAfter = %v, want %v", *retryAfter, 5*time.Second)
		}
	})
}

func TestNormalizeCodexRefreshErr(t *testing.T) {
	t.Run("wraps refresh status errors with status code", func(t *testing.T) {
		raw := errors.New(`token refresh failed after 3 attempts: token refresh failed with status 401: {"error":{"message":"Your OpenAI account has been deactivated, please check your email for more information.","type":"invalid_request_error","code":"account_deactivated","param":null},"status":401}`)

		err := normalizeCodexRefreshErr(raw)
		statusErr, ok := err.(interface{ StatusCode() int })
		if !ok {
			t.Fatalf("expected StatusCode() error, got %T", err)
		}
		if statusErr.StatusCode() != http.StatusUnauthorized {
			t.Fatalf("statusCode = %d, want %d", statusErr.StatusCode(), http.StatusUnauthorized)
		}
		if err.Error() == raw.Error() {
			t.Fatalf("expected normalized error message, got original %q", err.Error())
		}
		if err.Error() != `{"error":{"message":"Your OpenAI account has been deactivated, please check your email for more information.","type":"invalid_request_error","code":"account_deactivated","param":null},"status":401}` {
			t.Fatalf("error message = %q", err.Error())
		}
	})

	t.Run("leaves generic errors untouched", func(t *testing.T) {
		raw := errors.New("token refresh failed after 3 attempts: dial tcp timeout")
		err := normalizeCodexRefreshErr(raw)
		if err != raw {
			t.Fatalf("expected original error to be returned unchanged")
		}
	})
}

func TestNormalizeCodexDoRequestErr(t *testing.T) {
	t.Run("wraps goaway protocol error as retryable status", func(t *testing.T) {
		raw := errors.New(`Post "https://chatgpt.com/backend-api/codex/responses": http2: server sent GOAWAY and closed the connection; LastStreamID=3623, ErrCode=PROTOCOL_ERROR, debug=""`)
		err := normalizeCodexDoRequestErr(raw)
		statusErr, ok := err.(interface{ StatusCode() int })
		if !ok {
			t.Fatalf("expected StatusCode() error, got %T", err)
		}
		if statusErr.StatusCode() != http.StatusBadGateway {
			t.Fatalf("statusCode = %d, want %d", statusErr.StatusCode(), http.StatusBadGateway)
		}
		if !strings.Contains(strings.ToLower(err.Error()), "stream disconnected before completion") {
			t.Fatalf("expected normalized stream-disconnect marker, got %q", err.Error())
		}
	})

	t.Run("wraps error sending request for url as retryable status", func(t *testing.T) {
		raw := errors.New("stream disconnected before completion: error sending request for url (https://api.openai.com/v1/responses)")
		err := normalizeCodexDoRequestErr(raw)
		statusErr, ok := err.(interface{ StatusCode() int })
		if !ok {
			t.Fatalf("expected StatusCode() error, got %T", err)
		}
		if statusErr.StatusCode() != http.StatusBadGateway {
			t.Fatalf("statusCode = %d, want %d", statusErr.StatusCode(), http.StatusBadGateway)
		}
	})

	t.Run("keeps stream disconnected timeout as 408 status", func(t *testing.T) {
		raw := errors.New("stream disconnected before completion: stream closed before response.completed")
		err := normalizeCodexDoRequestErr(raw)
		statusErr, ok := err.(interface{ StatusCode() int })
		if !ok {
			t.Fatalf("expected StatusCode() error, got %T", err)
		}
		if statusErr.StatusCode() != http.StatusRequestTimeout {
			t.Fatalf("statusCode = %d, want %d", statusErr.StatusCode(), http.StatusRequestTimeout)
		}
	})

	t.Run("leaves unrelated transport errors unchanged", func(t *testing.T) {
		raw := errors.New("dial tcp 1.2.3.4:443: i/o timeout")
		err := normalizeCodexDoRequestErr(raw)
		if err != raw {
			t.Fatalf("expected unchanged error")
		}
	})
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
