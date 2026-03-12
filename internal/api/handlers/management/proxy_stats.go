package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// GetProxyStatistics returns the in-memory outbound proxy statistics snapshot.
func (h *Handler) GetProxyStatistics(c *gin.Context) {
	if h == nil || h.proxyStats == nil {
		c.JSON(http.StatusOK, gin.H{"proxy_stats": gin.H{}})
		return
	}
	snapshot := h.proxyStats.Snapshot()
	c.JSON(http.StatusOK, gin.H{
		"proxy_stats": snapshot,
	})
}
