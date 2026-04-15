package auth

import (
	"testing"
	"time"
)

func TestIsUsageLimitReachedError(t *testing.T) {
	tests := []struct {
		name string
		err  *Error
		want bool
	}{
		{
			name: "usage_limit_reached in message",
			err:  &Error{Message: `{"error":{"type":"usage_limit_reached","resets_at":1700000000}}`},
			want: true,
		},
		{
			name: "plain rate limit",
			err:  &Error{Message: "rate limit exceeded"},
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "resets_in_seconds present",
			err:  &Error{Message: `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`},
			want: true,
		},
		{
			name: "empty message",
			err:  &Error{Message: ""},
			want: false,
		},
		{
			name: "natural language usage limit from ChatGPT",
			err:  &Error{Message: "The usage limit has been reached"},
			want: true,
		},
		{
			name: "usage limit in detail field",
			err:  &Error{Message: `{"detail":"The usage limit has been reached"}`},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isUsageLimitReachedError(tt.err)
			if got != tt.want {
				t.Errorf("isUsageLimitReachedError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRetrySettings_DefaultsWhenUnconfigured(t *testing.T) {
	m := NewManager(nil, nil, nil)
	retry, maxCreds, maxWait := m.retrySettings()
	if retry == 0 {
		t.Error("expected non-zero default requestRetry")
	}
	if maxCreds == 0 {
		t.Error("expected non-zero default maxRetryCredentials")
	}
	if maxWait == 0 {
		t.Error("expected non-zero default maxRetryInterval")
	}
}

func TestRetrySettings_ExplicitOverridesDefaults(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(5, 300*time.Second, 10)
	retry, maxCreds, maxWait := m.retrySettings()
	if retry != 5 {
		t.Errorf("expected retry=5, got %d", retry)
	}
	if maxCreds != 10 {
		t.Errorf("expected maxCreds=10, got %d", maxCreds)
	}
	if maxWait != 300*time.Second {
		t.Errorf("expected maxWait=300s, got %v", maxWait)
	}
}

func TestNormalizedRetryAfter_FallbackFor429WithoutMatch(t *testing.T) {
	result := normalizedRetryAfter(429, nil, "some unknown 429 body")
	if result == nil {
		t.Error("expected fallback retry-after for unrecognized 429, got nil")
	}
	if result != nil && *result != plain429Cooldown {
		t.Errorf("expected %v fallback, got %v", plain429Cooldown, *result)
	}
}

func TestNormalizedRetryAfter_ExplicitRetryAfterPreferred(t *testing.T) {
	explicit := 5 * time.Minute
	result := normalizedRetryAfter(429, &explicit, "anything")
	if result == nil || *result != 5*time.Minute {
		t.Errorf("expected explicit 5m, got %v", result)
	}
}

func TestNormalizedRetryAfter_Non429ReturnsNil(t *testing.T) {
	result := normalizedRetryAfter(500, nil, "server error")
	if result != nil {
		t.Errorf("expected nil for non-429, got %v", result)
	}
}
