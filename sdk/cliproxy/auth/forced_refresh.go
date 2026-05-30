package auth

import (
	"context"
	"fmt"
	"strings"
	"sync"
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

// DefaultBoundReprobeInterval is how old a bound-paid auth's last refresh
// must be before the forced loop re-probes it. OpenAI's per-edge plan_type
// cache can flip plus→free on an already-bound account over time (account
// cancellation, billing lag, risk flags); the binding points to a specific
// edge so we must re-walk the pool periodically to catch silent flips.
//
// Empirically observed 2026-04-21: for newly-registered / just-upgraded
// accounts the OpenAI edge caches are VOLATILE — a single bound path can
// flip plus→free within 2–3 minutes (verified across 5 samples on
// cli-proxy-api-free). Setting this equal to the forced-refresh tick
// interval (5min) means every paid+bound auth gets re-validated on every
// tick, keeping the cached probed/bound state within one tick of reality.
// Probe cost per tick ≈ N probes (break on first paid), so ~200 accounts
// at ~1-5 HTTP calls each ≈ 1 req/sec average against OpenAI — well
// within budget.
const DefaultBoundReprobeInterval = 5 * time.Minute

// DefaultPendingActivationProbeInterval is the retry cadence for newly
// imported free Codex session auths that expose only the weekly usage window.
const DefaultPendingActivationProbeInterval = 5 * time.Minute

// DefaultPendingActivationDeletionGrace is the maximum age for a free
// weekly-only Codex session auth to wait for activation before removal.
const DefaultPendingActivationDeletionGrace = 4 * time.Hour

// BoundProxyHealthChecker, when non-nil, is consulted by shouldForceRefresh
// to also bring back into scope any codex auth whose pinned proxy entry is
// no longer usable (disabled in config, removed from pool, or marked
// unhealthy by the proxy HealthManager). Returning true triggers a full
// multi-path re-probe which will select a new binding.
//
// The registration lives in the sdk/cliproxy layer because that's where
// proxy config and HealthManager are reachable; forced_refresh lives one
// layer below and cannot import them directly.
var BoundProxyHealthChecker func(*Auth) bool

// shouldForceRefresh returns true iff the auth is within the short-cycle
// scope. Disabled is irrelevant — disabled auths MUST be refreshed so they
// can be re-enabled on activation. Rules:
//
//  1. Never-probed codex auths (probed == "") are always in scope, regardless
//     of submitted, so every codex auth gets probed at least once shortly
//     after import/startup. After that first probe the decision fn backfills
//     the submitted pin and writes probed, so the auth naturally exits (or
//     stays in) scope on the next check.
//  2. Already-probed codex auths stay in scope when submitted is paid AND
//     probed is free — the re-activation-watching state.
//  3. Already-probed codex auths with probed=paid but NO bound proxy entry
//     are in scope so the multi-path probe runs and pins a usable node.
//     Without this, auths that were probed under the pre-binding code
//     (single path, no binding persisted) would never get a binding and
//     would continue being dispatched via FNV-hash — which lands on a
//     node that might currently return free, masking them as free in
//     practice even though cached probed=plus.
//  4. Already-probed codex auths are ALSO in scope when their bound pool
//     entry has become unusable (dead proxy / config removal). The probe
//     re-picks a healthy binding, keeping the auth serving without an
//     operator touch.
//  5. Already-probed codex auths with probed=paid AND a binding that has
//     gone stale (>DefaultBoundReprobeInterval since last refresh) are in
//     scope so we can re-validate against OpenAI's per-edge plan_type
//     cache. OpenAI does flip plus→free on accounts silently (cancellation,
//     risk flags, billing lag); without this periodic re-probe those auths
//     would be dispatched as plus while actually serving free quota until
//     the access_token finally expires 10 days later.
//  6. Non-codex providers are never in scope.
func shouldForceRefresh(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if hasRevokedAuthTombstoneMemory(auth, time.Now()) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	if isCodexPendingActivationAuth(auth) {
		return true
	}
	probed := probedPlanType(auth)
	if probed == "" {
		return true
	}
	submitted := submittedPlanType(auth)
	if isPaidPlan(submitted) && isFreePlan(probed) {
		return true
	}
	if isPaidPlan(probed) && BoundProxyEntry(auth) == "" && IPv6BindLease(auth).URL == "" {
		return true
	}
	if BoundProxyHealthChecker != nil && BoundProxyHealthChecker(auth) {
		return true
	}
	// Rule 5: bound paid + stale last-refresh. File-loaded auths don't
	// populate the in-memory LastRefreshedAt field directly — it's the
	// Metadata["last_refresh"] string that persists across reloads — so we
	// mirror shouldRefresh's fallback and consult both. Without this
	// fallback the rule would never fire for any auth loaded from disk
	// (their LastRefreshedAt is zero until they're refreshed in-process),
	// leaving stale bindings uncaught until some other trigger fires.
	if isPaidPlan(probed) && (BoundProxyEntry(auth) != "" || IPv6BindLease(auth).URL != "") {
		lastRefresh := auth.LastRefreshedAt
		if lastRefresh.IsZero() {
			if ts, ok := authLastRefreshTimestamp(auth); ok {
				lastRefresh = ts
			}
		}
		if !lastRefresh.IsZero() && time.Since(lastRefresh) >= DefaultBoundReprobeInterval {
			return true
		}
	}
	return false
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
	var wg sync.WaitGroup
	for _, a := range snapshot {
		if ctx.Err() != nil {
			break
		}
		if !shouldForceRefresh(a) {
			continue
		}
		if shouldDeleteCodexPendingActivationAuth(a, now, DefaultPendingActivationDeletionGrace) {
			m.deletePendingActivationExpired(a.ID, DefaultPendingActivationDeletionGrace)
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
		// Parallel via refreshSemaphore — same cap as the regular refresh
		// loop. A 194-auth initial pass drops from ~8 min (sequential) to
		// ~25s at concurrency 16 (two HTTP RTTs per refresh). We wait for
		// all spawned refreshes before returning so (a) tests can observe
		// results deterministically, and (b) the next 5m tick never piles
		// up on top of a still-running pass.
		id := a.ID
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.refreshAuthWithLimit(ctx, id)
		}()
	}
	wg.Wait()
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
	if hasRevokedAuthTombstoneMemory(auth, now) {
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

func (m *Manager) deletePendingActivationExpired(id string, grace time.Duration) {
	m.mu.Lock()
	current := m.auths[id]
	if current == nil {
		m.mu.Unlock()
		return
	}
	if !isCodexPendingActivationAuth(current) || !shouldDeleteCodexPendingActivationAuth(current, time.Now(), grace) {
		m.mu.Unlock()
		return
	}
	removed := current.Clone()
	delete(m.auths, id)
	m.mu.Unlock()
	log.Warnf("codex forced-refresh: deleting auth %s (pending activation persisted >=%s)", id, grace)
	reason := fmt.Sprintf("%s_expired (>= %s)", codexPendingActivationReason, grace)
	m.deleteRevokedAuth(removed, reason, "forced_refresh")
}
