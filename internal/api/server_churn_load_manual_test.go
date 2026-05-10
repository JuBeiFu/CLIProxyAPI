//go:build manualbench

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

type manualChurnExecutor struct{}

const manualChurnModel = "gpt-5.4"

var manualRetryWindowState = newManualRetryWindowState()

func (manualChurnExecutor) Identifier() string { return "codex" }

func (manualChurnExecutor) Execute(_ context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	scenario := manualScenarioFromRequest(req)
	if manualRetryWindowState.shouldInject(scenario, auth) {
		return cliproxyexecutor.Response{}, &coreauth.Error{Code: "request_scoped_auth_unavailable", Message: "manual retry window", HTTPStatus: http.StatusServiceUnavailable}
	}
	if manualScenarioHasReasoningDelay(scenario) {
		time.Sleep(8 * time.Millisecond)
	}
	return cliproxyexecutor.Response{
		Payload: []byte(`{"id":"resp_manual_bench","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`),
	}, nil
}

func (manualChurnExecutor) ExecuteStream(_ context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	scenario := manualScenarioFromRequest(req)
	if manualRetryWindowState.shouldInject(scenario, auth) {
		return nil, &coreauth.Error{Code: "request_scoped_auth_unavailable", Message: "manual retry window", HTTPStatus: http.StatusServiceUnavailable}
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 4)
	go func() {
		defer close(chunks)
		switch scenario {
		case "reasoning-before-output", "reasoning-before-output-retry-window":
			chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.created\"}\n\n")}
			time.Sleep(15 * time.Millisecond)
			chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")}
			time.Sleep(4 * time.Millisecond)
		default:
			chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.created\"}\n\n")}
			chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")}
		}
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_manual_bench\",\"object\":\"response\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")}
	}()
	return &cliproxyexecutor.StreamResult{Headers: http.Header{}, Chunks: chunks}, nil
}

func (manualChurnExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (manualChurnExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (manualChurnExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func manualScenarioFromRequest(req cliproxyexecutor.Request) string {
	scenario := strings.TrimSpace(gjson.GetBytes(req.Payload, "metadata.cpa_mock_scenario").String())
	if scenario == "" {
		return "fast-complete"
	}
	return scenario
}

func manualScenarioHasReasoningDelay(scenario string) bool {
	scenario = strings.ToLower(strings.TrimSpace(scenario))
	return strings.HasPrefix(scenario, "reasoning-before-output")
}

func TestManualServerChurnLoadCPA(t *testing.T) {
	const (
		totalAuths   = 256
		churnAuths   = 16
		requestCount = 1024
		concurrency  = 64
	)

	restoreLogger := muteManualBenchLogger()
	defer restoreLogger()

	testCases := []struct {
		name     string
		scenario string
		stream   bool
		mutator  func(context.Context, *coreauth.Manager, []string)
	}{
		{
			name:     "static-fast-stream",
			scenario: "fast-complete",
			stream:   true,
		},
		{
			name:     "cooldown-churn-fast-stream",
			scenario: "fast-complete",
			stream:   true,
			mutator:  runCooldownChurn,
		},
		{
			name:     "disable-churn-fast-stream",
			scenario: "fast-complete",
			stream:   true,
			mutator:  runDisableChurn,
		},
		{
			name:     "remove-readd-churn-fast-stream",
			scenario: "fast-complete",
			stream:   true,
			mutator:  runRemoveReaddChurn,
		},
		{
			name:     "remove-readd-retry-fast-stream",
			scenario: "fast-complete-retry-window",
			stream:   true,
			mutator:  runRemoveReaddChurn,
		},
		{
			name:     "static-reasoning-stream",
			scenario: "reasoning-before-output",
			stream:   true,
		},
		{
			name:     "cooldown-churn-reasoning-stream",
			scenario: "reasoning-before-output",
			stream:   true,
			mutator:  runCooldownChurn,
		},
		{
			name:     "disable-churn-reasoning-stream",
			scenario: "reasoning-before-output",
			stream:   true,
			mutator:  runDisableChurn,
		},
		{
			name:     "remove-readd-churn-reasoning-stream",
			scenario: "reasoning-before-output",
			stream:   true,
			mutator:  runRemoveReaddChurn,
		},
		{
			name:     "remove-readd-retry-reasoning-stream",
			scenario: "reasoning-before-output-retry-window",
			stream:   true,
			mutator:  runRemoveReaddChurn,
		},
	}

	results := make(map[string]helps.CodexLoadRunnerSummary, len(testCases))

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			manualRetryWindowState.reset()
			server, manager, authIDs := manualChurnServerSetup(t, totalAuths)
			httpServer := httptest.NewServer(server.engine)
			defer httpServer.Close()

			runCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.mutator != nil {
				go tc.mutator(runCtx, manager, authIDs[:churnAuths])
			}

			summary, err := helps.RunCodexLoad(context.Background(), helps.CodexLoadRunnerOptions{
				TargetURL:   httpServer.URL + "/v1/responses",
				Model:       "gpt-5.4",
				Scenario:    tc.scenario,
				Stream:      tc.stream,
				Requests:    requestCount,
				Concurrency: concurrency,
				Timeout:     15 * time.Second,
				Input:       "hello",
			})
			cancel()
			if err != nil {
				t.Fatalf("RunCodexLoad error: %v", err)
			}
			if summary.Failures != 0 {
				body, _ := json.Marshal(summary)
				t.Fatalf("unexpected failures=%d summary=%s", summary.Failures, string(body))
			}
			results[tc.name] = summary
			t.Log(manualSummaryLine(tc.name, summary))
		})
	}

	t.Log(manualDeltaLine("static-fast-stream", "cooldown-churn-fast-stream", results))
	t.Log(manualDeltaLine("static-fast-stream", "disable-churn-fast-stream", results))
	t.Log(manualDeltaLine("static-fast-stream", "remove-readd-churn-fast-stream", results))
	t.Log(manualDeltaLine("remove-readd-churn-fast-stream", "remove-readd-retry-fast-stream", results))
	t.Log(manualDeltaLine("static-reasoning-stream", "cooldown-churn-reasoning-stream", results))
	t.Log(manualDeltaLine("static-reasoning-stream", "disable-churn-reasoning-stream", results))
	t.Log(manualDeltaLine("static-reasoning-stream", "remove-readd-churn-reasoning-stream", results))
	t.Log(manualDeltaLine("remove-readd-churn-reasoning-stream", "remove-readd-retry-reasoning-stream", results))
}

func manualChurnServerSetup(t *testing.T, total int) (*Server, *coreauth.Manager, []string) {
	t.Helper()

	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(manualChurnExecutor{})

	modelRegistry := registry.GetGlobalRegistry()
	authIDs := make([]string, 0, total)
	for index := 0; index < total; index++ {
		authID := "manual-churn-codex-" + strconv.Itoa(index)
		auth := manualChurnAuth(authID)
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s) error = %v", authID, err)
		}
		modelRegistry.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: manualChurnModel}})
		authIDs = append(authIDs, authID)
	}
	t.Cleanup(func() {
		for _, authID := range authIDs {
			modelRegistry.UnregisterClient(authID)
		}
	})

	cfg := &config.Config{}
	return NewServer(cfg, manager, sdkaccess.NewManager(), "manual-churn-bench-config.yaml"), manager, authIDs
}

func runCooldownChurn(ctx context.Context, manager *coreauth.Manager, authIDs []string) {
	manualRunToggleChurn(ctx, manager, authIDs, func(authID string, blocked bool, now time.Time) *coreauth.Auth {
		auth := &coreauth.Auth{
			ID:        authID,
			Provider:  "codex",
			UpdatedAt: now.Add(2 * time.Second),
		}
		if blocked {
			auth.Status = coreauth.StatusError
			auth.Unavailable = true
			auth.NextRetryAfter = now.Add(5 * time.Second)
			return auth
		}
		auth.Status = coreauth.StatusActive
		return auth
	})
}

func runDisableChurn(ctx context.Context, manager *coreauth.Manager, authIDs []string) {
	manualRunToggleChurn(ctx, manager, authIDs, func(authID string, disabled bool, now time.Time) *coreauth.Auth {
		auth := &coreauth.Auth{
			ID:        authID,
			Provider:  "codex",
			Disabled:  disabled,
			UpdatedAt: now.Add(2 * time.Second),
		}
		if disabled {
			auth.Status = coreauth.StatusDisabled
			return auth
		}
		auth.Status = coreauth.StatusActive
		return auth
	})
}

func manualRunToggleChurn(ctx context.Context, manager *coreauth.Manager, authIDs []string, build func(string, bool, time.Time) *coreauth.Auth) {
	if manager == nil || len(authIDs) == 0 || build == nil {
		return
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	index := 0
	blocked := false
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			authID := authIDs[index%len(authIDs)]
			if _, err := manager.Update(context.Background(), build(authID, blocked, now)); err != nil {
				continue
			}
			index++
			if index%len(authIDs) == 0 {
				blocked = !blocked
			}
		}
	}
}

func runRemoveReaddChurn(ctx context.Context, manager *coreauth.Manager, authIDs []string) {
	if manager == nil || len(authIDs) == 0 {
		return
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	modelRegistry := registry.GetGlobalRegistry()
	persistCtx := coreauth.WithSkipPersist(ctx)
	present := make(map[string]bool, len(authIDs))
	for _, authID := range authIDs {
		present[authID] = true
	}

	index := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			authID := authIDs[index%len(authIDs)]
			if present[authID] {
				if _, err := manager.Remove(persistCtx, authID); err == nil {
					present[authID] = false
				}
			} else {
				if _, err := manager.Register(persistCtx, manualChurnAuth(authID)); err == nil {
					modelRegistry.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: manualChurnModel}})
					manager.RefreshSchedulerEntry(authID)
					manualRetryWindowState.markReadded(authID, time.Now())
					present[authID] = true
				}
			}
			index++
		}
	}
}

func manualChurnAuth(authID string) *coreauth.Auth {
	return &coreauth.Auth{ID: authID, Provider: "codex"}
}

func muteManualBenchLogger() func() {
	logger := log.StandardLogger()
	prevOut := logger.Out
	prevLevel := logger.GetLevel()
	logger.SetOutput(io.Discard)
	logger.SetLevel(log.PanicLevel)
	return func() {
		logger.SetOutput(prevOut)
		logger.SetLevel(prevLevel)
	}
}

func manualSummaryLine(name string, summary helps.CodexLoadRunnerSummary) string {
	return fmt.Sprintf(
		"SUMMARY name=%s avg_first_byte_ms=%.3f p95_first_byte_ms=%.3f avg_first_visible_ms=%.3f p95_first_visible_ms=%.3f avg_gap_ms=%.3f p95_gap_ms=%.3f avg_total_ms=%.3f p95_total_ms=%.3f failures=%d zero_output=%d forced_retries=%d",
		name,
		manualDurationMs(summary.AvgFirstByte),
		manualDurationMs(summary.P95FirstByte),
		manualDurationMs(summary.AvgFirstVisibleOutput),
		manualDurationMs(summary.P95FirstVisibleOutput),
		manualDurationMs(summary.AvgVisibleAfterFirstByteGap),
		manualDurationMs(summary.P95VisibleAfterFirstByteGap),
		manualDurationMs(summary.AvgTotal),
		manualDurationMs(summary.P95Total),
		summary.Failures,
		summary.ZeroOutputResponses,
		manualRetryWindowState.injections(),
	)
}

func manualDeltaLine(baseName string, currentName string, summaries map[string]helps.CodexLoadRunnerSummary) string {
	base, okBase := summaries[baseName]
	current, okCurrent := summaries[currentName]
	if !okBase || !okCurrent {
		return fmt.Sprintf("DELTA base=%s current=%s missing_summary=true", baseName, currentName)
	}
	return fmt.Sprintf(
		"DELTA base=%s current=%s delta_avg_first_byte_ms=%+.3f delta_p95_first_byte_ms=%+.3f delta_avg_first_visible_ms=%+.3f delta_p95_first_visible_ms=%+.3f delta_avg_total_ms=%+.3f delta_p95_total_ms=%+.3f",
		baseName,
		currentName,
		manualDurationMs(current.AvgFirstByte-base.AvgFirstByte),
		manualDurationMs(current.P95FirstByte-base.P95FirstByte),
		manualDurationMs(current.AvgFirstVisibleOutput-base.AvgFirstVisibleOutput),
		manualDurationMs(current.P95FirstVisibleOutput-base.P95FirstVisibleOutput),
		manualDurationMs(current.AvgTotal-base.AvgTotal),
		manualDurationMs(current.P95Total-base.P95Total),
	)
}

func manualDurationMs(value time.Duration) float64 {
	return float64(value) / float64(time.Millisecond)
}

type manualRetryState struct {
	injectionCount atomic.Int64
	mu             sync.RWMutex
	readdedAt      map[string]time.Time
}

func newManualRetryWindowState() *manualRetryState {
	return &manualRetryState{readdedAt: make(map[string]time.Time)}
}

func (s *manualRetryState) reset() {
	if s == nil {
		return
	}
	s.injectionCount.Store(0)
	s.mu.Lock()
	clear(s.readdedAt)
	s.mu.Unlock()
}

func (s *manualRetryState) markReadded(authID string, at time.Time) {
	if s == nil || strings.TrimSpace(authID) == "" {
		return
	}
	s.mu.Lock()
	s.readdedAt[authID] = at
	s.mu.Unlock()
}

func (s *manualRetryState) shouldInject(scenario string, auth *coreauth.Auth) bool {
	if s == nil || auth == nil || !strings.Contains(strings.ToLower(strings.TrimSpace(scenario)), "retry-window") {
		return false
	}
	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		return false
	}
	s.mu.RLock()
	readdedAt, ok := s.readdedAt[authID]
	s.mu.RUnlock()
	if !ok {
		return false
	}
	if time.Since(readdedAt) > 4*time.Millisecond {
		return false
	}
	s.injectionCount.Add(1)
	return true
}

func (s *manualRetryState) injections() int64 {
	if s == nil {
		return 0
	}
	return s.injectionCount.Load()
}
