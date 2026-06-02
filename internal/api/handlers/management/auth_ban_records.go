package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// ListAuthBanRecords returns ban records for a single day. Defaults to today;
// pass ?date=YYYY-MM-DD to query any historical day.
func (h *Handler) ListAuthBanRecords(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}

	day := time.Now()
	if q := strings.TrimSpace(c.Query("date")); q != "" {
		parsed, err := time.ParseInLocation("2006-01-02", q, time.Local)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid date, expected YYYY-MM-DD"})
			return
		}
		day = parsed
	}

	records, err := coreauth.LoadBanRecordsForDay(h.cfg.AuthDir, day)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"date":    day.Format("2006-01-02"),
		"total":   len(records),
		"records": records,
	})
}

// GetAuthAccountStats aggregates daily 上号(added) vs 封号(banned) counts plus
// ban breakdowns by plan / reason / source over a date range. Query params
// ?start=YYYY-MM-DD&end=YYYY-MM-DD (default: last 30 days through today).
func (h *Handler) GetAuthAccountStats(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}

	loc := time.Local
	end := time.Now()
	start := end.AddDate(0, 0, -29) // inclusive 30-day window by default

	if q := strings.TrimSpace(c.Query("end")); q != "" {
		parsed, err := time.ParseInLocation("2006-01-02", q, loc)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid end, expected YYYY-MM-DD"})
			return
		}
		end = parsed
	}
	if q := strings.TrimSpace(c.Query("start")); q != "" {
		parsed, err := time.ParseInLocation("2006-01-02", q, loc)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid start, expected YYYY-MM-DD"})
			return
		}
		start = parsed
	}

	addRecords, err := coreauth.LoadAddRecordsRange(h.cfg.AuthDir, start, end)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	banRecords, err := coreauth.LoadBanRecordsRange(h.cfg.AuthDir, start, end)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	stats := coreauth.BuildAccountStats(addRecords, banRecords, start, end, loc)
	c.JSON(http.StatusOK, stats)
}
