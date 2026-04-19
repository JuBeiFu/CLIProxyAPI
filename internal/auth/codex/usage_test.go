package codex

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestFetchWhamUsagePlanType_Plus(t *testing.T) {
	t.Parallel()
	auth := &CodexAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.String() != "https://chatgpt.com/backend-api/wham/usage" {
					t.Errorf("unexpected URL: %s", req.URL.String())
				}
				if got := req.Header.Get("Authorization"); got != "Bearer test-access-token" {
					t.Errorf("Authorization header = %q, want Bearer test-access-token", got)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body: io.NopCloser(strings.NewReader(
						`{"user_id":"u1","plan_type":"plus","rate_limit":{"limit_reached":false}}`)),
					Header:  make(http.Header),
					Request: req,
				}, nil
			}),
		},
	}
	got, err := auth.FetchWhamUsagePlanType(context.Background(), "test-access-token")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "plus" {
		t.Fatalf("plan_type = %q, want plus", got)
	}
}

func TestFetchWhamUsagePlanType_Free(t *testing.T) {
	t.Parallel()
	auth := &CodexAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body: io.NopCloser(strings.NewReader(
						`{"plan_type":"free"}`)),
					Header:  make(http.Header),
					Request: req,
				}, nil
			}),
		},
	}
	got, err := auth.FetchWhamUsagePlanType(context.Background(), "tok")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "free" {
		t.Fatalf("got %q", got)
	}
}

func TestFetchWhamUsagePlanType_HTTPError(t *testing.T) {
	t.Parallel()
	auth := &CodexAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Body:       io.NopCloser(strings.NewReader(`unauthorized`)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}),
		},
	}
	got, err := auth.FetchWhamUsagePlanType(context.Background(), "tok")
	if err == nil {
		t.Fatalf("expected error for 401")
	}
	if got != "" {
		t.Fatalf("plan_type should be empty on error, got %q", got)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error should mention status code, got: %v", err)
	}
}

func TestFetchWhamUsagePlanType_MalformedJSON(t *testing.T) {
	t.Parallel()
	auth := &CodexAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`not-json{{{`)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}),
		},
	}
	got, err := auth.FetchWhamUsagePlanType(context.Background(), "tok")
	// Malformed JSON: either returns empty string with no error (gjson treats as empty),
	// or returns an error. Either is acceptable — but got MUST be empty.
	if got != "" {
		t.Fatalf("plan_type should be empty for malformed JSON, got %q (err=%v)", got, err)
	}
}

func TestFetchWhamUsagePlanType_EmptyToken(t *testing.T) {
	t.Parallel()
	auth := &CodexAuth{httpClient: &http.Client{}}
	_, err := auth.FetchWhamUsagePlanType(context.Background(), "")
	if err == nil {
		t.Fatalf("expected error for empty access token")
	}
}

func TestFetchWhamUsagePlanType_NetworkError(t *testing.T) {
	t.Parallel()
	auth := &CodexAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return nil, io.ErrUnexpectedEOF
			}),
		},
	}
	got, err := auth.FetchWhamUsagePlanType(context.Background(), "tok")
	if err == nil {
		t.Fatalf("expected error for network failure")
	}
	if got != "" {
		t.Fatalf("plan_type should be empty on network error, got %q", got)
	}
}
