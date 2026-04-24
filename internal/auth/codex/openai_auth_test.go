package codex

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRefreshTokensWithRetry_NonRetryableOnlyAttemptsOnce(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "refresh token reused",
			body: `{"error":"invalid_grant","code":"refresh_token_reused"}`,
			want: "refresh_token_reused",
		},
		{
			name: "refresh token invalidated",
			body: `{"error":{"message":"Your refresh token has been invalidated. Please try signing in again.","type":"invalid_request_error","code":"token_invalidated"}}`,
			want: "invalidated",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			auth := &CodexAuth{
				httpClient: &http.Client{
					Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
						atomic.AddInt32(&calls, 1)
						return &http.Response{
							StatusCode: http.StatusBadRequest,
							Body:       io.NopCloser(strings.NewReader(tc.body)),
							Header:     make(http.Header),
							Request:    req,
						}, nil
					}),
				},
			}

			_, err := auth.RefreshTokensWithRetry(context.Background(), "dummy_refresh_token", 3)
			if err == nil {
				t.Fatalf("expected error for non-retryable refresh failure")
			}
			if !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("expected %q in error, got: %v", tc.want, err)
			}
			if got := atomic.LoadInt32(&calls); got != 1 {
				t.Fatalf("expected 1 refresh attempt, got %d", got)
			}
		})
	}
}
