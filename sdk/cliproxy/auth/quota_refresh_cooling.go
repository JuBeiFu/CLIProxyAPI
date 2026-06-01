package auth

import (
	"context"
	"strings"
	"sync"
	"time"
)

// RefreshCodexAuthResult is the per-auth outcome of a manual refresh.
type RefreshCodexAuthResult struct {
	AuthID       string `json:"auth_id"`
	AuthIndex    string `json:"auth_index,omitempty"`
	Email        string `json:"email,omitempty"`
	WasCooling   bool   `json:"was_cooling"`
	StillCooling bool   `json:"still_cooling"`
	Recovered    bool   `json:"recovered"`
	PlanBefore   string `json:"plan_before,omitempty"`
	PlanAfter    string `json:"plan_after,omitempty"`
	Upgraded     bool   `json:"upgraded"`
	Downgraded   bool   `json:"downgraded"`
	Error        string `json:"error,omitempty"`
}

// RefreshCodexResult aggregates a manual refresh pass.
type RefreshCodexResult struct {
	Total        int                      `json:"total"`
	Recovered    int                      `json:"recovered"`
	Upgraded     int                      `json:"upgraded"`
	Downgraded   int                      `json:"downgraded"`
	StillCooling int                      `json:"still_cooling"`
	Errored      int                      `json:"errored"`
	Results      []RefreshCodexAuthResult `json:"results"`
}

// authQuotaCoolingNow reports whether an auth is currently blocked by quota
// cooldown (not operator-disabled). Mirrors the selector's cooldown checks.
func authQuotaCoolingNow(auth *Auth, now time.Time) bool {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
		return false
	}
	if auth.Quota.Exceeded && !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) {
		return true
	}
	if auth.Unavailable && auth.NextRetryAfter.After(now) {
		return true
	}
	return false
}

// RefreshCodexAuths re-probes codex auths via the existing per-auth refresh
// (which runs /wham/usage and clears recovered cooldowns + updates plan_type),
// then classifies each. When coolingOnly is true, only currently-cooling auths
// are probed. Concurrency is bounded by the existing refresh semaphore.
func (m *Manager) RefreshCodexAuths(ctx context.Context, coolingOnly bool) RefreshCodexResult {
	var out RefreshCodexResult
	if m == nil {
		return out
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now()
	snapshot := m.snapshotAuths()

	type pre struct {
		id, index, email, plan string
		cooling                bool
	}
	var targets []pre
	for _, a := range snapshot {
		if a == nil || !isCodexAuth(a) {
			continue
		}
		cooling := authQuotaCoolingNow(a, now)
		if coolingOnly && !cooling {
			continue
		}
		_, email := a.AccountInfo()
		targets = append(targets, pre{
			id:      a.ID,
			index:   a.Index,
			email:   email,
			plan:    normalizedPlanTypeKey(codexPlanTypeForAuth(a)),
			cooling: cooling,
		})
	}

	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(t pre) {
			defer wg.Done()
			m.refreshAuthWithLimit(ctx, t.id)
		}(t)
	}
	wg.Wait()

	out.Total = len(targets)
	out.Results = make([]RefreshCodexAuthResult, 0, len(targets))
	for _, t := range targets {
		r := RefreshCodexAuthResult{AuthID: t.id, AuthIndex: t.index, Email: t.email, WasCooling: t.cooling, PlanBefore: t.plan}
		after, ok := m.GetByID(t.id)
		if !ok || after == nil {
			r.Error = "auth not found after refresh"
			out.Errored++
			out.Results = append(out.Results, r)
			continue
		}
		r.PlanAfter = normalizedPlanTypeKey(codexPlanTypeForAuth(after))
		stillCooling := authQuotaCoolingNow(after, time.Now())
		r.StillCooling = stillCooling
		if t.cooling && !stillCooling {
			r.Recovered = true
			out.Recovered++
		}
		if stillCooling {
			out.StillCooling++
		}
		if r.PlanBefore != "" && r.PlanAfter != "" && r.PlanBefore != r.PlanAfter {
			if authPlanTierScore(after) > planTierScoreForKey(r.PlanBefore) {
				r.Upgraded = true
				out.Upgraded++
			} else if authPlanTierScore(after) < planTierScoreForKey(r.PlanBefore) {
				r.Downgraded = true
				out.Downgraded++
			}
		}
		out.Results = append(out.Results, r)
	}
	return out
}

// planTierScoreForKey maps a normalized plan key to the same tier score
// authPlanTierScore assigns, so before/after comparison uses one scale.
// It intentionally duplicates the tier mapping in authPlanTierScore
// (selector.go) because that one takes an *Auth and reads
// Attributes["plan_type"], whereas here we need to score a plain normalized
// key (the BEFORE snapshot, captured before refresh mutated the auth).
func planTierScoreForKey(key string) int {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "pro":
		return 4
	case "team", "business", "go":
		return 3
	case "plus", "prolite":
		return 2
	case "free":
		return 1
	default:
		return 0
	}
}
