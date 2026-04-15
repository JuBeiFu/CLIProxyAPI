package usage

import (
	"context"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestRequestStatisticsRecordIncludesLatency(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		RequestedAt: time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
		Latency:     1500 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:  10,
			OutputTokens: 20,
			TotalTokens:  30,
		},
	})

	snapshot := stats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].LatencyMs != 1500 {
		t.Fatalf("latency_ms = %d, want 1500", details[0].LatencyMs)
	}
}

func TestRequestStatisticsMergeSnapshotDedupIgnoresLatency(t *testing.T) {
	stats := NewRequestStatistics()
	timestamp := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	first := StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"test-key": {
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						Details: []RequestDetail{{
							Timestamp: timestamp,
							LatencyMs: 0,
							Source:    "user@example.com",
							AuthIndex: "0",
							Tokens: TokenStats{
								InputTokens:  10,
								OutputTokens: 20,
								TotalTokens:  30,
							},
						}},
					},
				},
			},
		},
	}
	second := StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"test-key": {
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						Details: []RequestDetail{{
							Timestamp: timestamp,
							LatencyMs: 2500,
							Source:    "user@example.com",
							AuthIndex: "0",
							Tokens: TokenStats{
								InputTokens:  10,
								OutputTokens: 20,
								TotalTokens:  30,
							},
						}},
					},
				},
			},
		},
	}

	result := stats.MergeSnapshot(first)
	if result.Added != 1 || result.Skipped != 0 {
		t.Fatalf("first merge = %+v, want added=1 skipped=0", result)
	}

	result = stats.MergeSnapshot(second)
	if result.Added != 0 || result.Skipped != 1 {
		t.Fatalf("second merge = %+v, want added=0 skipped=1", result)
	}

	snapshot := stats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
}

func TestRequestStatisticsRecordIncludesFailureDiagnostics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx := &gin.Context{}
	ginCtx.Set("apiKey", "test-key")
	ginCtx.Set("REQUEST_ID", "req-123")
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	stats := NewRequestStatistics()
	stats.Record(ctx, coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		Source:      "user@example.com",
		AuthIndex:   "0",
		RequestedAt: time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC),
		Failed:      true,
		Detail: coreusage.Detail{
			StatusCode:   429,
			ErrorMessage: "rate limit exceeded",
			CompactFailure: &coreusage.CompactFailureSample{
				FailureStage: "upstream_http_status",
				ErrorClass:   "rate_limited",
			},
		},
	})

	snapshot := stats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].StatusCode != 429 {
		t.Fatalf("status_code = %d, want 429", details[0].StatusCode)
	}
	if details[0].ErrorMessage != "rate limit exceeded" {
		t.Fatalf("error_message = %q, want rate limit exceeded", details[0].ErrorMessage)
	}
	if details[0].RequestID != "req-123" {
		t.Fatalf("request_id = %q, want req-123", details[0].RequestID)
	}
	if details[0].CompactFailure == nil {
		t.Fatal("compact_failure is nil")
	}
	if details[0].CompactFailure.ErrorClass != "rate_limited" {
		t.Fatalf("compact_failure.error_class = %q, want rate_limited", details[0].CompactFailure.ErrorClass)
	}
}
