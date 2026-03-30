package management

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type slowAuthView struct {
	ID                         string    `json:"id"`
	Index                      string    `json:"index,omitempty"`
	Provider                   string    `json:"provider,omitempty"`
	Label                      string    `json:"label,omitempty"`
	Status                     string    `json:"status,omitempty"`
	Disabled                   bool      `json:"disabled"`
	Unavailable                bool      `json:"unavailable"`
	ProxyURL                   string    `json:"proxy_url,omitempty"`
	SlowRequestPriorityPenalty int       `json:"slow_request_priority_penalty"`
	SlowRequestWindowStartedAt time.Time `json:"slow_request_window_started_at,omitempty"`
	SlowRequestWindowCount     int       `json:"slow_request_window_count"`
	SlowRequestCooldownUntil   time.Time `json:"slow_request_cooldown_until,omitempty"`
	LastObservedLatencyMS      int64     `json:"last_observed_latency_ms"`
	QuotaPriorityPenalty       int       `json:"quota_priority_penalty"`
	TransientCooldownUntil     time.Time `json:"transient_cooldown_until,omitempty"`
	QuotaExceeded              bool      `json:"quota_exceeded"`
	QuotaReason                string    `json:"quota_reason,omitempty"`
	QuotaNextRecoverAt         time.Time `json:"quota_next_recover_at,omitempty"`
}

func (h *Handler) GetSlowAuths(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusOK, gin.H{
			"items": []slowAuthView{},
			"count": 0,
		})
		return
	}

	activeOnly := true
	if raw := strings.TrimSpace(c.Query("active_only")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid active_only"})
			return
		}
		activeOnly = parsed
	}

	cooldownOnly := false
	if raw := strings.TrimSpace(c.Query("cooldown_only")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid cooldown_only"})
			return
		}
		cooldownOnly = parsed
	}

	authID := strings.TrimSpace(c.Query("auth_id"))
	providerFilter := strings.ToLower(strings.TrimSpace(c.Query("provider")))
	now := time.Now()

	items := make([]slowAuthView, 0)
	for _, auth := range h.authManager.List() {
		if auth == nil {
			continue
		}
		if authID != "" && auth.ID != authID {
			continue
		}
		if providerFilter != "" && strings.ToLower(strings.TrimSpace(auth.Provider)) != providerFilter {
			continue
		}

		auth.EnsureIndex()
		view := slowAuthView{
			ID:                         auth.ID,
			Index:                      auth.Index,
			Provider:                   auth.Provider,
			Label:                      auth.Label,
			Status:                     string(auth.Status),
			Disabled:                   auth.Disabled,
			Unavailable:                auth.Unavailable,
			ProxyURL:                   auth.ProxyURL,
			SlowRequestPriorityPenalty: auth.SlowRequestPriorityPenalty,
			SlowRequestWindowStartedAt: auth.SlowRequestWindowStartedAt,
			SlowRequestWindowCount:     auth.SlowRequestWindowCount,
			SlowRequestCooldownUntil:   auth.SlowRequestCooldownUntil,
			LastObservedLatencyMS:      auth.LastObservedLatency.Milliseconds(),
			QuotaPriorityPenalty:       auth.QuotaPriorityPenalty,
			TransientCooldownUntil:     auth.TransientCooldownUntil,
			QuotaExceeded:              auth.Quota.Exceeded,
			QuotaReason:                auth.Quota.Reason,
			QuotaNextRecoverAt:         auth.Quota.NextRecoverAt,
		}

		activeSlow := view.SlowRequestPriorityPenalty > 0 || view.SlowRequestWindowCount > 0 || view.SlowRequestCooldownUntil.After(now)
		activeCooldown := view.SlowRequestCooldownUntil.After(now)
		if cooldownOnly && !activeCooldown {
			continue
		}
		if activeOnly && !activeSlow {
			continue
		}
		items = append(items, view)
	}

	sort.SliceStable(items, func(i, j int) bool {
		left := items[i]
		right := items[j]
		switch {
		case left.SlowRequestCooldownUntil.After(now) && !right.SlowRequestCooldownUntil.After(now):
			return true
		case !left.SlowRequestCooldownUntil.After(now) && right.SlowRequestCooldownUntil.After(now):
			return false
		case left.SlowRequestPriorityPenalty != right.SlowRequestPriorityPenalty:
			return left.SlowRequestPriorityPenalty > right.SlowRequestPriorityPenalty
		case left.LastObservedLatencyMS != right.LastObservedLatencyMS:
			return left.LastObservedLatencyMS > right.LastObservedLatencyMS
		case left.SlowRequestWindowCount != right.SlowRequestWindowCount:
			return left.SlowRequestWindowCount > right.SlowRequestWindowCount
		default:
			return left.ID < right.ID
		}
	})

	c.JSON(http.StatusOK, gin.H{
		"items": items,
		"count": len(items),
	})
}
