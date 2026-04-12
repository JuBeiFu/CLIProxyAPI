package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func (h *Handler) GetDefaultProxyPool(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"default-proxy-pool": h.cfg.DefaultProxyPool})
}

func (h *Handler) PutDefaultProxyPool(c *gin.Context) {
	h.updateStringField(c, func(v string) { h.cfg.DefaultProxyPool = strings.TrimSpace(v) })
}

func (h *Handler) DeleteDefaultProxyPool(c *gin.Context) {
	h.cfg.DefaultProxyPool = ""
	h.persist(c)
}

func (h *Handler) GetProxyPools(c *gin.Context) {
	results := h.proxyHealth.PoolStatuses(h.cfg.ProxyPools, "")
	c.JSON(http.StatusOK, gin.H{
		"default-proxy-pool": h.cfg.DefaultProxyPool,
		"proxy-pools":        h.cfg.ProxyPools,
		"proxy-health": gin.H{
			"results": results,
		},
	})
}

func (h *Handler) PutProxyPools(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}
	var pools []config.ProxyPool
	if err = json.Unmarshal(data, &pools); err != nil {
		var body struct {
			Items []config.ProxyPool `json:"items"`
		}
		if errBody := json.Unmarshal(data, &body); errBody != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		pools = body.Items
	}
	h.cfg.ProxyPools = config.NormalizeProxyPools(pools)
	h.persist(c)
}

func (h *Handler) PatchProxyPools(c *gin.Context) {
	var body struct {
		Index *int              `json:"index"`
		Name  *string           `json:"name"`
		Value *config.ProxyPool `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	targetIndex := -1
	if body.Index != nil && *body.Index >= 0 && *body.Index < len(h.cfg.ProxyPools) {
		targetIndex = *body.Index
	}
	if targetIndex == -1 && body.Name != nil {
		match := strings.TrimSpace(*body.Name)
		for i := range h.cfg.ProxyPools {
			if strings.EqualFold(strings.TrimSpace(h.cfg.ProxyPools[i].Name), match) {
				targetIndex = i
				break
			}
		}
	}
	if targetIndex == -1 {
		c.JSON(http.StatusNotFound, gin.H{"error": "proxy pool not found"})
		return
	}

	next := config.NormalizeProxyPools([]config.ProxyPool{*body.Value})
	if len(next) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid proxy pool"})
		return
	}
	h.cfg.ProxyPools[targetIndex] = next[0]
	h.cfg.ProxyPools = config.NormalizeProxyPools(h.cfg.ProxyPools)
	h.persist(c)
}

func (h *Handler) DeleteProxyPools(c *gin.Context) {
	if idxRaw := strings.TrimSpace(c.Query("index")); idxRaw != "" {
		var idx int
		if _, err := fmt.Sscanf(idxRaw, "%d", &idx); err == nil && idx >= 0 && idx < len(h.cfg.ProxyPools) {
			h.cfg.ProxyPools = append(h.cfg.ProxyPools[:idx], h.cfg.ProxyPools[idx+1:]...)
			h.cfg.ProxyPools = config.NormalizeProxyPools(h.cfg.ProxyPools)
			h.persist(c)
			return
		}
	}
	if name := strings.TrimSpace(c.Query("name")); name != "" {
		out := make([]config.ProxyPool, 0, len(h.cfg.ProxyPools))
		for _, pool := range h.cfg.ProxyPools {
			if strings.EqualFold(strings.TrimSpace(pool.Name), name) {
				continue
			}
			out = append(out, pool)
		}
		h.cfg.ProxyPools = config.NormalizeProxyPools(out)
		h.persist(c)
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": "missing index or name"})
}

func (h *Handler) CheckProxyPools(c *gin.Context) {
	var body struct {
		Name *string `json:"name"`
	}
	_ = c.ShouldBindJSON(&body)

	targetName := ""
	if body.Name != nil {
		targetName = strings.TrimSpace(*body.Name)
	}

	results := h.proxyHealth.CheckPools(c.Request.Context(), h.cfg.ProxyPools, targetName)

	c.JSON(http.StatusOK, gin.H{
		"default-proxy-pool": h.cfg.DefaultProxyPool,
		"results":            results,
	})
}
