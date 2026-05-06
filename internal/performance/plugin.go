package performance

import (
	"context"
	"net/http"
	"strings"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

type Plugin struct {
	tracker *Tracker
}

var defaultTracker = NewTracker(DefaultConfig())

func init() {
	coreusage.RegisterPlugin(NewPlugin(defaultTracker))
}

func NewPlugin(tracker *Tracker) *Plugin {
	return &Plugin{tracker: tracker}
}

func DefaultTracker() *Tracker {
	return defaultTracker
}

func ConfigureDefault(cfg Config) {
	defaultTracker.Configure(cfg)
}

func (p *Plugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil || p.tracker == nil {
		return
	}
	if shouldSkipAuthPerformanceRecord(record) {
		return
	}
	p.tracker.Record(Sample{
		Provider:         record.Provider,
		AuthID:           record.AuthID,
		AuthIndex:        record.AuthIndex,
		Model:            record.Model,
		RequestedAt:      record.RequestedAt,
		Latency:          record.Latency,
		FirstByteLatency: record.FirstByteLatency,
		OutputTokens:     record.Detail.OutputTokens,
		TotalTokens:      record.Detail.TotalTokens,
		Failed:           record.Failed,
		StatusCode:       record.Detail.StatusCode,
	})
}

func shouldSkipAuthPerformanceRecord(record coreusage.Record) bool {
	if !record.Failed {
		return false
	}
	statusCode := record.Detail.StatusCode
	message := authPerformanceFailureText(record.Detail)
	if isAuthPerformanceClientCancel(message) {
		if shouldKeepAuthPerformanceClientCancel(record) {
			return false
		}
		return true
	}
	if isAuthPerformanceSafetyBlock(message) {
		return true
	}
	switch statusCode {
	case http.StatusBadRequest:
		return isAuthPerformanceRequestInvalid(message)
	case http.StatusNotFound:
		return isAuthPerformanceRequestScopedNotFound(message)
	case http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

func shouldKeepAuthPerformanceClientCancel(record coreusage.Record) bool {
	if normalize(record.Provider) != "codex" || normalizeModel(record.Model) != "gpt-5.4" {
		return false
	}
	return record.Latency >= 3*time.Minute
}

func authPerformanceFailureText(detail coreusage.Detail) string {
	parts := []string{
		detail.ErrorMessage,
	}
	if detail.CompactFailure != nil {
		parts = append(parts,
			detail.CompactFailure.ErrorClass,
			detail.CompactFailure.FailureStage,
			detail.CompactFailure.Summary,
		)
	}
	return strings.ToLower(strings.TrimSpace(strings.Join(parts, " ")))
}

func isAuthPerformanceClientCancel(message string) bool {
	return strings.Contains(message, "context canceled") ||
		strings.Contains(message, "client disconnected") ||
		strings.Contains(message, "client closed")
}

func isAuthPerformanceSafetyBlock(message string) bool {
	return strings.Contains(message, "moderation_blocked") ||
		strings.Contains(message, "cyber_policy") ||
		strings.Contains(message, "request was rejected by the safety system")
}

func isAuthPerformanceRequestInvalid(message string) bool {
	if !strings.Contains(message, "invalid_request_error") {
		return false
	}
	return strings.Contains(message, "missing_required_parameter") ||
		strings.Contains(message, "previous_response_not_found") ||
		strings.Contains(message, "previous_response_id") ||
		strings.Contains(message, "conversation_id") ||
		strings.Contains(message, "prompt") ||
		strings.Contains(message, "input")
}

func isAuthPerformanceRequestScopedNotFound(message string) bool {
	return strings.Contains(message, "item with id") &&
		strings.Contains(message, "not found") &&
		strings.Contains(message, "store")
}
