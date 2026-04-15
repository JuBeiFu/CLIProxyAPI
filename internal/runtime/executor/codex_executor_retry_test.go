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

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
