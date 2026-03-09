package executor

import (
	"net/http"
	"strconv"
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
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
