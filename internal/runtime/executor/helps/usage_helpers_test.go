package helps

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestParseOpenAIUsageChatCompletions(t *testing.T) {
	data := []byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":5}}}`)
	detail := ParseOpenAIUsage(data)
	if detail.InputTokens != 1 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 1)
	}
	if detail.OutputTokens != 2 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 2)
	}
	if detail.TotalTokens != 3 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 3)
	}
	if detail.CachedTokens != 4 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 4)
	}
	if detail.ReasoningTokens != 5 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 5)
	}
}

func TestParseOpenAIUsageResponses(t *testing.T) {
	data := []byte(`{"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":7},"output_tokens_details":{"reasoning_tokens":9}}}`)
	detail := ParseOpenAIUsage(data)
	if detail.InputTokens != 10 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 10)
	}
	if detail.OutputTokens != 20 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 20)
	}
	if detail.TotalTokens != 30 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 30)
	}
	if detail.CachedTokens != 7 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 7)
	}
	if detail.ReasoningTokens != 9 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 9)
	}
}

func TestUsageReporterBuildRecordIncludesLatency(t *testing.T) {
	reporter := &UsageReporter{
		provider:    "openai",
		model:       "gpt-5.4",
		requestedAt: time.Now().Add(-1500 * time.Millisecond),
	}

	record := reporter.buildRecord(usage.Detail{TotalTokens: 3}, false)
	if record.Latency < time.Second {
		t.Fatalf("latency = %v, want >= 1s", record.Latency)
	}
	if record.Latency > 3*time.Second {
		t.Fatalf("latency = %v, want <= 3s", record.Latency)
	}
}

func TestCompactFailureFromContextDeadlineExceeded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest("POST", "/v1/responses/compact", nil)
	ginCtx.Set(apiAttemptsKey, []*upstreamAttempt{{
		index:   1,
		request: "=== API REQUEST 1 ===\nUpstream URL: https://chatgpt.com/backend-api/codex/responses/compact\n",
	}})
	ginCtx.Set(apiResponseKey, []byte("response body"))

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	sample := CompactFailureFromContext(
		ctx,
		&config.Config{},
		&cliproxyauth.Auth{Provider: "codex"},
		`Post "https://chatgpt.com/backend-api/codex/responses/compact": context deadline exceeded`,
		errors.New(`Post "https://chatgpt.com/backend-api/codex/responses/compact": context deadline exceeded`),
	)
	if sample == nil {
		t.Fatal("expected compact failure sample")
	}
	if sample.ErrorClass != "timeout" {
		t.Fatalf("error class = %q, want timeout", sample.ErrorClass)
	}
	if sample.FailureStage != "upstream_roundtrip_timeout" {
		t.Fatalf("failure stage = %q, want upstream_roundtrip_timeout", sample.FailureStage)
	}
	if sample.ProxyMode != "direct" {
		t.Fatalf("proxy mode = %q, want direct", sample.ProxyMode)
	}
	if sample.UpstreamURL != "https://chatgpt.com/backend-api/codex/responses/compact" {
		t.Fatalf("upstream url = %q", sample.UpstreamURL)
	}
}

func TestCompactFailureFromContextProxyReset(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest("POST", "/v1/responses/compact", nil)
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	sample := CompactFailureFromContext(
		ctx,
		&config.Config{SDKConfig: config.SDKConfig{ProxyURL: "socks5://warp-lb:1080"}},
		&cliproxyauth.Auth{Provider: "codex"},
		`read tcp 172.17.0.2:52642->154.40.43.194:1080: read: connection reset by peer`,
		errors.New(`read tcp 172.17.0.2:52642->154.40.43.194:1080: read: connection reset by peer`),
	)
	if sample == nil {
		t.Fatal("expected compact failure sample")
	}
	if sample.ErrorClass != "connection_reset" {
		t.Fatalf("error class = %q, want connection_reset", sample.ErrorClass)
	}
	if sample.ProxyMode != "proxy" {
		t.Fatalf("proxy mode = %q, want proxy", sample.ProxyMode)
	}
	if sample.ProxyTarget != "socks5://warp-lb:1080" {
		t.Fatalf("proxy target = %q", sample.ProxyTarget)
	}
}
