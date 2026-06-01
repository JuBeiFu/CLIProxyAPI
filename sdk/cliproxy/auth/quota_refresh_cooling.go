package auth

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// refreshCodexJobTimeout bounds an entire async refresh pass. A pass re-probes
// up to a few hundred codex auths via /wham/usage; each probe has its own
// internal timeout, but this is the backstop so a hung probe can never leave
// the job (and the single-job slot) running forever. Exported as a var so tests
// can shorten it.
var refreshCodexJobTimeout = 15 * time.Minute

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

// refreshCodexTarget is a pre-refresh snapshot of a single codex auth target.
// It captures the BEFORE state (plan key, cooling flag) so post-refresh
// classification can compare against it.
type refreshCodexTarget struct {
	id, index, email, plan string
	cooling                bool
}

// RefreshCodexJobStatus is the lifecycle state of an async refresh job.
type RefreshCodexJobStatus string

const (
	RefreshJobRunning RefreshCodexJobStatus = "running"
	RefreshJobDone    RefreshCodexJobStatus = "done"
)

// RefreshCodexJob is the live progress + final result of an async refresh pass.
// Snapshot returns a deep-ish copy safe to serialize without holding the lock.
type RefreshCodexJob struct {
	mu           sync.Mutex
	ID           string                   `json:"job_id"`
	Status       RefreshCodexJobStatus    `json:"status"`
	CoolingOnly  bool                     `json:"cooling_only"`
	Total        int                      `json:"total"`
	Done         int                      `json:"done"`
	Recovered    int                      `json:"recovered"`
	Upgraded     int                      `json:"upgraded"`
	Downgraded   int                      `json:"downgraded"`
	StillCooling int                      `json:"still_cooling"`
	Errored      int                      `json:"errored"`
	StartedAt    time.Time                `json:"started_at"`
	FinishedAt   time.Time                `json:"finished_at,omitempty"`
	Results      []RefreshCodexAuthResult `json:"results,omitempty"`
}

// RefreshCodexJobView is a lock-free, JSON-serializable VALUE copy of a
// RefreshCodexJob. It omits the mutex so it can be copied/marshaled safely
// (copying a sync.Mutex by value trips go vet copylocks).
type RefreshCodexJobView struct {
	ID           string                   `json:"job_id"`
	Status       RefreshCodexJobStatus    `json:"status"`
	CoolingOnly  bool                     `json:"cooling_only"`
	Total        int                      `json:"total"`
	Done         int                      `json:"done"`
	Recovered    int                      `json:"recovered"`
	Upgraded     int                      `json:"upgraded"`
	Downgraded   int                      `json:"downgraded"`
	StillCooling int                      `json:"still_cooling"`
	Errored      int                      `json:"errored"`
	StartedAt    time.Time                `json:"started_at"`
	FinishedAt   time.Time                `json:"finished_at,omitempty"`
	Results      []RefreshCodexAuthResult `json:"results,omitempty"`
}

// snapshot returns a lock-free VALUE copy of the job, safe to serialize
// without holding the lock. It builds the view field-by-field (NOT *j) so the
// embedded mutex is never copied.
func (j *RefreshCodexJob) snapshot() RefreshCodexJobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	view := RefreshCodexJobView{
		ID:           j.ID,
		Status:       j.Status,
		CoolingOnly:  j.CoolingOnly,
		Total:        j.Total,
		Done:         j.Done,
		Recovered:    j.Recovered,
		Upgraded:     j.Upgraded,
		Downgraded:   j.Downgraded,
		StillCooling: j.StillCooling,
		Errored:      j.Errored,
		StartedAt:    j.StartedAt,
		FinishedAt:   j.FinishedAt,
	}
	if len(j.Results) > 0 {
		view.Results = make([]RefreshCodexAuthResult, len(j.Results))
		copy(view.Results, j.Results)
	}
	return view
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

// codexRefreshTargets builds the list of codex auths to re-probe. When
// coolingOnly is true, only currently-cooling auths are included. Each target
// captures the BEFORE plan/cooling state for post-refresh classification.
func (m *Manager) codexRefreshTargets(coolingOnly bool) []refreshCodexTarget {
	now := time.Now()
	snapshot := m.snapshotAuths()
	var targets []refreshCodexTarget
	for _, a := range snapshot {
		if a == nil || !isCodexAuth(a) {
			continue
		}
		cooling := authQuotaCoolingNow(a, now)
		if coolingOnly && !cooling {
			continue
		}
		_, email := a.AccountInfo()
		targets = append(targets, refreshCodexTarget{
			id:      a.ID,
			index:   a.Index,
			email:   email,
			plan:    normalizedPlanTypeKey(codexPlanTypeForAuth(a)),
			cooling: cooling,
		})
	}
	return targets
}

// refreshOneCodexTarget runs the bounded per-auth refresh for a single target
// (which runs /wham/usage and clears recovered cooldowns + updates plan_type),
// then classifies the post-refresh outcome. Concurrency is bounded by the
// existing 16-slot refresh semaphore inside refreshAuthWithLimit.
func (m *Manager) refreshOneCodexTarget(ctx context.Context, t refreshCodexTarget) RefreshCodexAuthResult {
	m.refreshAuthWithLimit(ctx, t.id)

	r := RefreshCodexAuthResult{AuthID: t.id, AuthIndex: t.index, Email: t.email, WasCooling: t.cooling, PlanBefore: t.plan}
	after, ok := m.GetByID(t.id)
	if !ok || after == nil {
		r.Error = "auth not found after refresh"
		return r
	}
	r.PlanAfter = normalizedPlanTypeKey(codexPlanTypeForAuth(after))
	stillCooling := authQuotaCoolingNow(after, time.Now())
	r.StillCooling = stillCooling
	if t.cooling && !stillCooling {
		r.Recovered = true
	}
	if r.PlanBefore != "" && r.PlanAfter != "" && r.PlanBefore != r.PlanAfter {
		if authPlanTierScore(after) > planTierScoreForKey(r.PlanBefore) {
			r.Upgraded = true
		} else if authPlanTierScore(after) < planTierScoreForKey(r.PlanBefore) {
			r.Downgraded = true
		}
	}
	return r
}

// RefreshCodexAuths re-probes codex auths via the existing per-auth refresh
// (which runs /wham/usage and clears recovered cooldowns + updates plan_type),
// then classifies each. When coolingOnly is true, only currently-cooling auths
// are probed. Concurrency is bounded by the existing refresh semaphore. This is
// the SYNCHRONOUS variant: it blocks until all targets are done.
func (m *Manager) RefreshCodexAuths(ctx context.Context, coolingOnly bool) RefreshCodexResult {
	var out RefreshCodexResult
	if m == nil {
		return out
	}
	if ctx == nil {
		ctx = context.Background()
	}
	targets := m.codexRefreshTargets(coolingOnly)

	out.Total = len(targets)
	results := make([]RefreshCodexAuthResult, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t refreshCodexTarget) {
			defer wg.Done()
			results[i] = m.refreshOneCodexTarget(ctx, t)
		}(i, t)
	}
	wg.Wait()

	out.Results = make([]RefreshCodexAuthResult, 0, len(targets))
	for _, r := range results {
		switch {
		case r.Error != "":
			out.Errored++
		default:
			if r.Recovered {
				out.Recovered++
			}
			if r.StillCooling {
				out.StillCooling++
			}
			if r.Upgraded {
				out.Upgraded++
			}
			if r.Downgraded {
				out.Downgraded++
			}
		}
		out.Results = append(out.Results, r)
	}
	return out
}

// StartRefreshCodexAuths starts an ASYNCHRONOUS refresh pass and returns the
// job id plus whether a new job was started. SINGLE-JOB model: if a job is
// already running, its id is returned with started=false.
func (m *Manager) StartRefreshCodexAuths(coolingOnly bool) (string, bool) {
	if m == nil {
		return "", false
	}
	m.refreshJobMu.Lock()
	if m.refreshJob != nil && m.refreshJob.Status == RefreshJobRunning && !refreshJobIsStale(m.refreshJob) {
		id := m.refreshJob.ID
		m.refreshJobMu.Unlock()
		return id, false
	}
	job := &RefreshCodexJob{
		ID:          uuid.NewString(),
		Status:      RefreshJobRunning,
		CoolingOnly: coolingOnly,
		StartedAt:   time.Now(),
	}
	m.refreshJob = job
	m.refreshJobMu.Unlock()

	go m.runRefreshCodexJob(job, coolingOnly)
	return job.ID, true
}

// refreshJobIsStale reports whether a still-"running" job has exceeded the
// job timeout by enough margin that it must be treated as dead (so it can never
// permanently block the single-job slot). Reads StartedAt under the job lock.
func refreshJobIsStale(job *RefreshCodexJob) bool {
	if job == nil {
		return false
	}
	job.mu.Lock()
	started := job.StartedAt
	status := job.Status
	job.mu.Unlock()
	if status != RefreshJobRunning || started.IsZero() {
		return false
	}
	return time.Since(started) > refreshCodexJobTimeout+time.Minute
}

// GetRefreshCodexJob returns a snapshot of the tracked refresh job. When jobID
// is empty the current/most-recent job is returned (if any).
func (m *Manager) GetRefreshCodexJob(jobID string) (RefreshCodexJobView, bool) {
	if m == nil {
		return RefreshCodexJobView{}, false
	}
	m.refreshJobMu.Lock()
	job := m.refreshJob
	m.refreshJobMu.Unlock()
	if job == nil {
		return RefreshCodexJobView{}, false
	}
	if jobID != "" && job.ID != jobID {
		return RefreshCodexJobView{}, false
	}
	return job.snapshot(), true
}

// runRefreshCodexJob executes the refresh pass for an async job, updating the
// job's live progress counters as each target completes. Concurrency stays
// bounded by the existing 16-slot refresh semaphore.
func (m *Manager) runRefreshCodexJob(job *RefreshCodexJob, coolingOnly bool) {
	ctx, cancel := context.WithTimeout(context.Background(), refreshCodexJobTimeout)
	defer cancel()
	targets := m.codexRefreshTargets(coolingOnly)

	job.mu.Lock()
	job.Total = len(targets)
	job.mu.Unlock()

	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(t refreshCodexTarget) {
			defer wg.Done()
			r := m.refreshOneCodexTarget(ctx, t)
			job.mu.Lock()
			job.Done++
			switch {
			case r.Error != "":
				job.Errored++
			default:
				if r.Recovered {
					job.Recovered++
				}
				if r.StillCooling {
					job.StillCooling++
				}
				if r.Upgraded {
					job.Upgraded++
				}
				if r.Downgraded {
					job.Downgraded++
				}
			}
			job.Results = append(job.Results, r)
			job.mu.Unlock()
		}(t)
	}
	wg.Wait()

	job.mu.Lock()
	job.Status = RefreshJobDone
	job.FinishedAt = time.Now()
	job.mu.Unlock()
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
