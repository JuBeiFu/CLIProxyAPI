package codex

import (
	"context"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
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

func TestFetchWhamUsageInfoExtractsFiveHourQuota(t *testing.T) {
	t.Parallel()
	resetAt := time.Now().Add(2 * time.Hour).UTC().Unix()
	weeklyResetAt := time.Now().Add(4 * 24 * time.Hour).UTC().Unix()
	auth := &CodexAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body: io.NopCloser(strings.NewReader(
						`{"plan_type":"plus","rate_limits":[{"window":"5h","limit":100,"remaining":19,"resets_at":` + strconv.FormatInt(resetAt, 10) + `},{"window":"week","limit":200,"remaining":0,"resets_at":` + strconv.FormatInt(weeklyResetAt, 10) + `}]}`)),
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
	if got.FiveHourQuota == nil {
		t.Fatal("FiveHourQuota = nil, want parsed quota")
	}
	if got.FiveHourQuota.RemainingRatio != 0.19 {
		t.Fatalf("RemainingRatio = %v, want 0.19", got.FiveHourQuota.RemainingRatio)
	}
	if got.FiveHourQuota.ResetAt.IsZero() || got.FiveHourQuota.ResetAt.Unix() != resetAt {
		t.Fatalf("ResetAt = %s, want unix %d", got.FiveHourQuota.ResetAt, resetAt)
	}
	if got.WeeklyQuota == nil {
		t.Fatal("WeeklyQuota = nil, want parsed quota")
	}
	if got.WeeklyQuota.RemainingRatio != 0 {
		t.Fatalf("Weekly RemainingRatio = %v, want 0", got.WeeklyQuota.RemainingRatio)
	}
	if got.WeeklyQuota.ResetAt.IsZero() || got.WeeklyQuota.ResetAt.Unix() != weeklyResetAt {
		t.Fatalf("Weekly ResetAt = %s, want unix %d", got.WeeklyQuota.ResetAt, weeklyResetAt)
	}
}

func TestParseWhamUsageFiveHourQuota(t *testing.T) {
	t.Parallel()

	resetAt := time.Now().Add(2 * time.Hour).UTC().Unix()
	got := ParseWhamUsageFiveHourQuota([]byte(
		`{"rate_limits":[{"window":"5h","limit":100,"remaining":19,"resets_at":` + strconv.FormatInt(resetAt, 10) + `}]}`,
	))
	if got == nil {
		t.Fatal("ParseWhamUsageFiveHourQuota = nil, want quota")
	}
	if got.RemainingRatio != 0.19 {
		t.Fatalf("RemainingRatio = %v, want 0.19", got.RemainingRatio)
	}
}

func TestParseWhamUsageWeeklyQuota(t *testing.T) {
	t.Parallel()

	resetAt := time.Now().Add(4 * 24 * time.Hour).UTC().Unix()
	got := ParseWhamUsageWeeklyQuota([]byte(
		`{"rate_limits":[{"window":"5h","limit":100,"remaining":19},{"window":"week","limit":200,"remaining":0,"resets_at":` + strconv.FormatInt(resetAt, 10) + `}]}`,
	))
	if got == nil {
		t.Fatal("ParseWhamUsageWeeklyQuota = nil, want quota")
	}
	if got.RemainingRatio != 0 {
		t.Fatalf("RemainingRatio = %v, want 0", got.RemainingRatio)
	}
	if got.ResetAt.IsZero() || got.ResetAt.Unix() != resetAt {
		t.Fatalf("ResetAt = %s, want unix %d", got.ResetAt, resetAt)
	}
}

func TestParseWhamUsageFiveHourQuotaFromUsedPercentWindow(t *testing.T) {
	t.Parallel()

	resetAt := time.Now().Add(2 * time.Hour).UTC().Unix()
	got := ParseWhamUsageFiveHourQuota([]byte(
		`{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":58,"reset_at":` + strconv.FormatInt(resetAt, 10) + `}}}`,
	))
	if got == nil {
		t.Fatal("ParseWhamUsageFiveHourQuota = nil, want quota")
	}
	if math.Abs(got.RemainingRatio-0.42) > 0.0001 {
		t.Fatalf("RemainingRatio = %v, want 0.42", got.RemainingRatio)
	}
	if got.ResetAt.IsZero() || got.ResetAt.Unix() != resetAt {
		t.Fatalf("ResetAt = %s, want unix %d", got.ResetAt, resetAt)
	}
}

func TestParseWhamUsageFiveHourQuotaPrefersPrimaryRateLimit(t *testing.T) {
	t.Parallel()

	primaryResetAt := time.Now().Add(2 * time.Hour).UTC().Unix()
	reviewResetAt := time.Now().Add(1 * time.Hour).UTC().Unix()
	got := ParseWhamUsageFiveHourQuota([]byte(
		`{
			"code_review_rate_limit":{
				"primary_window":{"limit_window_seconds":18000,"used_percent":100,"reset_at":` + strconv.FormatInt(reviewResetAt, 10) + `}
			},
			"rate_limit":{
				"primary_window":{"limit_window_seconds":18000,"used_percent":1,"reset_at":` + strconv.FormatInt(primaryResetAt, 10) + `}
			}
		}`,
	))
	if got == nil {
		t.Fatal("ParseWhamUsageFiveHourQuota = nil, want quota")
	}
	if math.Abs(got.RemainingRatio-0.99) > 0.0001 {
		t.Fatalf("RemainingRatio = %v, want 0.99", got.RemainingRatio)
	}
	if got.ResetAt.IsZero() || got.ResetAt.Unix() != primaryResetAt {
		t.Fatalf("ResetAt = %s, want primary unix %d", got.ResetAt, primaryResetAt)
	}
}
