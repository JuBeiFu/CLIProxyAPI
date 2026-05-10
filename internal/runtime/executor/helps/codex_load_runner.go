package helps

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/proxyutil"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CodexLoadRunnerOptions configures load generation against a /responses-style endpoint.
type CodexLoadRunnerOptions struct {
	TargetURL    string
	APIKey       string
	Model        string
	Scenario     string
	Stream       bool
	Requests     int
	Concurrency  int
	Timeout      time.Duration
	CancelAfter  time.Duration
	Input        string
	RequestInput string
	RequestBody  []byte
	Headers      map[string]string
}

// CodexLoadRunnerSummary summarizes one load run.
type CodexLoadRunnerSummary struct {
	StartedAt                   time.Time     `json:"started_at"`
	EndedAt                     time.Time     `json:"ended_at"`
	Requests                    int64         `json:"requests"`
	Successes                   int64         `json:"successes"`
	Failures                    int64         `json:"failures"`
	CompletedResponses          int64         `json:"completed_responses"`
	ZeroOutputResponses         int64         `json:"zero_output_responses"`
	ClientCanceled              int64         `json:"client_canceled"`
	FirstByteSamples            int64         `json:"first_byte_samples"`
	FirstVisibleOutputSamples   int64         `json:"first_visible_output_samples"`
	Status2xx                   int64         `json:"status_2xx"`
	Status4xx                   int64         `json:"status_4xx"`
	Status5xx                   int64         `json:"status_5xx"`
	AvgFirstByte                time.Duration `json:"avg_first_byte"`
	P95FirstByte                time.Duration `json:"p95_first_byte"`
	AvgFirstVisibleOutput       time.Duration `json:"avg_first_visible_output"`
	P95FirstVisibleOutput       time.Duration `json:"p95_first_visible_output"`
	AvgVisibleAfterFirstByteGap time.Duration `json:"avg_visible_after_first_byte_gap"`
	P95VisibleAfterFirstByteGap time.Duration `json:"p95_visible_after_first_byte_gap"`
	AvgTotal                    time.Duration `json:"avg_total"`
	P95Total                    time.Duration `json:"p95_total"`
	ErrorSamples                []string      `json:"error_samples,omitempty"`
}

type codexLoadRunnerResult struct {
	statusCode            int
	firstByte             time.Duration
	firstVisibleOutput    time.Duration
	total                 time.Duration
	hasFirstByte          bool
	hasFirstVisibleOutput bool
	success               bool
	completed             bool
	zeroOutput            bool
	canceled              bool
	errText               string
}

// BuildCodexLoadRunnerRequestBody builds a minimal OpenAI Responses-style request body.
func BuildCodexLoadRunnerRequestBody(opts CodexLoadRunnerOptions) ([]byte, error) {
	if len(opts.RequestBody) > 0 {
		return bytes.Clone(opts.RequestBody), nil
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = "gpt-5.4"
	}
	input := strings.TrimSpace(opts.Input)
	if input == "" {
		input = strings.TrimSpace(opts.RequestInput)
	}
	if input == "" {
		input = "hello"
	}

	body := []byte(`{}`)
	var err error
	body, err = sjson.SetBytes(body, "model", model)
	if err != nil {
		return nil, err
	}
	body, err = sjson.SetBytes(body, "input", input)
	if err != nil {
		return nil, err
	}
	body, err = sjson.SetBytes(body, "stream", opts.Stream)
	if err != nil {
		return nil, err
	}
	if scenario := strings.TrimSpace(opts.Scenario); scenario != "" {
		body, err = sjson.SetBytes(body, "metadata.cpa_mock_scenario", scenario)
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

// RunCodexLoad executes the configured request load and returns aggregate metrics.
func RunCodexLoad(ctx context.Context, opts CodexLoadRunnerOptions) (CodexLoadRunnerSummary, error) {
	summary := CodexLoadRunnerSummary{StartedAt: time.Now()}
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(opts.TargetURL) == "" {
		return summary, errors.New("target_url is required")
	}
	if opts.Requests <= 0 {
		return summary, errors.New("requests must be > 0")
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 1
	}
	if opts.Concurrency > opts.Requests {
		opts.Concurrency = opts.Requests
	}

	body, err := BuildCodexLoadRunnerRequestBody(opts)
	if err != nil {
		return summary, err
	}

	client := &http.Client{Transport: proxyutil.NewInheritedTransport()}
	results := make(chan codexLoadRunnerResult, opts.Requests)
	jobs := make(chan int)
	var workers sync.WaitGroup

	for i := 0; i < opts.Concurrency; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range jobs {
				results <- runCodexLoadOnce(ctx, client, opts, body)
			}
		}()
	}

	go func() {
		for i := 0; i < opts.Requests; i++ {
			jobs <- i
		}
		close(jobs)
		workers.Wait()
		close(results)
	}()

	var firstBytes []time.Duration
	var firstVisibleOutputs []time.Duration
	var visibleAfterFirstByteGaps []time.Duration
	var totals []time.Duration
	for result := range results {
		summary.Requests++
		switch {
		case result.statusCode >= 500:
			summary.Status5xx++
		case result.statusCode >= 400:
			summary.Status4xx++
		case result.statusCode >= 200:
			summary.Status2xx++
		}
		if result.success {
			summary.Successes++
		} else {
			summary.Failures++
			if result.errText != "" && len(summary.ErrorSamples) < 8 {
				summary.ErrorSamples = append(summary.ErrorSamples, result.errText)
			}
		}
		if result.completed {
			summary.CompletedResponses++
		}
		if result.zeroOutput {
			summary.ZeroOutputResponses++
		}
		if result.canceled {
			summary.ClientCanceled++
		}
		if result.hasFirstByte {
			summary.FirstByteSamples++
			firstBytes = append(firstBytes, result.firstByte)
		}
		if result.hasFirstVisibleOutput {
			summary.FirstVisibleOutputSamples++
			firstVisibleOutputs = append(firstVisibleOutputs, result.firstVisibleOutput)
			if result.hasFirstByte && result.firstVisibleOutput >= result.firstByte {
				visibleAfterFirstByteGaps = append(visibleAfterFirstByteGaps, result.firstVisibleOutput-result.firstByte)
			}
		}
		if result.total > 0 {
			totals = append(totals, result.total)
		}
	}

	summary.AvgFirstByte, summary.P95FirstByte = summarizeDurations(firstBytes)
	summary.AvgFirstVisibleOutput, summary.P95FirstVisibleOutput = summarizeDurations(firstVisibleOutputs)
	summary.AvgVisibleAfterFirstByteGap, summary.P95VisibleAfterFirstByteGap = summarizeDurations(visibleAfterFirstByteGaps)
	summary.AvgTotal, summary.P95Total = summarizeDurations(totals)
	summary.EndedAt = time.Now()
	return summary, nil
}

func runCodexLoadOnce(parent context.Context, client *http.Client, opts CodexLoadRunnerOptions, body []byte) codexLoadRunnerResult {
	result := codexLoadRunnerResult{}
	ctx := parent
	cancel := func() {}
	if opts.Timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, opts.Timeout)
	}
	defer cancel()

	if opts.CancelAfter > 0 {
		var cancelRun context.CancelFunc
		ctx, cancelRun = context.WithCancel(ctx)
		timer := time.AfterFunc(opts.CancelAfter, cancelRun)
		defer timer.Stop()
		defer cancelRun()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.TargetURL, bytes.NewReader(body))
	if err != nil {
		result.errText = err.Error()
		return result
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(opts.APIKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(opts.APIKey))
	}
	if opts.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	for key, value := range opts.Headers {
		if strings.TrimSpace(key) == "" {
			continue
		}
		req.Header.Set(key, value)
	}

	startedAt := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		result.total = time.Since(startedAt)
		result.canceled = ctx.Err() != nil
		result.errText = err.Error()
		return result
	}
	defer func() { _ = resp.Body.Close() }()

	result.statusCode = resp.StatusCode
	result.firstByte = time.Since(startedAt)
	result.hasFirstByte = true

	if opts.Stream {
		return finishCodexLoadStreamResult(startedAt, resp, result)
	}
	return finishCodexLoadNonStreamResult(startedAt, resp, result)
}

func finishCodexLoadStreamResult(startedAt time.Time, resp *http.Response, result codexLoadRunnerResult) codexLoadRunnerResult {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(nil, 8*1024*1024)

	sawUserOutput := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		if eventHasUserOutput([]byte(data)) {
			sawUserOutput = true
			if !result.hasFirstVisibleOutput {
				result.firstVisibleOutput = time.Since(startedAt)
				result.hasFirstVisibleOutput = true
			}
		}
		if isCompletedEvent([]byte(data)) {
			result.completed = true
			result.zeroOutput = !sawUserOutput && !completedEventHasVisibleOutput([]byte(data))
		}
	}
	result.total = time.Since(startedAt)
	if err := scanner.Err(); err != nil {
		result.canceled = result.canceled || strings.Contains(strings.ToLower(err.Error()), "context canceled")
		result.errText = err.Error()
		return result
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && result.completed {
		result.success = true
		return result
	}
	if result.errText == "" {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			result.errText = "stream ended before response.completed"
		} else {
			result.errText = "non-success status"
		}
	}
	return result
}

func finishCodexLoadNonStreamResult(startedAt time.Time, resp *http.Response, result codexLoadRunnerResult) codexLoadRunnerResult {
	body, err := io.ReadAll(resp.Body)
	result.total = time.Since(startedAt)
	if err != nil {
		result.errText = err.Error()
		result.canceled = result.canceled || strings.Contains(strings.ToLower(err.Error()), "context canceled")
		return result
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.errText = summarizeBodyError(body)
		return result
	}

	result.completed = strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "status").String()), "completed")
	result.zeroOutput = !nonStreamBodyHasVisibleOutput(body)
	if !result.zeroOutput {
		result.firstVisibleOutput = result.total
		result.hasFirstVisibleOutput = true
	}
	result.success = true
	return result
}

func summarizeDurations(values []time.Duration) (avg time.Duration, p95 time.Duration) {
	if len(values) == 0 {
		return 0, 0
	}
	ordered := append([]time.Duration(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	var total time.Duration
	for _, value := range ordered {
		total += value
	}
	avg = total / time.Duration(len(ordered))
	index := int(float64(len(ordered)-1) * 0.95)
	if index < 0 {
		index = 0
	}
	if index >= len(ordered) {
		index = len(ordered) - 1
	}
	return avg, ordered[index]
}

func summarizeBodyError(body []byte) string {
	if message := strings.TrimSpace(gjson.GetBytes(body, "error.message").String()); message != "" {
		return message
	}
	if len(body) == 0 {
		return "empty error body"
	}
	return string(body)
}

func isCompletedEvent(data []byte) bool {
	return strings.EqualFold(strings.TrimSpace(gjson.GetBytes(data, "type").String()), "response.completed")
}

func eventHasUserOutput(data []byte) bool {
	eventType := strings.TrimSpace(gjson.GetBytes(data, "type").String())
	switch eventType {
	case "response.output_text.delta":
		return strings.TrimSpace(gjson.GetBytes(data, "delta").String()) != ""
	case "response.output_item.done":
		return outputItemHasVisibleOutput(gjson.GetBytes(data, "item"))
	case "response.completed":
		return completedEventHasVisibleOutput(data)
	default:
		return false
	}
}

func completedEventHasVisibleOutput(data []byte) bool {
	for _, item := range gjson.GetBytes(data, "response.output").Array() {
		if outputItemHasVisibleOutput(item) {
			return true
		}
	}
	return false
}

func outputItemHasVisibleOutput(item gjson.Result) bool {
	if !item.Exists() {
		return false
	}
	switch strings.TrimSpace(item.Get("type").String()) {
	case "message":
		for _, content := range item.Get("content").Array() {
			if strings.TrimSpace(content.Get("text").String()) != "" {
				return true
			}
		}
		return false
	case "function_call":
		return strings.TrimSpace(item.Get("call_id").String()) != "" ||
			strings.TrimSpace(item.Get("name").String()) != "" ||
			strings.TrimSpace(item.Get("arguments").String()) != ""
	default:
		return false
	}
}

func nonStreamBodyHasVisibleOutput(body []byte) bool {
	for _, item := range gjson.GetBytes(body, "output").Array() {
		if outputItemHasVisibleOutput(item) {
			return true
		}
	}
	for _, choice := range gjson.GetBytes(body, "choices").Array() {
		if strings.TrimSpace(choice.Get("message.content").String()) != "" {
			return true
		}
	}
	return false
}
