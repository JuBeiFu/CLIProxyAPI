package management

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func (h *Handler) ListAuthBanRecords(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}

	now := time.Now()
	records, err := coreauth.LoadBanRecordsForDay(h.cfg.AuthDir, now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"date":    now.Format("2006-01-02"),
		"total":   len(records),
		"records": records,
	})
}
