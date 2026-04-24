package management

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/performance"
)

type authPerformanceConfigResponse struct {
	Enabled        bool    `json:"enabled"`
	ShadowLog      bool    `json:"shadow_log"`
	WindowSeconds  int     `json:"window_seconds"`
	MinSamples     int     `json:"min_samples"`
	EWMAAlpha      float64 `json:"ewma_alpha"`
	WeightTPS      float64 `json:"weight_tps"`
	WeightLatency  float64 `json:"weight_latency"`
	WeightFailure  float64 `json:"weight_failure"`
	WeightInflight float64 `json:"weight_inflight"`
}

func (h *Handler) GetAuthPerformance(c *gin.Context) {
	filter := performance.SnapshotFilter{
		Provider: c.Query("provider"),
		Model:    c.Query("model"),
		AuthID:   c.Query("auth_id"),
	}
	if readyOnly, err := strconv.ParseBool(c.Query("ready_only")); err == nil {
		filter.ReadyOnly = readyOnly
	}

	snapshots := []performance.Snapshot{}
	if h != nil && h.performanceTracker != nil {
		snapshots = h.performanceTracker.Snapshot(filter, time.Now())
	}
	cfg := performance.DefaultConfig()
	if h != nil && h.cfg != nil {
		cfg.Enabled = h.cfg.Routing.PerformanceAware
		cfg.ShadowLog = h.cfg.Routing.PerformanceShadowLog
		cfg.Window = time.Duration(h.cfg.Routing.PerformanceWindowSeconds) * time.Second
		cfg.MinSamples = int64(h.cfg.Routing.PerformanceMinSamples)
		cfg.EWMAAlpha = h.cfg.Routing.PerformanceEWMAAlpha
		cfg.WeightTPS = h.cfg.Routing.PerformanceWeightTPS
		cfg.WeightLatency = h.cfg.Routing.PerformanceWeightLatency
		cfg.WeightFailure = h.cfg.Routing.PerformanceWeightFailure
		cfg.WeightInflight = h.cfg.Routing.PerformanceWeightInflight
		cfg = performance.NormalizeConfig(cfg)
	}

	c.JSON(http.StatusOK, gin.H{
		"performance": snapshots,
		"config": authPerformanceConfigResponse{
			Enabled:        cfg.Enabled,
			ShadowLog:      cfg.ShadowLog,
			WindowSeconds:  int(cfg.Window / time.Second),
			MinSamples:     int(cfg.MinSamples),
			EWMAAlpha:      cfg.EWMAAlpha,
			WeightTPS:      cfg.WeightTPS,
			WeightLatency:  cfg.WeightLatency,
			WeightFailure:  cfg.WeightFailure,
			WeightInflight: cfg.WeightInflight,
		},
	})
}
