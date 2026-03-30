package executor

import (
	"net/http"
	"testing"
	"time"
)

func TestParseCodexWebsocketError_UsageLimitReachedNormalizesTo429(t *testing.T) {
	payload := []byte(`{"type":"error","status":400,"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":308415}}`)
	err, ok := parseCodexWebsocketError(payload)
	if !ok || err == nil {
		t.Fatalf("expected websocket error, got ok=%v err=%v", ok, err)
	}

	sc, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("expected StatusCode() on error, got %T", err)
	}
	if sc.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("expected status 429, got %d", sc.StatusCode())
	}

	rap, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok {
		t.Fatalf("expected RetryAfter() on error, got %T", err)
	}
	retryAfter := rap.RetryAfter()
	if retryAfter == nil {
		t.Fatalf("expected retryAfter, got nil")
	}
	if want := 308415 * time.Second; *retryAfter != want {
		t.Fatalf("expected retryAfter %v, got %v", want, *retryAfter)
	}

	hp, ok := err.(interface{ Headers() http.Header })
	if !ok {
		t.Fatalf("expected Headers() on error, got %T", err)
	}
	headers := hp.Headers()
	if headers == nil {
		t.Fatalf("expected headers, got nil")
	}
	if got := headers.Get("Retry-After"); got != "308415" {
		t.Fatalf("expected Retry-After=308415, got %q", got)
	}
}

func TestParseCodexWebsocketError_UsageLimitReachedWithoutStatusStillWorks(t *testing.T) {
	payload := []byte(`{"type":"error","error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":30}}`)
	err, ok := parseCodexWebsocketError(payload)
	if !ok || err == nil {
		t.Fatalf("expected websocket error, got ok=%v err=%v", ok, err)
	}

	sc, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("expected StatusCode() on error, got %T", err)
	}
	if sc.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("expected status 429, got %d", sc.StatusCode())
	}

	rap, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok {
		t.Fatalf("expected RetryAfter() on error, got %T", err)
	}
	retryAfter := rap.RetryAfter()
	if retryAfter == nil {
		t.Fatalf("expected retryAfter, got nil")
	}
	if want := 30 * time.Second; *retryAfter != want {
		t.Fatalf("expected retryAfter %v, got %v", want, *retryAfter)
	}
}

func TestParseCodexWebsocketError_MissingStatusAndNotUsageLimitIsIgnored(t *testing.T) {
	payload := []byte(`{"type":"error","error":{"type":"server_error","message":"no status"}}`)
	err, ok := parseCodexWebsocketError(payload)
	if ok || err != nil {
		t.Fatalf("expected no parsed error, got ok=%v err=%v", ok, err)
	}
}

func TestParseCodexWebsocketError_CapacityErrorNormalizesTo429(t *testing.T) {
	payload := []byte(`{"type":"error","status":500,"error":{"type":"server_error","message":"Selected model is at capacity. Please try a different model."}}`)
	err, ok := parseCodexWebsocketError(payload)
	if !ok || err == nil {
		t.Fatalf("expected websocket error, got ok=%v err=%v", ok, err)
	}

	sc, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("expected StatusCode() on error, got %T", err)
	}
	if sc.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("expected status 429, got %d", sc.StatusCode())
	}

	rap, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok {
		t.Fatalf("expected RetryAfter() on error, got %T", err)
	}
	retryAfter := rap.RetryAfter()
	if retryAfter == nil {
		t.Fatalf("expected retryAfter, got nil")
	}
	if want := 5 * time.Second; *retryAfter != want {
		t.Fatalf("expected retryAfter %v, got %v", want, *retryAfter)
	}
}
