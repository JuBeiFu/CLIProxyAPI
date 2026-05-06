package helps

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCodexUpstreamMockSelectsScenarioFromMetadata(t *testing.T) {
	mock := NewCodexUpstreamMock()
	server := httptest.NewServer(mock.Handler())
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/responses", strings.NewReader(`{"model":"gpt-5.4","stream":true,"metadata":{"cpa_mock_scenario":"reasoning-before-output"}}`))
	if err != nil {
		t.Fatalf("NewRequest error: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do error: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "response.reasoning_summary_text.delta") {
		t.Fatalf("response body missing reasoning delta: %s", got)
	}
	if !strings.Contains(got, "response.output_text.delta") {
		t.Fatalf("response body missing output delta: %s", got)
	}
	if !strings.Contains(got, "response.completed") {
		t.Fatalf("response body missing completion: %s", got)
	}

	stats := mock.Snapshot()
	scenario := stats.Scenarios["reasoning-before-output"]
	if scenario.Requests != 1 {
		t.Fatalf("scenario requests = %d, want 1", scenario.Requests)
	}
	if scenario.Completed != 1 {
		t.Fatalf("scenario completed = %d, want 1", scenario.Completed)
	}
}

func TestCodexUpstreamMockSelectsScenarioFromPathPrefix(t *testing.T) {
	mock := NewCodexUpstreamMock()
	server := httptest.NewServer(mock.Handler())
	defer server.Close()

	resp, err := http.Post(server.URL+"/scenarios/zero-usage-complete/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4","stream":true}`))
	if err != nil {
		t.Fatalf("Post error: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, `"output_tokens":0`) {
		t.Fatalf("response body missing zero-usage completion: %s", got)
	}
	if strings.Contains(got, "response.output_text.delta") {
		t.Fatalf("zero-usage scenario unexpectedly emitted output delta: %s", got)
	}
}

func TestCodexUpstreamMockInjectedEOFAppearsInStats(t *testing.T) {
	mock := NewCodexUpstreamMock()
	server := httptest.NewServer(mock.Handler())
	defer server.Close()

	resp, err := http.Post(server.URL+"/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4","stream":true,"metadata":{"cpa_mock_scenario":"reasoning-eof"}}`))
	if err != nil {
		t.Fatalf("Post error: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "response.reasoning_summary_text.delta") {
		t.Fatalf("response body missing reasoning delta: %s", got)
	}
	if strings.Contains(got, "response.completed") {
		t.Fatalf("unexpected completion in EOF scenario: %s", got)
	}

	stats := mock.Snapshot()
	scenario := stats.Scenarios["reasoning-eof"]
	if scenario.Requests != 1 {
		t.Fatalf("scenario requests = %d, want 1", scenario.Requests)
	}
	if scenario.InjectedEOF != 1 {
		t.Fatalf("scenario injected_eof = %d, want 1", scenario.InjectedEOF)
	}
	if scenario.Completed != 0 {
		t.Fatalf("scenario completed = %d, want 0", scenario.Completed)
	}
}

func TestCodexUpstreamMockTracksClientDisconnects(t *testing.T) {
	mock := NewCodexUpstreamMock()
	server := httptest.NewServer(mock.Handler())
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/responses", strings.NewReader(`{"model":"gpt-5.4","stream":true,"metadata":{"cpa_mock_scenario":"stall-before-output"}}`))
	if err != nil {
		t.Fatalf("NewRequest error: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do error: %v", err)
	}

	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("ReadString error: %v", err)
	}
	if !strings.Contains(line, "response.created") {
		t.Fatalf("first line = %q, want response.created", line)
	}

	cancel()
	_ = resp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stats := mock.Snapshot()
		if stats.Scenarios["stall-before-output"].ClientCanceled == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("client cancellation was not recorded")
}

func TestCodexUpstreamMockCompactAndStatsEndpoints(t *testing.T) {
	mock := NewCodexUpstreamMock()
	server := httptest.NewServer(mock.Handler())
	defer server.Close()

	resp, err := http.Post(server.URL+"/responses/compact", "application/json", strings.NewReader(`{"model":"gpt-5.4","input":"hello","metadata":{"cpa_mock_scenario":"compact-success"}}`))
	if err != nil {
		t.Fatalf("Post error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("compact status = %d, want 200", resp.StatusCode)
	}

	statsResp, err := http.Get(server.URL + "/_mock/stats")
	if err != nil {
		t.Fatalf("Get stats error: %v", err)
	}
	defer statsResp.Body.Close()

	statsBody, err := io.ReadAll(statsResp.Body)
	if err != nil {
		t.Fatalf("ReadAll stats error: %v", err)
	}
	got := string(statsBody)
	if !strings.Contains(got, `"compact-success"`) {
		t.Fatalf("stats missing compact-success scenario: %s", got)
	}
	if !strings.Contains(got, `"requests":1`) {
		t.Fatalf("stats missing request count: %s", got)
	}
}

func TestCodexUpstreamMockTracksAuthAndProfileStats(t *testing.T) {
	t.Parallel()

	mock := NewCodexUpstreamMock()
	server := httptest.NewServer(mock.Handler())
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/responses", strings.NewReader(`{"model":"gpt-5.4","stream":true}`))
	if err != nil {
		t.Fatalf("NewRequest error: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mock-Auth-ID", "auth-0001")
	req.Header.Set("X-Mock-Profile", "slow-first-byte")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do error: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	stats := mock.Snapshot()
	if stats.Routes["responses"].Requests != 1 {
		t.Fatalf("route requests = %d, want 1", stats.Routes["responses"].Requests)
	}
	if stats.Scenarios["fast-complete"].Requests != 1 {
		t.Fatalf("scenario requests = %d, want 1", stats.Scenarios["fast-complete"].Requests)
	}
	if stats.Auths["auth-0001"].Requests != 1 {
		t.Fatalf("auth requests = %d, want 1", stats.Auths["auth-0001"].Requests)
	}
	if stats.Profiles["slow-first-byte"].Requests != 1 {
		t.Fatalf("profile requests = %d, want 1", stats.Profiles["slow-first-byte"].Requests)
	}
}

func TestCodexUpstreamMockIntermittent429ProfileEveryThirdRequest(t *testing.T) {
	t.Parallel()

	mock := NewCodexUpstreamMock()
	server := httptest.NewServer(mock.Handler())
	defer server.Close()

	statuses := make([]int, 0, 3)
	for i := 0; i < 3; i++ {
		req, err := http.NewRequest(http.MethodPost, server.URL+"/responses", strings.NewReader(`{"model":"gpt-5.4","stream":false}`))
		if err != nil {
			t.Fatalf("NewRequest error: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Mock-Auth-ID", "auth-0002")
		req.Header.Set("X-Mock-Profile", "intermittent-429")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do error: %v", err)
		}
		statuses = append(statuses, resp.StatusCode)
		_ = resp.Body.Close()
	}

	if !reflect.DeepEqual(statuses, []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests}) {
		t.Fatalf("statuses = %v, want [200 200 429]", statuses)
	}
}

func TestCodexUpstreamMockMalformedStreamFrameScenario(t *testing.T) {
	t.Parallel()

	mock := NewCodexUpstreamMock()
	server := httptest.NewServer(mock.Handler())
	defer server.Close()

	resp, err := http.Post(server.URL+"/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4","stream":true,"metadata":{"cpa_mock_scenario":"malformed-stream-frame"}}`))
	if err != nil {
		t.Fatalf("Post error: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "response.created") {
		t.Fatalf("missing response.created: %s", got)
	}
	if !strings.Contains(got, `data: {"type":"response.output_text.delta","delta":"broken"`) {
		t.Fatalf("missing malformed payload marker: %s", got)
	}
}

func TestCodexUpstreamMockReasoningBurstBeforeOutputScenario(t *testing.T) {
	t.Parallel()

	mock := NewCodexUpstreamMock()
	server := httptest.NewServer(mock.Handler())
	defer server.Close()

	resp, err := http.Post(server.URL+"/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4","stream":true,"metadata":{"cpa_mock_scenario":"reasoning-burst-before-output"}}`))
	if err != nil {
		t.Fatalf("Post error: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	got := string(body)
	if strings.Count(got, "response.reasoning_summary_text.delta") < 4 {
		t.Fatalf("reasoning burst too small: %s", got)
	}
	if !strings.Contains(got, "response.output_text.delta") {
		t.Fatalf("missing output delta: %s", got)
	}
	if strings.Index(got, "response.reasoning_summary_text.delta") > strings.Index(got, "response.output_text.delta") {
		t.Fatalf("reasoning burst did not precede output: %s", got)
	}
}

func TestCodexUpstreamMockFragmentedOutputDeltasScenario(t *testing.T) {
	t.Parallel()

	mock := NewCodexUpstreamMock()
	server := httptest.NewServer(mock.Handler())
	defer server.Close()

	resp, err := http.Post(server.URL+"/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4","stream":true,"metadata":{"cpa_mock_scenario":"fragmented-output-deltas"}}`))
	if err != nil {
		t.Fatalf("Post error: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	got := string(body)
	if strings.Count(got, "response.output_text.delta") != 6 {
		t.Fatalf("fragmented delta count = %d, want 6: %s", strings.Count(got, "response.output_text.delta"), got)
	}
	for _, delta := range []string{`"delta":"a"`, `"delta":"n"`, `"delta":"s"`, `"delta":"w"`, `"delta":"e"`, `"delta":"r"`} {
		if !strings.Contains(got, delta) {
			t.Fatalf("missing fragmented delta %s: %s", delta, got)
		}
	}
}

func TestCodexUpstreamMockOversizedCompletedEventScenario(t *testing.T) {
	t.Parallel()

	mock := NewCodexUpstreamMock()
	server := httptest.NewServer(mock.Handler())
	defer server.Close()

	resp, err := http.Post(server.URL+"/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4","stream":true,"metadata":{"cpa_mock_scenario":"oversized-completed-event"}}`))
	if err != nil {
		t.Fatalf("Post error: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "response.completed") {
		t.Fatalf("missing completion event: %s", got)
	}
	if !strings.Contains(got, strings.Repeat("z", 1024)) {
		t.Fatalf("missing oversized completed payload marker")
	}
}

func TestCodexUpstreamMockScenarioCatalogIncludesExtremeCases(t *testing.T) {
	t.Parallel()

	mock := NewCodexUpstreamMock()
	server := httptest.NewServer(mock.Handler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/_mock/scenarios")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	got := string(body)
	for _, name := range []string{
		"reasoning-burst-before-output",
		"fragmented-output-deltas",
		"oversized-completed-event",
		"malformed-stream-frame",
		"keepalive-without-events",
	} {
		if !strings.Contains(got, name) {
			t.Fatalf("scenario catalog missing %q: %s", name, got)
		}
	}
}

func TestCodexUpstreamMockKeepaliveWithoutEventsScenario(t *testing.T) {
	t.Parallel()

	mock := NewCodexUpstreamMock()
	server := httptest.NewServer(mock.Handler())
	defer server.Close()

	resp, err := http.Post(server.URL+"/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4","stream":true,"metadata":{"cpa_mock_scenario":"keepalive-without-events"}}`))
	if err != nil {
		t.Fatalf("Post error: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, ": keepalive") {
		t.Fatalf("missing keepalive comments: %s", got)
	}
	if strings.Contains(got, "response.output_text.delta") {
		t.Fatalf("unexpected output delta in keepalive-only stream: %s", got)
	}

	stats := mock.Snapshot()
	if stats.Scenarios["keepalive-without-events"].Requests != 1 {
		t.Fatalf("scenario requests = %d, want 1", stats.Scenarios["keepalive-without-events"].Requests)
	}
}
