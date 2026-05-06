package helps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCodexLoadRunnerBuildRequestBodyIncludesScenarioMetadata(t *testing.T) {
	body, err := BuildCodexLoadRunnerRequestBody(CodexLoadRunnerOptions{
		Model:    "gpt-5.4",
		Scenario: "reasoning-before-output",
		Stream:   true,
		Input:    "hello",
	})
	if err != nil {
		t.Fatalf("BuildCodexLoadRunnerRequestBody error: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, `"model":"gpt-5.4"`) {
		t.Fatalf("request body missing model: %s", got)
	}
	if !strings.Contains(got, `"stream":true`) {
		t.Fatalf("request body missing stream flag: %s", got)
	}
	if !strings.Contains(got, `"cpa_mock_scenario":"reasoning-before-output"`) {
		t.Fatalf("request body missing scenario metadata: %s", got)
	}
}

func TestCodexLoadRunnerRunTracksStreamingFirstByteAndCompletion(t *testing.T) {
	mock := NewCodexUpstreamMock()
	server := httptest.NewServer(mock.Handler())
	defer server.Close()

	summary, err := RunCodexLoad(context.Background(), CodexLoadRunnerOptions{
		TargetURL:    server.URL + "/responses",
		Requests:     1,
		Concurrency:  1,
		Model:        "gpt-5.4",
		Scenario:     "reasoning-before-output",
		Stream:       true,
		RequestInput: "hello",
		Timeout:      5 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunCodexLoad error: %v", err)
	}
	if summary.Requests != 1 {
		t.Fatalf("requests = %d, want 1", summary.Requests)
	}
	if summary.Successes != 1 {
		t.Fatalf("successes = %d, want 1", summary.Successes)
	}
	if summary.Failures != 0 {
		t.Fatalf("failures = %d, want 0", summary.Failures)
	}
	if summary.CompletedResponses != 1 {
		t.Fatalf("completed_responses = %d, want 1", summary.CompletedResponses)
	}
	if summary.FirstByteSamples != 1 {
		t.Fatalf("first_byte_samples = %d, want 1", summary.FirstByteSamples)
	}
	if summary.ZeroOutputResponses != 0 {
		t.Fatalf("zero_output_responses = %d, want 0", summary.ZeroOutputResponses)
	}
}

func TestCodexLoadRunnerRunTracksZeroOutputResponses(t *testing.T) {
	mock := NewCodexUpstreamMock()
	server := httptest.NewServer(mock.Handler())
	defer server.Close()

	summary, err := RunCodexLoad(context.Background(), CodexLoadRunnerOptions{
		TargetURL:    server.URL + "/responses",
		Requests:     1,
		Concurrency:  1,
		Model:        "gpt-5.4",
		Scenario:     "zero-usage-complete",
		Stream:       true,
		RequestInput: "hello",
		Timeout:      5 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunCodexLoad error: %v", err)
	}
	if summary.ZeroOutputResponses != 1 {
		t.Fatalf("zero_output_responses = %d, want 1", summary.ZeroOutputResponses)
	}
	if summary.CompletedResponses != 1 {
		t.Fatalf("completed_responses = %d, want 1", summary.CompletedResponses)
	}
}

func TestCodexLoadRunnerRunTracksServerErrors(t *testing.T) {
	mock := NewCodexUpstreamMock()
	server := httptest.NewServer(mock.Handler())
	defer server.Close()

	summary, err := RunCodexLoad(context.Background(), CodexLoadRunnerOptions{
		TargetURL:    server.URL + "/responses",
		Requests:     1,
		Concurrency:  1,
		Model:        "gpt-5.4",
		Scenario:     "server-error",
		Stream:       true,
		RequestInput: "hello",
		Timeout:      5 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunCodexLoad error: %v", err)
	}
	if summary.Failures != 1 {
		t.Fatalf("failures = %d, want 1", summary.Failures)
	}
	if summary.Status5xx != 1 {
		t.Fatalf("status_5xx = %d, want 1", summary.Status5xx)
	}
	if summary.Successes != 0 {
		t.Fatalf("successes = %d, want 0", summary.Successes)
	}
}

func TestCodexLoadRunnerRunIssuesAuthHeaderWhenConfigured(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	_, err := RunCodexLoad(context.Background(), CodexLoadRunnerOptions{
		TargetURL:    server.URL,
		Requests:     1,
		Concurrency:  1,
		Model:        "gpt-5.4",
		Scenario:     "fast-complete",
		Stream:       true,
		RequestInput: "hello",
		APIKey:       "test-key",
		Timeout:      5 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunCodexLoad error: %v", err)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("authorization header = %q, want %q", gotAuth, "Bearer test-key")
	}
}
