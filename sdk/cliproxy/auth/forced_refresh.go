package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// DefaultForcedRefreshInterval is the short-cycle cadence at which we probe
// "submitted paid but live-state free" auths to catch subscription activation
// (or keep them disabled if it never arrives). 5 minutes is a tradeoff
// between responsiveness and upstream/token-rotation pressure.
const DefaultForcedRefreshInterval = 5 * time.Minute

// DefaultDowngradeDeletionGrace is how long an auth may linger in the
// disabled-by-downgrade state before the forced loop removes it permanently.
// Accounts that briefly flip back to paid have their timestamp cleared by
// ApplyPlanTypeRefreshDecision, so only continuously-free accounts get deleted.
const DefaultDowngradeDeletionGrace = 2 * time.Hour

// shouldForceRefresh returns true iff the auth is within the short-cycle
// scope: codex provider, submitted plan_type is paid, AND probed plan_type is
// either unknown or free. Disabled is irrelevant — disabled auths MUST be
// refreshed so they can be re-enabled on activation.
func shouldForceRefresh(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	submitted := submittedPlanType(auth)
	if !isPaidPlan(submitted) {
		return false
	}
	probed := probedPlanType(auth)
	return probed == "" || isFreePlan(probed)
}

// shouldDeleteDowngradedAuth decides whether a forced-refresh pass should
// permanently delete an auth. Requires:
//   - disabled AND StatusMessage bears our downgrade prefix (we own the
//     disable; never delete auths disabled for other reasons)
//   - downgradeDetectedAt recorded and (now - recorded) > grace
//   - current probed plan_type still looks free (upgrade hasn't landed)
func shouldDeleteDowngradedAuth(auth *Auth, now time.Time, grace time.Duration) bool {
	if auth == nil {
		return false
	}
	if !auth.Disabled {
		return false
	}
	if !strings.HasPrefix(auth.StatusMessage, downgradeDetectedPrefix) {
		return false
	}
	if !isFreePlan(probedPlanType(auth)) {
		return false
	}
	ts, ok := downgradeDetectedAt(auth)
	if !ok {
		return false
	}
	elapsed := now.Sub(ts)
	return elapsed > grace
}

// StartAutoForcedRefresh launches the short-cycle refresh loop used to probe
// "submitted paid but currently free" auths. Unlike StartAutoRefresh this
// loop (a) bypasses the Disabled-skip gate so disabled-by-downgrade auths can
// be re-enabled on activation, and (b) is scoped to a narrow slice of the
// pool via shouldForceRefresh, so it does not burn refresh_tokens on the
// majority of auths.
//
// interval <= 0 falls back to DefaultForcedRefreshInterval. grace <= 0 falls
// back to DefaultDowngradeDeletionGrace.
func (m *Manager) StartAutoForcedRefresh(parent context.Context, interval, grace time.Duration) {
	if m == nil {
		return
	}
	if interval <= 0 {
		interval = DefaultForcedRefreshInterval
	}
	if grace <= 0 {
		grace = DefaultDowngradeDeletionGrace
	}
	if m.forcedRefreshCancel != nil {
		m.forcedRefreshCancel()
		m.forcedRefreshCancel = nil
	}
	ctx, cancel := context.WithCancel(parent)
	m.forcedRefreshCancel = cancel
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		m.runForcedRefreshOnce(ctx, grace)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.runForcedRefreshOnce(ctx, grace)
			}
		}
	}()
}

// StopAutoForcedRefresh cancels the forced-refresh loop if running.
func (m *Manager) StopAutoForcedRefresh() {
	if m == nil {
		return
	}
	if m.forcedRefreshCancel != nil {
		m.forcedRefreshCancel()
		m.forcedRefreshCancel = nil
	}
}

// runForcedRefreshOnce performs a single pass of the forced-refresh loop:
//  1. Snapshot all auths; filter to the shouldForceRefresh scope.
//  2. For each scope auth, if the downgrade grace window has elapsed (using
//     the LAST probe result, at most one interval stale), permanently delete
//     it and skip further probing.
//  3. Otherwise, reserve a refresh slot that ignores Disabled but honors
//     NextRefreshAfter (to avoid racing the regular refresh loop), then
//     synchronously invoke refreshAuth to trigger a fresh token + probe.
func (m *Manager) runForcedRefreshOnce(ctx context.Context, grace time.Duration) {
	if m == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	snapshot := m.snapshotAuths()
	now := time.Now()
	for _, a := range snapshot {
		if ctx.Err() != nil {
			return
		}
		if !shouldForceRefresh(a) {
			continue
		}
		// Delete-eligibility check BEFORE refresh: the last probe is at most
		// one interval stale; if it reported free AND the grace window has
		// elapsed, that is authoritative enough to remove the auth.
		if shouldDeleteDowngradedAuth(a, now, grace) {
			m.deleteForcedRefreshExpired(a.ID, grace)
			continue
		}
		if m.executorFor(a.Provider) == nil {
			continue
		}
		if !m.reserveForcedRefreshSlot(a.ID, now) {
			continue
		}
		m.refreshAuth(ctx, a.ID)
	}
}

// reserveForcedRefreshSlot is the forced-loop equivalent of markRefreshPending,
// differing only in that it does NOT skip disabled auths (re-enable path).
// Still honors NextRefreshAfter so concurrent regular refreshes can't race.
func (m *Manager) reserveForcedRefreshSlot(id string, now time.Time) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	auth, ok := m.auths[id]
	if !ok || auth == nil {
		return false
	}
	if !auth.NextRefreshAfter.IsZero() && now.Before(auth.NextRefreshAfter) {
		return false
	}
	auth.NextRefreshAfter = now.Add(refreshPendingBackoff)
	m.auths[id] = auth
	return true
}

func (m *Manager) deleteForcedRefreshExpired(id string, grace time.Duration) {
	m.mu.Lock()
	current := m.auths[id]
	if current == nil {
		m.mu.Unlock()
		return
	}
	removed := current.Clone()
	delete(m.auths, id)
	m.mu.Unlock()
	log.Warnf("codex forced-refresh: deleting auth %s (downgrade persisted >%s): %s",
		id, grace, removed.StatusMessage)
	reason := fmt.Sprintf("codex_downgrade_grace_expired (>%s)", grace)
	m.deleteRevokedAuth(removed, reason, "forced_refresh")
}
