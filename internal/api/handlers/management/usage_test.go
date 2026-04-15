package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	internalusage "github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestGetUsageStatisticsIncludesFailureDiagnostics(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	stats := internalusage.NewRequestStatistics()
	stats.Record(ctx, coreusage.Record{
		APIKey:      "api-key-1",
		Model:       "gpt-5.4",
		RequestID:   "req-usage-1",
		Source:      "user@example.com",
		AuthIndex:   "7",
		RequestedAt: time.Date(2026, 4, 14, 10, 30, 0, 0, time.UTC),
		Failed:      true,
		Detail: coreusage.Detail{
			StatusCode:   429,
			ErrorMessage: "rate limit exceeded",
			CompactFailure: &coreusage.CompactFailureSample{
				FailureStage: "upstream_http_status",
				ErrorClass:   "rate_limited",
				Summary:      "rate limit exceeded",
			},
		},
	})

	handler := &Handler{usageStats: stats}
	handler.GetUsageStatistics(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	var payload struct {
		Usage struct {
			APIs map[string]struct {
				Models map[string]struct {
					Details []struct {
						RequestID      string `json:"request_id"`
						StatusCode     int    `json:"status_code"`
						ErrorMessage   string `json:"error_message"`
						CompactFailure struct {
							FailureStage string `json:"failure_stage"`
							ErrorClass   string `json:"error_class"`
							Summary      string `json:"summary"`
						} `json:"compact_failure"`
					} `json:"details"`
				} `json:"models"`
			} `json:"apis"`
		} `json:"usage"`
		FailedRequests int64 `json:"failed_requests"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.FailedRequests != 1 {
		t.Fatalf("failed_requests = %d, want 1", payload.FailedRequests)
	}
	details := payload.Usage.APIs["api-key-1"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].RequestID != "req-usage-1" {
		t.Fatalf("request_id = %q, want req-usage-1", details[0].RequestID)
	}
	if details[0].StatusCode != 429 {
		t.Fatalf("status_code = %d, want 429", details[0].StatusCode)
	}
	if details[0].ErrorMessage != "rate limit exceeded" {
		t.Fatalf("error_message = %q", details[0].ErrorMessage)
	}
	if details[0].CompactFailure.ErrorClass != "rate_limited" {
		t.Fatalf("compact_failure.error_class = %q", details[0].CompactFailure.ErrorClass)
	}
}

func TestExportImportUsageStatisticsPreservesFailureDiagnostics(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	sourceStats := internalusage.NewRequestStatistics()
	sourceStats.Record(nil, coreusage.Record{
		APIKey:      "api-key-2",
		Model:       "gpt-5.3-codex",
		RequestID:   "req-export-1",
		Source:      "codex:alice@example.com",
		AuthIndex:   "3",
		RequestedAt: time.Date(2026, 4, 14, 11, 0, 0, 0, time.UTC),
		Failed:      true,
		Detail: coreusage.Detail{
			StatusCode:   408,
			ErrorMessage: "stream closed before response.completed",
			CompactFailure: &coreusage.CompactFailureSample{
				FailureStage: "upstream_read",
				ErrorClass:   "timeout",
				Summary:      "stream closed before response.completed",
			},
		},
	})

	exportRecorder := httptest.NewRecorder()
	exportCtx, _ := gin.CreateTestContext(exportRecorder)
	(&Handler{usageStats: sourceStats}).ExportUsageStatistics(exportCtx)
	if exportRecorder.Code != http.StatusOK {
		t.Fatalf("export status = %d, want 200", exportRecorder.Code)
	}

	importRecorder := httptest.NewRecorder()
	importCtx, _ := gin.CreateTestContext(importRecorder)
	importCtx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/usage/import", strings.NewReader(exportRecorder.Body.String()))
	importCtx.Request.Header.Set("Content-Type", "application/json")

	targetStats := internalusage.NewRequestStatistics()
	(&Handler{usageStats: targetStats}).ImportUsageStatistics(importCtx)
	if importRecorder.Code != http.StatusOK {
		t.Fatalf("import status = %d, want 200 body=%s", importRecorder.Code, importRecorder.Body.String())
	}

	snapshot := targetStats.Snapshot()
	details := snapshot.APIs["api-key-2"].Models["gpt-5.3-codex"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].RequestID != "req-export-1" {
		t.Fatalf("request_id = %q, want req-export-1", details[0].RequestID)
	}
	if details[0].StatusCode != 408 {
		t.Fatalf("status_code = %d, want 408", details[0].StatusCode)
	}
	if details[0].ErrorMessage != "stream closed before response.completed" {
		t.Fatalf("error_message = %q", details[0].ErrorMessage)
	}
	if details[0].CompactFailure == nil {
		t.Fatal("compact_failure is nil")
	}
	if details[0].CompactFailure.FailureStage != "upstream_read" {
		t.Fatalf("compact_failure.failure_stage = %q", details[0].CompactFailure.FailureStage)
	}
}
