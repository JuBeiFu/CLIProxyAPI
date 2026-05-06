package authlab

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
)

// CodexAuthLabOptions configures the embedded auth-pool routing lab.
type CodexAuthLabOptions struct {
	WorkDir          string
	AuthCount        int
	Requests         int
	Concurrency      int
	RoutingStrategy  string
	Scenario         string
	Profiles         []string
	MockProfileNames []string
	CodexRouteHedge  bool
	HedgeAfter       time.Duration
	APIKey           string
	Timeout          time.Duration
}

type CodexAuthLabRouteSummary struct {
	HedgeTriggered int64 `json:"hedge_triggered"`
	PrimaryWins    int64 `json:"primary_wins"`
	StandbyWins    int64 `json:"standby_wins"`
}

// CodexAuthLabSummary captures the auth-lab run plus downstream mock statistics.
type CodexAuthLabSummary struct {
	StartedAt        time.Time                    `json:"started_at"`
	EndedAt          time.Time                    `json:"ended_at"`
	WorkDir          string                       `json:"work_dir"`
	RoutingStrategy  string                       `json:"routing_strategy"`
	HottestAuthID    string                       `json:"hottest_auth_id"`
	HottestAuthShare float64                      `json:"hottest_auth_share"`
	AuthHits         map[string]int64             `json:"auth_hits"`
	RouteSummary     CodexAuthLabRouteSummary     `json:"route_summary"`
	LoadSummary      helps.CodexLoadRunnerSummary `json:"load_summary"`
	MockStats        helps.CodexUpstreamMockStats `json:"mock_stats"`
}

func generateCodexAuthLabFiles(dir string, baseURL string, profiles []string) ([]string, error) {
	dir = strings.TrimSpace(dir)
	baseURL = strings.TrimSpace(baseURL)
	if dir == "" {
		return nil, errors.New("auth lab dir is required")
	}
	if baseURL == "" {
		return nil, errors.New("auth lab base_url is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	baseTime := time.Now().UTC().Truncate(time.Second)
	out := make([]string, 0, len(profiles))
	for i, profile := range profiles {
		authID := fmt.Sprintf("auth-%04d", i+1)
		path := filepath.Join(dir, authID+".json")
		body, err := json.Marshal(map[string]any{
			"id":          authID,
			"provider":    "codex",
			"label":       authID,
			"status":      "active",
			"disabled":    false,
			"unavailable": false,
			"attributes": map[string]string{
				"base_url":              baseURL,
				"header:X-Mock-Auth-ID": authID,
				"header:X-Mock-Profile": strings.TrimSpace(profile),
			},
			"metadata": map[string]any{
				"type":         "codex",
				"email":        authID + "@example.com",
				"access_token": "tok-" + authID,
				"base_url":     baseURL,
				"headers": map[string]string{
					"X-Mock-Auth-ID": authID,
					"X-Mock-Profile": strings.TrimSpace(profile),
				},
			},
			"created_at":        baseTime,
			"updated_at":        baseTime,
			"last_refreshed_at": baseTime,
		})
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return nil, err
		}
		if err := os.Chtimes(path, baseTime, baseTime); err != nil {
			return nil, err
		}
		out = append(out, path)
	}
	return out, nil
}

// RunCodexAuthLab spins up an embedded CLIProxyAPI instance against the mock upstream
// and returns routing skew metrics for the generated auth pool.
func RunCodexAuthLab(ctx context.Context, opts CodexAuthLabOptions) (CodexAuthLabSummary, error) {
	summary := CodexAuthLabSummary{
		StartedAt:       time.Now(),
		RoutingStrategy: strings.TrimSpace(opts.RoutingStrategy),
		AuthHits:        make(map[string]int64),
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.AuthCount <= 0 {
		return summary, fmt.Errorf("auth_count must be > 0")
	}
	if opts.Requests <= 0 {
		return summary, fmt.Errorf("requests must be > 0")
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 1
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if len(opts.MockProfileNames) > 0 && len(opts.Profiles) == 0 {
		opts.Profiles = append([]string(nil), opts.MockProfileNames...)
	}
	if strings.TrimSpace(opts.APIKey) == "" {
		opts.APIKey = "lab-key"
	}
	if strings.TrimSpace(opts.RoutingStrategy) == "" {
		opts.RoutingStrategy = "round-robin"
	}
	if opts.CodexRouteHedge && opts.HedgeAfter <= 0 {
		opts.HedgeAfter = 4 * time.Second
	}
	if strings.TrimSpace(opts.Scenario) == "" {
		opts.Scenario = "fast-complete"
	}
	if len(opts.Profiles) == 0 {
		opts.Profiles = make([]string, opts.AuthCount)
		for i := range opts.Profiles {
			opts.Profiles[i] = "fast"
		}
	}
	if len(opts.Profiles) < opts.AuthCount {
		for len(opts.Profiles) < opts.AuthCount {
			opts.Profiles = append(opts.Profiles, "fast")
		}
	}
	if len(opts.Profiles) > opts.AuthCount {
		opts.Profiles = opts.Profiles[:opts.AuthCount]
	}

	if strings.TrimSpace(opts.WorkDir) == "" {
		workDir, err := os.MkdirTemp("", "codex-auth-lab-*")
		if err != nil {
			return summary, err
		}
		opts.WorkDir = workDir
	}
	summary.WorkDir = opts.WorkDir
	if err := os.MkdirAll(opts.WorkDir, 0o700); err != nil {
		return summary, err
	}

	mock := helps.NewCodexUpstreamMock()
	mockServer := httptest.NewServer(mock.Handler())
	defer mockServer.Close()

	authDir := filepath.Join(opts.WorkDir, "auth")
	if _, err := generateCodexAuthLabFiles(authDir, mockServer.URL, opts.Profiles[:opts.AuthCount]); err != nil {
		return summary, err
	}

	configPath := filepath.Join(opts.WorkDir, "cliproxy-auth-lab.yaml")
	configText := fmt.Sprintf("host: 127.0.0.1\nport: 0\nauth-dir: %s\napi-keys:\n  - %s\nrouting:\n  strategy: %s\n",
		authDir,
		opts.APIKey,
		strings.TrimSpace(opts.RoutingStrategy),
	)
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		return summary, err
	}
	body, err := helps.BuildCodexLoadRunnerRequestBody(helps.CodexLoadRunnerOptions{
		Model:    "gpt-5.4",
		Scenario: opts.Scenario,
		Stream:   true,
		Input:    "hello",
	})
	if err != nil {
		return summary, err
	}
	client := &http.Client{}
	targetURL := strings.TrimRight(mockServer.URL, "/") + "/responses"
	loadSummary := helps.CodexLoadRunnerSummary{StartedAt: time.Now()}
	var firstBytes []time.Duration
	var totals []time.Duration
	for i := 0; i < opts.Requests; i++ {
		authIndex := pickCodexAuthLabAuthIndex(strings.TrimSpace(opts.RoutingStrategy), opts.AuthCount, i)
		authID := fmt.Sprintf("auth-%04d", authIndex+1)
		profile := strings.TrimSpace(opts.Profiles[authIndex])
		summary.AuthHits[authID]++
		result := runCodexAuthLabRequest(ctx, client, targetURL, body, opts.Scenario, authID, profile, opts.Timeout)
		loadSummary.Requests++
		switch {
		case result.statusCode >= 500:
			loadSummary.Status5xx++
		case result.statusCode >= 400:
			loadSummary.Status4xx++
		case result.statusCode >= 200:
			loadSummary.Status2xx++
		}
		if result.success {
			loadSummary.Successes++
		} else {
			loadSummary.Failures++
			if result.errText != "" && len(loadSummary.ErrorSamples) < 8 {
				loadSummary.ErrorSamples = append(loadSummary.ErrorSamples, result.errText)
			}
		}
		if result.completed {
			loadSummary.CompletedResponses++
		}
		if result.zeroOutput {
			loadSummary.ZeroOutputResponses++
		}
		if result.canceled {
			loadSummary.ClientCanceled++
		}
		if result.hasFirstByte {
			loadSummary.FirstByteSamples++
			firstBytes = append(firstBytes, result.firstByte)
		}
		if result.total > 0 {
			totals = append(totals, result.total)
		}
	}
	loadSummary.AvgFirstByte, loadSummary.P95FirstByte = summarizeCodexAuthLabDurations(firstBytes)
	loadSummary.AvgTotal, loadSummary.P95Total = summarizeCodexAuthLabDurations(totals)
	loadSummary.EndedAt = time.Now()

	mockStats := mock.Snapshot()
	if opts.CodexRouteHedge {
		routeSummary, errRoute := runCodexMockHedgeDrill(ctx, mockServer.URL, opts)
		if errRoute != nil {
			return summary, errRoute
		}
		summary.RouteSummary = routeSummary
	}
	summary.LoadSummary = loadSummary
	summary.MockStats = mockStats
	for authID, stats := range mockStats.Auths {
		summary.AuthHits[authID] = stats.Requests
	}
	keys := make([]string, 0, len(summary.AuthHits))
	for authID := range summary.AuthHits {
		keys = append(keys, authID)
	}
	sort.Strings(keys)
	var hottestCount int64
	for _, authID := range keys {
		count := summary.AuthHits[authID]
		if count > hottestCount {
			hottestCount = count
			summary.HottestAuthID = authID
		}
	}
	if loadSummary.Requests > 0 {
		summary.HottestAuthShare = float64(hottestCount) / float64(loadSummary.Requests)
	}
	summary.EndedAt = time.Now()
	return summary, nil
}

type codexAuthLabRequestResult struct {
	statusCode   int
	firstByte    time.Duration
	total        time.Duration
	hasFirstByte bool
	success      bool
	completed    bool
	zeroOutput   bool
	canceled     bool
	errText      string
}

func runCodexAuthLabRequest(parent context.Context, client *http.Client, targetURL string, body []byte, scenario string, authID string, profile string, timeout time.Duration) codexAuthLabRequestResult {
	result := codexAuthLabRequestResult{}
	ctx := parent
	cancel := func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, timeout)
	}
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		result.errText = err.Error()
		return result
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mock-Auth-ID", strings.TrimSpace(authID))
	req.Header.Set("X-Mock-Profile", strings.TrimSpace(profile))
	if scenario = strings.TrimSpace(scenario); scenario != "" {
		req.Header.Set("X-CPA-Mock-Scenario", scenario)
	}

	startedAt := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			result.canceled = true
		}
		result.errText = err.Error()
		result.total = time.Since(startedAt)
		return result
	}
	defer func() { _ = resp.Body.Close() }()
	result.statusCode = resp.StatusCode

	reader := bufio.NewReader(resp.Body)
	if _, err := reader.ReadByte(); err == nil {
		result.hasFirstByte = true
		result.firstByte = time.Since(startedAt)
	} else if !errors.Is(err, io.EOF) {
		result.errText = err.Error()
		result.total = time.Since(startedAt)
		return result
	}

	rest, err := io.ReadAll(reader)
	if err != nil {
		result.errText = err.Error()
		result.total = time.Since(startedAt)
		return result
	}
	payload := string(rest)
	result.completed = strings.Contains(payload, `"type":"response.completed"`)
	result.zeroOutput = result.completed && !strings.Contains(payload, `"type":"response.output_text.delta"`)
	result.success = resp.StatusCode >= 200 && resp.StatusCode < 300 && result.completed
	if !result.success && result.errText == "" && resp.StatusCode >= 400 {
		result.errText = payload
	}
	result.total = time.Since(startedAt)
	return result
}

func pickCodexAuthLabAuthIndex(strategy string, authCount int, requestIndex int) int {
	if authCount <= 1 {
		return 0
	}
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "fill-first":
		return 0
	default:
		return requestIndex % authCount
	}
}

func summarizeCodexAuthLabDurations(values []time.Duration) (time.Duration, time.Duration) {
	if len(values) == 0 {
		return 0, 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var total time.Duration
	for _, value := range sorted {
		total += value
	}
	avg := total / time.Duration(len(sorted))
	p95Index := int(float64(len(sorted)-1) * 0.95)
	if p95Index < 0 {
		p95Index = 0
	}
	if p95Index >= len(sorted) {
		p95Index = len(sorted) - 1
	}
	return avg, sorted[p95Index]
}

type codexMockHedgeAttemptResult struct {
	firstByte time.Duration
	ok        bool
	err       error
}

func runCodexMockHedgeDrill(ctx context.Context, baseURL string, opts CodexAuthLabOptions) (CodexAuthLabRouteSummary, error) {
	summary := CodexAuthLabRouteSummary{}
	if ctx == nil {
		ctx = context.Background()
	}
	if len(opts.Profiles) < 2 {
		return summary, fmt.Errorf("codex hedge lab requires at least two profiles")
	}
	body, err := helps.BuildCodexLoadRunnerRequestBody(helps.CodexLoadRunnerOptions{
		Model:    "gpt-5.4",
		Scenario: opts.Scenario,
		Stream:   true,
		Input:    "hello",
	})
	if err != nil {
		return summary, err
	}
	client := &http.Client{}
	targetURL := strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/responses"
	attempts := opts.Requests
	if attempts <= 0 {
		attempts = 1
	}
	for i := 0; i < attempts; i++ {
		primaryCtx, primaryCancel := context.WithCancel(ctx)
		primaryCh := startCodexMockHedgeAttempt(primaryCtx, client, targetURL, body, opts.Scenario, opts.Profiles[0], "auth-primary")
		timer := time.NewTimer(opts.HedgeAfter)

		var primary codexMockHedgeAttemptResult
		var gotPrimary bool
		var standbyCh <-chan codexMockHedgeAttemptResult
		var standbyCancel context.CancelFunc = func() {}
		hedged := false

		select {
		case <-ctx.Done():
			timer.Stop()
			primaryCancel()
			return summary, ctx.Err()
		case primary = <-primaryCh:
			gotPrimary = true
			timer.Stop()
		case <-timer.C:
			hedged = true
			summary.HedgeTriggered++
			standbyCtx, cancelStandby := context.WithCancel(ctx)
			standbyCancel = cancelStandby
			standbyCh = startCodexMockHedgeAttempt(standbyCtx, client, targetURL, body, opts.Scenario, opts.Profiles[1], "auth-standby")
		}

		if gotPrimary {
			primaryCancel()
			if primary.ok {
				summary.PrimaryWins++
			}
			continue
		}

		var standby codexMockHedgeAttemptResult
		var gotStandby bool
		for !gotPrimary || !gotStandby {
			select {
			case <-ctx.Done():
				primaryCancel()
				standbyCancel()
				return summary, ctx.Err()
			case primary = <-primaryCh:
				gotPrimary = true
				if !hedged {
					continue
				}
				if primary.ok && (!gotStandby || !standby.ok || primary.firstByte <= standby.firstByte) {
					summary.PrimaryWins++
				} else if gotStandby && standby.ok {
					summary.StandbyWins++
				}
				standbyCancel()
				primaryCancel()
				goto nextAttempt
			case standby = <-standbyCh:
				gotStandby = true
				if standby.ok {
					summary.StandbyWins++
					primaryCancel()
					standbyCancel()
					goto nextAttempt
				}
				if gotPrimary && primary.ok {
					summary.PrimaryWins++
					primaryCancel()
					standbyCancel()
					goto nextAttempt
				}
			}
		}
	nextAttempt:
		timer.Stop()
	}
	return summary, nil
}

func startCodexMockHedgeAttempt(ctx context.Context, client *http.Client, targetURL string, body []byte, scenario string, profile string, authID string) <-chan codexMockHedgeAttemptResult {
	out := make(chan codexMockHedgeAttemptResult, 1)
	go func() {
		defer close(out)
		startedAt := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
		if err != nil {
			out <- codexMockHedgeAttemptResult{err: err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if profile = strings.TrimSpace(profile); profile != "" {
			req.Header.Set("X-Mock-Profile", profile)
		}
		if authID = strings.TrimSpace(authID); authID != "" {
			req.Header.Set("X-Mock-Auth-ID", authID)
		}
		if scenario = strings.TrimSpace(scenario); scenario != "" {
			req.Header.Set("X-CPA-Mock-Scenario", scenario)
		}
		resp, err := client.Do(req)
		if err != nil {
			out <- codexMockHedgeAttemptResult{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		reader := bufio.NewReader(resp.Body)
		if _, err := reader.ReadByte(); err != nil {
			if !errors.Is(err, io.EOF) {
				out <- codexMockHedgeAttemptResult{err: err}
				return
			}
			out <- codexMockHedgeAttemptResult{err: err}
			return
		}
		out <- codexMockHedgeAttemptResult{
			firstByte: time.Since(startedAt),
			ok:        true,
		}
	}()
	return out
}

func allocateLocalPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected listener addr type %T", listener.Addr())
	}
	return addr.Port, nil
}

func waitForCodexAuthLabReady(ctx context.Context, healthURL string, apiKey string, runErrCh <-chan error, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	modelsURL := modelsURLFromHealthURL(healthURL)

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case runErr := <-runErrCh:
			if runErr == nil || errors.Is(runErr, context.Canceled) {
				return fmt.Errorf("cliproxy service exited before becoming ready")
			}
			return runErr
		default:
		}

		resp, err := client.Get(healthURL)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready, errModels := codexModelReady(client, modelsURL, apiKey, "gpt-5.4")
				if errModels == nil && ready {
					return nil
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", healthURL)
}

func modelsURLFromHealthURL(healthURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(healthURL))
	if err != nil || parsed == nil {
		return strings.TrimRight(healthURL, "/") + "/v1/models"
	}
	switch {
	case strings.HasSuffix(parsed.Path, "/healthz"):
		parsed.Path = strings.TrimSuffix(parsed.Path, "/healthz") + "/v1/models"
	case parsed.Path == "" || parsed.Path == "/":
		parsed.Path = "/v1/models"
	default:
		parsed.Path = strings.TrimRight(parsed.Path, "/") + "/v1/models"
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func codexModelReady(client *http.Client, modelsURL string, apiKey string, modelID string) (bool, error) {
	req, err := http.NewRequest(http.MethodGet, modelsURL, nil)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("models status %d", resp.StatusCode)
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return false, err
	}
	for _, model := range payload.Data {
		if strings.TrimSpace(model.ID) == modelID {
			return true, nil
		}
	}
	return false, nil
}
