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

// The probe MUST be bearer-only. Empirical (2026-04-20): adding
// Chatgpt-Account-Id flips /wham/usage to a workspace-scoped view that
// defaults to "plus" for any user owning a paid workspace, masking newly
// registered free accounts pending upgrade. Bearer-only returns the
// access_token's own user_id and that user's true plan_type.
func TestFetchWhamUsagePlanType_OmitsChatgptAccountIDHeader(t *testing.T) {
	t.Parallel()
	var hasHeader bool
	auth := &CodexAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if _, ok := req.Header["Chatgpt-Account-Id"]; ok {
					hasHeader = true
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"plan_type":"free"}`)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}),
		},
	}
	_, _ = auth.FetchWhamUsagePlanType(context.Background(), "tok")
	if hasHeader {
		t.Fatalf("Chatgpt-Account-Id header MUST NOT be sent on /wham/usage")
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
	// or returns an error. Either is acceptable 鈥?but got MUST be empty.
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

func TestFetchWhamUsageInfoExtractsSupportedModels(t *testing.T) {
	t.Parallel()
	auth := &CodexAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body: io.NopCloser(strings.NewReader(
						`{"plan_type":"plus","entitlements":{"models":["gpt-5.4",{"id":"gpt-5.5"}]}}`)),
					Header:  make(http.Header),
					Request: req,
				}, nil
			}),
		},
	}
	got, err := auth.FetchWhamUsageInfo(context.Background(), "tok")
	if err != nil {
		t.Fatalf("FetchWhamUsageInfo error = %v", err)
	}
	if got.PlanType != "plus" {
		t.Fatalf("PlanType = %q, want plus", got.PlanType)
	}
	want := []string{"gpt-5.4", "gpt-5.5"}
	if len(got.SupportedModels) != len(want) {
		t.Fatalf("SupportedModels = %v, want %v", got.SupportedModels, want)
	}
	for index, wantModel := range want {
		if got.SupportedModels[index] != wantModel {
			t.Fatalf("SupportedModels[%d] = %q, want %q", index, got.SupportedModels[index], wantModel)
		}
	}
}
