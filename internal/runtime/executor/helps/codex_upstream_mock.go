package helps

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

const (
	defaultCodexUpstreamMockScenario = "fast-complete"

	codexUpstreamMockOutcomeCompleted      = "completed"
	codexUpstreamMockOutcomeInjectedEOF    = "injected_eof"
	codexUpstreamMockOutcomeClientCanceled = "client_canceled"
	codexUpstreamMockOutcomeWriteError     = "write_error"
)

// CodexUpstreamMock provides a reusable mock upstream for Codex /responses and /responses/compact.
type CodexUpstreamMock struct {
	mu        sync.Mutex
	startedAt time.Time
	scenarios map[string]codexUpstreamMockScenario
	stats     codexUpstreamMockStatsMutable
}

// CodexUpstreamMockStats is a snapshot of the mock state.
type CodexUpstreamMockStats struct {
	StartedAt time.Time                                 `json:"started_at"`
	Requests  int64                                     `json:"requests"`
	Active    int64                                     `json:"active"`
	Routes    map[string]CodexUpstreamMockRouteStats    `json:"routes"`
	Scenarios map[string]CodexUpstreamMockScenarioStats `json:"scenarios"`
	Auths     map[string]CodexUpstreamMockAuthStats     `json:"auths"`
	Profiles  map[string]CodexUpstreamMockProfileStats  `json:"profiles"`
}

// CodexUpstreamMockRouteStats groups totals by route.
type CodexUpstreamMockRouteStats struct {
	Requests       int64 `json:"requests"`
	Completed      int64 `json:"completed"`
	InjectedEOF    int64 `json:"injected_eof"`
	ClientCanceled int64 `json:"client_canceled"`
	WriteErrors    int64 `json:"write_errors"`
	Status2xx      int64 `json:"status_2xx"`
	Status4xx      int64 `json:"status_4xx"`
	Status5xx      int64 `json:"status_5xx"`
}

// CodexUpstreamMockScenarioStats groups totals by scenario.
type CodexUpstreamMockScenarioStats struct {
	Requests       int64 `json:"requests"`
	Completed      int64 `json:"completed"`
	InjectedEOF    int64 `json:"injected_eof"`
	ClientCanceled int64 `json:"client_canceled"`
	WriteErrors    int64 `json:"write_errors"`
	Status2xx      int64 `json:"status_2xx"`
	Status4xx      int64 `json:"status_4xx"`
	Status5xx      int64 `json:"status_5xx"`
}

// CodexUpstreamMockAuthStats groups totals by auth id.
type CodexUpstreamMockAuthStats struct {
	Requests       int64 `json:"requests"`
	Completed      int64 `json:"completed"`
	InjectedEOF    int64 `json:"injected_eof"`
	ClientCanceled int64 `json:"client_canceled"`
	WriteErrors    int64 `json:"write_errors"`
	Status2xx      int64 `json:"status_2xx"`
	Status4xx      int64 `json:"status_4xx"`
	Status5xx      int64 `json:"status_5xx"`
}

// CodexUpstreamMockProfileStats groups totals by auth profile.
type CodexUpstreamMockProfileStats struct {
	Requests       int64 `json:"requests"`
	Completed      int64 `json:"completed"`
	InjectedEOF    int64 `json:"injected_eof"`
	ClientCanceled int64 `json:"client_canceled"`
	WriteErrors    int64 `json:"write_errors"`
	Status2xx      int64 `json:"status_2xx"`
	Status4xx      int64 `json:"status_4xx"`
	Status5xx      int64 `json:"status_5xx"`
}

type codexUpstreamMockStatsMutable struct {
	requests  int64
	active    int64
	routes    map[string]*CodexUpstreamMockRouteStats
	scenarios map[string]*CodexUpstreamMockScenarioStats
	auths     map[string]*CodexUpstreamMockAuthStats
	profiles  map[string]*CodexUpstreamMockProfileStats
}

type codexUpstreamMockScenario struct {
	Name        string                         `json:"name"`
	Description string                         `json:"description"`
	Responses   codexUpstreamMockResponsesPlan `json:"responses"`
	Compact     *codexUpstreamMockCompactPlan  `json:"compact,omitempty"`
}

type codexUpstreamMockResponsesPlan struct {
	StatusCode  int                               `json:"status_code"`
	Body        string                            `json:"body,omitempty"`
	Events      []codexUpstreamMockResponsesEvent `json:"events,omitempty"`
	Outcome     string                            `json:"outcome"`
	ContentType string                            `json:"content_type,omitempty"`
}

type codexUpstreamMockResponsesEvent struct {
	Delay   time.Duration `json:"delay"`
	Data    string        `json:"data"`
	Comment string        `json:"comment,omitempty"`
}

type codexUpstreamMockCompactPlan struct {
	StatusCode  int           `json:"status_code"`
	Delay       time.Duration `json:"delay"`
	Body        string        `json:"body"`
	ContentType string        `json:"content_type,omitempty"`
}

type codexUpstreamMockRequestTracker struct {
	mock        *CodexUpstreamMock
	route       string
	scenario    string
	authID      string
	profile     string
	ordinal     int64
	finished    bool
	finishMutex sync.Mutex
}

// NewCodexUpstreamMock creates a mock with built-in scenarios.
func NewCodexUpstreamMock() *CodexUpstreamMock {
	return &CodexUpstreamMock{
		startedAt: time.Now(),
		scenarios: codexUpstreamMockBuiltIns(),
		stats: codexUpstreamMockStatsMutable{
			routes:    make(map[string]*CodexUpstreamMockRouteStats),
			scenarios: make(map[string]*CodexUpstreamMockScenarioStats),
			auths:     make(map[string]*CodexUpstreamMockAuthStats),
			profiles:  make(map[string]*CodexUpstreamMockProfileStats),
		},
	}
}

// Handler returns an HTTP handler suitable for httptest or a standalone server.
func (m *CodexUpstreamMock) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/_mock/stats", m.handleStats)
	mux.HandleFunc("/_mock/reset", m.handleReset)
	mux.HandleFunc("/_mock/scenarios", m.handleScenarios)
	mux.HandleFunc("/", m.handleRequest)
	return mux
}

// Snapshot returns a copy of the current stats.
func (m *CodexUpstreamMock) Snapshot() CodexUpstreamMockStats {
	m.mu.Lock()
	defer m.mu.Unlock()

	snapshot := CodexUpstreamMockStats{
		StartedAt: m.startedAt,
		Requests:  m.stats.requests,
		Active:    m.stats.active,
		Routes:    make(map[string]CodexUpstreamMockRouteStats, len(m.stats.routes)),
		Scenarios: make(map[string]CodexUpstreamMockScenarioStats, len(m.stats.scenarios)),
		Auths:     make(map[string]CodexUpstreamMockAuthStats, len(m.stats.auths)),
		Profiles:  make(map[string]CodexUpstreamMockProfileStats, len(m.stats.profiles)),
	}
	for key, value := range m.stats.routes {
		if value == nil {
			continue
		}
		snapshot.Routes[key] = *value
	}
	for key, value := range m.stats.scenarios {
		if value == nil {
			continue
		}
		snapshot.Scenarios[key] = *value
	}
	for key, value := range m.stats.auths {
		if value == nil {
			continue
		}
		snapshot.Auths[key] = *value
	}
	for key, value := range m.stats.profiles {
		if value == nil {
			continue
		}
		snapshot.Profiles[key] = *value
	}
	return snapshot
}

func (m *CodexUpstreamMock) handleStats(w http.ResponseWriter, _ *http.Request) {
	writeJSONResponse(w, http.StatusOK, m.Snapshot())
}

func (m *CodexUpstreamMock) handleReset(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Method, http.MethodPost) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	m.mu.Lock()
	m.startedAt = time.Now()
	m.stats = codexUpstreamMockStatsMutable{
		routes:    make(map[string]*CodexUpstreamMockRouteStats),
		scenarios: make(map[string]*CodexUpstreamMockScenarioStats),
		auths:     make(map[string]*CodexUpstreamMockAuthStats),
		profiles:  make(map[string]*CodexUpstreamMockProfileStats),
	}
	m.mu.Unlock()
	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (m *CodexUpstreamMock) handleScenarios(w http.ResponseWriter, _ *http.Request) {
	type scenarioView struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	out := make([]scenarioView, 0, len(m.scenarios))
	for _, scenario := range m.scenarios {
		out = append(out, scenarioView{Name: scenario.Name, Description: scenario.Description})
	}
	writeJSONResponse(w, http.StatusOK, out)
}

func (m *CodexUpstreamMock) handleRequest(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Method, http.MethodPost) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	route, pathScenario, ok := parseCodexUpstreamMockRoute(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	body, err := readAllBytes(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	scenarioName := strings.TrimSpace(selectCodexUpstreamMockScenario(r, body, pathScenario))
	if scenarioName == "" {
		scenarioName = defaultCodexUpstreamMockScenario
	}
	authID := selectCodexUpstreamMockAuthID(r)
	profileName := selectCodexUpstreamMockProfile(r)
	profile := m.selectProfile(profileName)
	scenarioName = strings.TrimSpace(m.effectiveScenarioName(scenarioName, profile))
	if scenarioName == "" {
		scenarioName = defaultCodexUpstreamMockScenario
	}
	scenario, found := m.scenarios[scenarioName]
	if !found {
		writeJSONError(w, http.StatusBadRequest, "unknown mock scenario: "+scenarioName)
		return
	}

	tracker := m.beginRequest(route, scenarioName, authID, profileName)
	switch route {
	case "responses":
		m.handleResponses(w, r, tracker, m.applyProfileToResponsesPlan(scenario.Responses, profile, tracker.ordinal))
	case "responses/compact":
		m.handleCompact(w, r, tracker, scenarioName, m.applyProfileToCompactPlan(scenario.Compact, profile, tracker.ordinal))
	default:
		http.NotFound(w, r)
	}
}

func (m *CodexUpstreamMock) handleResponses(w http.ResponseWriter, r *http.Request, tracker *codexUpstreamMockRequestTracker, plan codexUpstreamMockResponsesPlan) {
	statusCode := plan.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	if statusCode < 200 || statusCode >= 300 {
		contentType := strings.TrimSpace(plan.ContentType)
		if contentType == "" {
			contentType = "application/json"
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(statusCode)
		if len(plan.Body) > 0 {
			_, _ = w.Write([]byte(plan.Body))
		}
		tracker.Finish(codexUpstreamMockOutcomeCompleted, statusCode)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	for _, event := range plan.Events {
		if !sleepWithContext(r.Context(), event.Delay) {
			tracker.Finish(codexUpstreamMockOutcomeClientCanceled, 499)
			return
		}
		payload := ""
		switch {
		case event.Comment != "":
			payload = ": " + event.Comment + "\n\n"
		case event.Data != "":
			payload = "data: " + event.Data + "\n\n"
		default:
			payload = "\n"
		}
		if _, err := w.Write([]byte(payload)); err != nil {
			tracker.Finish(codexUpstreamMockOutcomeWriteError, http.StatusOK)
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	switch plan.Outcome {
	case codexUpstreamMockOutcomeInjectedEOF:
		tracker.Finish(codexUpstreamMockOutcomeInjectedEOF, http.StatusOK)
	default:
		tracker.Finish(codexUpstreamMockOutcomeCompleted, http.StatusOK)
	}
}

func (m *CodexUpstreamMock) handleCompact(w http.ResponseWriter, r *http.Request, tracker *codexUpstreamMockRequestTracker, scenarioName string, plan *codexUpstreamMockCompactPlan) {
	compact := plan
	if compact == nil {
		compact = defaultCodexUpstreamMockCompactPlan(scenarioName)
	}
	if !sleepWithContext(r.Context(), compact.Delay) {
		tracker.Finish(codexUpstreamMockOutcomeClientCanceled, 499)
		return
	}
	contentType := strings.TrimSpace(compact.ContentType)
	if contentType == "" {
		contentType = "application/json"
	}
	statusCode := compact.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(statusCode)
	if len(compact.Body) > 0 {
		_, _ = w.Write([]byte(compact.Body))
	}
	tracker.Finish(codexUpstreamMockOutcomeCompleted, statusCode)
}

func (m *CodexUpstreamMock) beginRequest(route, scenario, authID, profile string) *codexUpstreamMockRequestTracker {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.stats.requests++
	m.stats.active++
	routeStats := m.stats.routes[route]
	if routeStats == nil {
		routeStats = &CodexUpstreamMockRouteStats{}
		m.stats.routes[route] = routeStats
	}
	routeStats.Requests++

	scenarioStats := m.stats.scenarios[scenario]
	if scenarioStats == nil {
		scenarioStats = &CodexUpstreamMockScenarioStats{}
		m.stats.scenarios[scenario] = scenarioStats
	}
	scenarioStats.Requests++
	authStats := m.ensureAuthStatsLocked(authID)
	if authStats != nil {
		authStats.Requests++
	}
	profileStats := m.ensureProfileStatsLocked(profile)
	if profileStats != nil {
		profileStats.Requests++
	}
	ordinal := int64(0)
	if authStats != nil {
		ordinal = authStats.Requests
	}
	if ordinal == 0 {
		ordinal = routeStats.Requests
	}

	return &codexUpstreamMockRequestTracker{
		mock:     m,
		route:    route,
		scenario: scenario,
		authID:   authID,
		profile:  profile,
		ordinal:  ordinal,
	}
}

func (t *codexUpstreamMockRequestTracker) Finish(outcome string, statusCode int) {
	t.finishMutex.Lock()
	defer t.finishMutex.Unlock()
	if t.finished || t.mock == nil {
		return
	}
	t.finished = true

	t.mock.mu.Lock()
	defer t.mock.mu.Unlock()
	if t.mock.stats.active > 0 {
		t.mock.stats.active--
	}
	routeStats := t.mock.stats.routes[t.route]
	if routeStats == nil {
		routeStats = &CodexUpstreamMockRouteStats{}
		t.mock.stats.routes[t.route] = routeStats
	}
	scenarioStats := t.mock.stats.scenarios[t.scenario]
	if scenarioStats == nil {
		scenarioStats = &CodexUpstreamMockScenarioStats{}
		t.mock.stats.scenarios[t.scenario] = scenarioStats
	}
	authStats := t.mock.ensureAuthStatsLocked(t.authID)
	profileStats := t.mock.ensureProfileStatsLocked(t.profile)
	applyCodexUpstreamMockOutcome(routeStats, outcome, statusCode)
	applyCodexUpstreamMockOutcome(scenarioStats, outcome, statusCode)
	applyCodexUpstreamMockOutcome(authStats, outcome, statusCode)
	applyCodexUpstreamMockOutcome(profileStats, outcome, statusCode)
}

func applyCodexUpstreamMockOutcome(target any, outcome string, statusCode int) {
	if target == nil {
		return
	}
	switch stats := target.(type) {
	case *CodexUpstreamMockRouteStats:
		if stats == nil {
			return
		}
		applyCodexUpstreamMockCounters(&stats.Completed, &stats.InjectedEOF, &stats.ClientCanceled, &stats.WriteErrors, outcome)
		applyCodexUpstreamMockStatusCounters(&stats.Status2xx, &stats.Status4xx, &stats.Status5xx, statusCode)
	case *CodexUpstreamMockScenarioStats:
		if stats == nil {
			return
		}
		applyCodexUpstreamMockCounters(&stats.Completed, &stats.InjectedEOF, &stats.ClientCanceled, &stats.WriteErrors, outcome)
		applyCodexUpstreamMockStatusCounters(&stats.Status2xx, &stats.Status4xx, &stats.Status5xx, statusCode)
	case *CodexUpstreamMockAuthStats:
		if stats == nil {
			return
		}
		applyCodexUpstreamMockCounters(&stats.Completed, &stats.InjectedEOF, &stats.ClientCanceled, &stats.WriteErrors, outcome)
		applyCodexUpstreamMockStatusCounters(&stats.Status2xx, &stats.Status4xx, &stats.Status5xx, statusCode)
	case *CodexUpstreamMockProfileStats:
		if stats == nil {
			return
		}
		applyCodexUpstreamMockCounters(&stats.Completed, &stats.InjectedEOF, &stats.ClientCanceled, &stats.WriteErrors, outcome)
		applyCodexUpstreamMockStatusCounters(&stats.Status2xx, &stats.Status4xx, &stats.Status5xx, statusCode)
	}
}

func applyCodexUpstreamMockCounters(completed, injectedEOF, clientCanceled, writeErrors *int64, outcome string) {
	switch outcome {
	case codexUpstreamMockOutcomeInjectedEOF:
		*injectedEOF++
	case codexUpstreamMockOutcomeClientCanceled:
		*clientCanceled++
	case codexUpstreamMockOutcomeWriteError:
		*writeErrors++
	default:
		*completed++
	}
}

func applyCodexUpstreamMockStatusCounters(status2xx, status4xx, status5xx *int64, statusCode int) {
	switch {
	case statusCode >= 500:
		*status5xx++
	case statusCode >= 400:
		*status4xx++
	case statusCode >= 200:
		*status2xx++
	}
}

func selectCodexUpstreamMockScenario(r *http.Request, body []byte, pathScenario string) string {
	if bodyScenario := strings.TrimSpace(gjson.GetBytes(body, "metadata.cpa_mock_scenario").String()); bodyScenario != "" {
		return bodyScenario
	}
	if bodyScenario := strings.TrimSpace(gjson.GetBytes(body, "cpa_mock_scenario").String()); bodyScenario != "" {
		return bodyScenario
	}
	if queryScenario := strings.TrimSpace(r.URL.Query().Get("scenario")); queryScenario != "" {
		return queryScenario
	}
	if headerScenario := strings.TrimSpace(r.Header.Get("X-CPA-Mock-Scenario")); headerScenario != "" {
		return headerScenario
	}
	return pathScenario
}

func selectCodexUpstreamMockAuthID(r *http.Request) string {
	if r == nil {
		return ""
	}
	return strings.TrimSpace(r.Header.Get("X-Mock-Auth-ID"))
}

func selectCodexUpstreamMockProfile(r *http.Request) string {
	if r == nil {
		return ""
	}
	if value := strings.TrimSpace(r.Header.Get("X-Mock-Profile")); value != "" {
		return value
	}
	return strings.TrimSpace(r.Header.Get("X-CPA-Mock-Profile"))
}

func parseCodexUpstreamMockRoute(path string) (route string, scenario string, ok bool) {
	trimmed := "/" + strings.Trim(strings.TrimSpace(path), "/")
	switch trimmed {
	case "/responses":
		return "responses", "", true
	case "/responses/compact":
		return "responses/compact", "", true
	}
	if !strings.HasPrefix(trimmed, "/scenarios/") {
		return "", "", false
	}
	rest := strings.TrimPrefix(trimmed, "/scenarios/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		return "", "", false
	}
	scenario = strings.TrimSpace(parts[0])
	route = strings.Join(parts[1:], "/")
	switch route {
	case "responses", "responses/compact":
		return route, scenario, true
	default:
		return "", "", false
	}
}

func codexUpstreamMockBuiltIns() map[string]codexUpstreamMockScenario {
	scenarios := map[string]codexUpstreamMockScenario{
		"fast-complete": {
			Name:        "fast-complete",
			Description: "Immediate created -> output_text.delta -> completed stream.",
			Responses: codexUpstreamMockResponsesPlan{
				StatusCode: http.StatusOK,
				Events: []codexUpstreamMockResponsesEvent{
					{Data: `{"type":"response.created","response":{"id":"resp_fast","created_at":1775555723,"model":"gpt-5.4"}}`},
					{Data: `{"type":"response.output_text.delta","delta":"mock output"}`},
					{Data: `{"type":"response.completed","response":{"id":"resp_fast","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"mock output"}]}],"usage":{"input_tokens":12,"output_tokens":4,"total_tokens":16}}}`},
				},
				Outcome: codexUpstreamMockOutcomeCompleted,
			},
		},
		"reasoning-before-output": {
			Name:        "reasoning-before-output",
			Description: "Several reasoning events arrive before any user-visible output.",
			Responses: codexUpstreamMockResponsesPlan{
				StatusCode: http.StatusOK,
				Events: []codexUpstreamMockResponsesEvent{
					{Data: `{"type":"response.created","response":{"id":"resp_reasoning","created_at":1775555723,"model":"gpt-5.4"}}`},
					{Delay: 40 * time.Millisecond, Data: `{"type":"response.reasoning_summary_text.delta","delta":"thinking 1"}`},
					{Delay: 40 * time.Millisecond, Data: `{"type":"response.reasoning_summary_text.delta","delta":"thinking 2"}`},
					{Delay: 40 * time.Millisecond, Data: `{"type":"response.reasoning_summary_text.delta","delta":"thinking 3"}`},
					{Delay: 80 * time.Millisecond, Data: `{"type":"response.output_text.delta","delta":"answer"}`},
					{Data: `{"type":"response.completed","response":{"id":"resp_reasoning","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":128,"output_tokens":18,"total_tokens":146,"output_tokens_details":{"reasoning_tokens":96}}}}`},
				},
				Outcome: codexUpstreamMockOutcomeCompleted,
			},
		},
		"reasoning-burst-before-output": {
			Name:        "reasoning-burst-before-output",
			Description: "Many reasoning deltas arrive before the first visible output.",
			Responses: codexUpstreamMockResponsesPlan{
				StatusCode: http.StatusOK,
				Events: []codexUpstreamMockResponsesEvent{
					{Data: `{"type":"response.created","response":{"id":"resp_burst","created_at":1775555723,"model":"gpt-5.4"}}`},
					{Delay: 20 * time.Millisecond, Data: `{"type":"response.reasoning_summary_text.delta","delta":"thinking 1"}`},
					{Delay: 20 * time.Millisecond, Data: `{"type":"response.reasoning_summary_text.delta","delta":"thinking 2"}`},
					{Delay: 20 * time.Millisecond, Data: `{"type":"response.reasoning_summary_text.delta","delta":"thinking 3"}`},
					{Delay: 20 * time.Millisecond, Data: `{"type":"response.reasoning_summary_text.delta","delta":"thinking 4"}`},
					{Delay: 40 * time.Millisecond, Data: `{"type":"response.output_text.delta","delta":"answer"}`},
					{Data: `{"type":"response.completed","response":{"id":"resp_burst","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":160,"output_tokens":24,"total_tokens":184,"output_tokens_details":{"reasoning_tokens":112}}}}`},
				},
				Outcome: codexUpstreamMockOutcomeCompleted,
			},
		},
		"reasoning-eof": {
			Name:        "reasoning-eof",
			Description: "Reasoning arrives, then the stream closes before response.completed.",
			Responses: codexUpstreamMockResponsesPlan{
				StatusCode: http.StatusOK,
				Events: []codexUpstreamMockResponsesEvent{
					{Data: `{"type":"response.created","response":{"id":"resp_eof","created_at":1775555723,"model":"gpt-5.4"}}`},
					{Delay: 50 * time.Millisecond, Data: `{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`},
				},
				Outcome: codexUpstreamMockOutcomeInjectedEOF,
			},
		},
		"stall-before-output": {
			Name:        "stall-before-output",
			Description: "Emits response.created, then stalls before the first visible output.",
			Responses: codexUpstreamMockResponsesPlan{
				StatusCode: http.StatusOK,
				Events: []codexUpstreamMockResponsesEvent{
					{Data: `{"type":"response.created","response":{"id":"resp_stall","created_at":1775555723,"model":"gpt-5.4"}}`},
					{Delay: 1500 * time.Millisecond, Data: `{"type":"response.output_text.delta","delta":"finally"}`},
					{Data: `{"type":"response.completed","response":{"id":"resp_stall","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"finally"}]}],"usage":{"input_tokens":64,"output_tokens":8,"total_tokens":72}}}`},
				},
				Outcome: codexUpstreamMockOutcomeCompleted,
			},
		},
		"zero-usage-complete": {
			Name:        "zero-usage-complete",
			Description: "Completes with zero tokens and no user-visible output.",
			Responses: codexUpstreamMockResponsesPlan{
				StatusCode: http.StatusOK,
				Events: []codexUpstreamMockResponsesEvent{
					{Data: `{"type":"response.created","response":{"id":"resp_zero","created_at":1775555723,"model":"gpt-5.4"}}`},
					{Data: `{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`},
					{Data: `{"type":"response.completed","response":{"id":"resp_zero","object":"response","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`},
				},
				Outcome: codexUpstreamMockOutcomeCompleted,
			},
		},
		"fragmented-output-deltas": {
			Name:        "fragmented-output-deltas",
			Description: "Very small output deltas before completion.",
			Responses: codexUpstreamMockResponsesPlan{
				StatusCode: http.StatusOK,
				Events: []codexUpstreamMockResponsesEvent{
					{Data: `{"type":"response.created","response":{"id":"resp_fragmented","created_at":1775555723,"model":"gpt-5.4"}}`},
					{Data: `{"type":"response.output_text.delta","delta":"a"}`},
					{Data: `{"type":"response.output_text.delta","delta":"n"}`},
					{Data: `{"type":"response.output_text.delta","delta":"s"}`},
					{Data: `{"type":"response.output_text.delta","delta":"w"}`},
					{Data: `{"type":"response.output_text.delta","delta":"e"}`},
					{Data: `{"type":"response.output_text.delta","delta":"r"}`},
					{Data: `{"type":"response.completed","response":{"id":"resp_fragmented","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":12,"output_tokens":6,"total_tokens":18}}}`},
				},
				Outcome: codexUpstreamMockOutcomeCompleted,
			},
		},
		"tool-call-complete": {
			Name:        "tool-call-complete",
			Description: "Completes with a function call output item and zero visible text.",
			Responses: codexUpstreamMockResponsesPlan{
				StatusCode: http.StatusOK,
				Events: []codexUpstreamMockResponsesEvent{
					{Data: `{"type":"response.completed","response":{"id":"resp_tool","object":"response","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"tool_search","arguments":"{\"query\":\"hello\"}"}],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`},
				},
				Outcome: codexUpstreamMockOutcomeCompleted,
			},
		},
		"oversized-completed-event": {
			Name:        "oversized-completed-event",
			Description: "Normal streaming followed by a large response.completed payload.",
			Responses: codexUpstreamMockResponsesPlan{
				StatusCode: http.StatusOK,
				Events: []codexUpstreamMockResponsesEvent{
					{Data: `{"type":"response.created","response":{"id":"resp_big_done","created_at":1775555723,"model":"gpt-5.4"}}`},
					{Data: `{"type":"response.output_text.delta","delta":"preface"}`},
					{Data: `{"type":"response.completed","response":{"id":"resp_big_done","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + strings.Repeat("z", 16384) + `"}]}],"usage":{"input_tokens":32,"output_tokens":4096,"total_tokens":4128}}}`},
				},
				Outcome: codexUpstreamMockOutcomeCompleted,
			},
		},
		"malformed-stream-frame": {
			Name:        "malformed-stream-frame",
			Description: "A malformed SSE JSON frame appears mid-stream.",
			Responses: codexUpstreamMockResponsesPlan{
				StatusCode: http.StatusOK,
				Events: []codexUpstreamMockResponsesEvent{
					{Data: `{"type":"response.created","response":{"id":"resp_malformed","created_at":1775555723,"model":"gpt-5.4"}}`},
					{Data: `{"type":"response.output_text.delta","delta":"broken"`},
				},
				Outcome: codexUpstreamMockOutcomeInjectedEOF,
			},
		},
		"keepalive-without-events": {
			Name:        "keepalive-without-events",
			Description: "Only keepalive comments for a long period before EOF.",
			Responses: codexUpstreamMockResponsesPlan{
				StatusCode: http.StatusOK,
				Events: []codexUpstreamMockResponsesEvent{
					{Data: `{"type":"response.created","response":{"id":"resp_keepalive","created_at":1775555723,"model":"gpt-5.4"}}`},
					{Delay: 80 * time.Millisecond, Comment: "keepalive"},
					{Delay: 80 * time.Millisecond, Comment: "keepalive"},
					{Delay: 80 * time.Millisecond, Comment: "keepalive"},
				},
				Outcome: codexUpstreamMockOutcomeInjectedEOF,
			},
		},
		"rate-limit": {
			Name:        "rate-limit",
			Description: "Returns a retryable rate-limit style JSON error.",
			Responses: codexUpstreamMockResponsesPlan{
				StatusCode:  http.StatusTooManyRequests,
				ContentType: "application/json",
				Body:        `{"error":{"message":"mock rate limit","type":"usage_limit_reached","code":"rate_limit_exceeded"}}`,
				Outcome:     codexUpstreamMockOutcomeCompleted,
			},
			Compact: &codexUpstreamMockCompactPlan{
				StatusCode:  http.StatusTooManyRequests,
				ContentType: "application/json",
				Body:        `{"error":{"message":"mock rate limit","type":"usage_limit_reached","code":"rate_limit_exceeded"}}`,
			},
		},
		"server-error": {
			Name:        "server-error",
			Description: "Returns a 502 JSON error for upstream failure injection.",
			Responses: codexUpstreamMockResponsesPlan{
				StatusCode:  http.StatusBadGateway,
				ContentType: "application/json",
				Body:        `{"error":{"message":"mock upstream failure","type":"server_error","code":"bad_gateway"}}`,
				Outcome:     codexUpstreamMockOutcomeCompleted,
			},
			Compact: &codexUpstreamMockCompactPlan{
				StatusCode:  http.StatusBadGateway,
				ContentType: "application/json",
				Body:        `{"error":{"message":"mock upstream failure","type":"server_error","code":"bad_gateway"}}`,
			},
		},
	}
	fastComplete := scenarios["fast-complete"]
	scenarios["compact-success"] = codexUpstreamMockScenario{
		Name:        "compact-success",
		Description: "Compact endpoint returns an immediate completed JSON payload.",
		Responses:   fastComplete.Responses,
		Compact: &codexUpstreamMockCompactPlan{
			StatusCode: http.StatusOK,
			Body:       `{"id":"resp_compact","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"compact ok"}]}],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}`,
		},
	}
	return scenarios
}

func defaultCodexUpstreamMockCompactPlan(scenarioName string) *codexUpstreamMockCompactPlan {
	return &codexUpstreamMockCompactPlan{
		StatusCode: http.StatusOK,
		Body:       `{"id":"resp_compact_default","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"compact ` + escapeJSONString(scenarioName) + `"}]}],"usage":{"input_tokens":6,"output_tokens":2,"total_tokens":8}}`,
	}
}

func sleepWithContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func readAllBytes(body io.Reader) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	return io.ReadAll(body)
}

func writeJSONResponse(w http.ResponseWriter, statusCode int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to marshal response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = w.Write(body)
}

func writeJSONError(w http.ResponseWriter, statusCode int, message string) {
	writeJSONResponse(w, statusCode, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "invalid_request_error",
		},
	})
}

func escapeJSONString(value string) string {
	body, err := json.Marshal(value)
	if err != nil {
		return value
	}
	if len(body) < 2 {
		return value
	}
	return string(body[1 : len(body)-1])
}

func (m *CodexUpstreamMock) ensureAuthStatsLocked(authID string) *CodexUpstreamMockAuthStats {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	if m.stats.auths == nil {
		m.stats.auths = make(map[string]*CodexUpstreamMockAuthStats)
	}
	stats := m.stats.auths[authID]
	if stats == nil {
		stats = &CodexUpstreamMockAuthStats{}
		m.stats.auths[authID] = stats
	}
	return stats
}

func (m *CodexUpstreamMock) ensureProfileStatsLocked(profile string) *CodexUpstreamMockProfileStats {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return nil
	}
	if m.stats.profiles == nil {
		m.stats.profiles = make(map[string]*CodexUpstreamMockProfileStats)
	}
	stats := m.stats.profiles[profile]
	if stats == nil {
		stats = &CodexUpstreamMockProfileStats{}
		m.stats.profiles[profile] = stats
	}
	return stats
}

func containsCodexUpstreamMockCompletedMarker(data string) bool {
	return strings.Contains(data, `"type":"response.completed"`)
}
