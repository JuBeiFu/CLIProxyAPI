package management

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

type usageExportPayload struct {
	Version    int                      `json:"version"`
	ExportedAt time.Time                `json:"exported_at"`
	Usage      usage.StatisticsSnapshot `json:"usage"`
}

type usageImportPayload struct {
	Version int                      `json:"version"`
	Usage   usage.StatisticsSnapshot `json:"usage"`
}

// GetUsageStatistics returns the in-memory request statistics snapshot.
func (h *Handler) GetUsageStatistics(c *gin.Context) {
	var snapshot usage.StatisticsSnapshot
	if h != nil && h.usageStats != nil {
		snapshot = h.usageStats.Snapshot()
	}

	// Optional: trim request details to reduce payload size for large installs.
	// Query params:
	// - detail_limit: max detail events per model (0 disables details; default keeps all)
	// - range: limit to recent window ("7h", "24h", "7d"); default "all"
	if rangeKey := strings.ToLower(strings.TrimSpace(c.Query("range"))); rangeKey != "" && rangeKey != "all" {
		var window time.Duration
		switch rangeKey {
		case "7h":
			window = 7 * time.Hour
		case "24h":
			window = 24 * time.Hour
		case "7d":
			window = 7 * 24 * time.Hour
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid range"})
			return
		}
		cutoff := time.Now().Add(-window)
		snapshot = filterUsageSnapshotByTime(snapshot, cutoff)
	}

	if rawLimit := strings.TrimSpace(c.Query("detail_limit")); rawLimit != "" {
		limit, err := strconv.Atoi(rawLimit)
		if err != nil || limit < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid detail_limit"})
			return
		}
		snapshot = trimUsageSnapshotDetails(snapshot, limit)
	}
	c.JSON(http.StatusOK, gin.H{
		"usage":           snapshot,
		"failed_requests": snapshot.FailureCount,
	})
}

func trimUsageSnapshotDetails(snapshot usage.StatisticsSnapshot, limit int) usage.StatisticsSnapshot {
	if limit < 0 {
		return snapshot
	}
	if snapshot.APIs == nil {
		return snapshot
	}

	for apiKey, apiSnapshot := range snapshot.APIs {
		if apiSnapshot.Models == nil {
			continue
		}
		for modelKey, modelSnapshot := range apiSnapshot.Models {
			if limit == 0 {
				modelSnapshot.Details = nil
			} else if len(modelSnapshot.Details) > limit {
				modelSnapshot.Details = modelSnapshot.Details[len(modelSnapshot.Details)-limit:]
			}
			apiSnapshot.Models[modelKey] = modelSnapshot
		}
		snapshot.APIs[apiKey] = apiSnapshot
	}

	return snapshot
}

func filterUsageSnapshotByTime(snapshot usage.StatisticsSnapshot, cutoff time.Time) usage.StatisticsSnapshot {
	if snapshot.APIs == nil {
		return snapshot
	}

	filteredAPIs := make(map[string]usage.APISnapshot, len(snapshot.APIs))
	var totalRequests, successCount, failureCount, totalTokens int64

	for apiKey, apiSnapshot := range snapshot.APIs {
		if apiSnapshot.Models == nil {
			continue
		}

		nextModels := make(map[string]usage.ModelSnapshot, len(apiSnapshot.Models))
		var apiRequests, apiSuccess, apiFailure, apiTokens int64

		for modelKey, modelSnapshot := range apiSnapshot.Models {
			if len(modelSnapshot.Details) == 0 {
				continue
			}

			filteredDetails := make([]usage.RequestDetail, 0, len(modelSnapshot.Details))
			var modelReq, modelSuccess, modelFailure, modelTokens int64

			for _, detail := range modelSnapshot.Details {
				if detail.Timestamp.Before(cutoff) {
					continue
				}
				filteredDetails = append(filteredDetails, detail)
				modelReq++
				if detail.Failed {
					modelFailure++
				} else {
					modelSuccess++
				}
				modelTokens += detail.Tokens.TotalTokens
			}

			if len(filteredDetails) == 0 {
				continue
			}

			modelSnapshot.Details = filteredDetails
			modelSnapshot.TotalRequests = modelReq
			modelSnapshot.TotalTokens = modelTokens
			nextModels[modelKey] = modelSnapshot

			apiRequests += modelReq
			apiSuccess += modelSuccess
			apiFailure += modelFailure
			apiTokens += modelTokens
		}

		if len(nextModels) == 0 {
			continue
		}

		apiSnapshot.Models = nextModels
		apiSnapshot.TotalRequests = apiRequests
		apiSnapshot.TotalTokens = apiTokens
		filteredAPIs[apiKey] = apiSnapshot

		totalRequests += apiRequests
		successCount += apiSuccess
		failureCount += apiFailure
		totalTokens += apiTokens
	}

	snapshot.APIs = filteredAPIs
	snapshot.TotalRequests = totalRequests
	snapshot.SuccessCount = successCount
	snapshot.FailureCount = failureCount
	snapshot.TotalTokens = totalTokens
	return snapshot
}

// ExportUsageStatistics returns a complete usage snapshot for backup/migration.
func (h *Handler) ExportUsageStatistics(c *gin.Context) {
	var snapshot usage.StatisticsSnapshot
	if h != nil && h.usageStats != nil {
		snapshot = h.usageStats.Snapshot()
	}
	c.JSON(http.StatusOK, usageExportPayload{
		Version:    1,
		ExportedAt: time.Now().UTC(),
		Usage:      snapshot,
	})
}

// ImportUsageStatistics merges a previously exported usage snapshot into memory.
func (h *Handler) ImportUsageStatistics(c *gin.Context) {
	if h == nil || h.usageStats == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage statistics unavailable"})
		return
	}

	data, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	var payload usageImportPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	if payload.Version != 0 && payload.Version != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported version"})
		return
	}

	result := h.usageStats.MergeSnapshot(payload.Usage)
	snapshot := h.usageStats.Snapshot()
	c.JSON(http.StatusOK, gin.H{
		"added":           result.Added,
		"skipped":         result.Skipped,
		"total_requests":  snapshot.TotalRequests,
		"failed_requests": snapshot.FailureCount,
	})
}
