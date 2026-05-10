package auth

import (
	"context"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/performance"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type fakePerformanceScorer struct {
	mu         sync.Mutex
	candidates map[string]performance.ScoreCandidate
	snapshots  []performance.ScoreCandidate
	scored     []performance.ScoreCandidate
}

func (s *fakePerformanceScorer) SnapshotCandidate(provider, authID, model string, inflight int) performance.ScoreCandidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	candidate := s.candidates[authID]
	candidate.Provider = provider
	candidate.AuthID = authID
	candidate.Model = model
	candidate.Inflight = inflight
	s.snapshots = append(s.snapshots, candidate)
	return candidate
}

func (s *fakePerformanceScorer) ScoreCandidates(candidates []performance.ScoreCandidate, cfg performance.Config) []performance.Score {
	s.mu.Lock()
	s.scored = append([]performance.ScoreCandidate(nil), candidates...)
	s.mu.Unlock()
	return performance.NewScorer(nil).ScoreCandidates(candidates, cfg)
}

func (s *fakePerformanceScorer) snapshotCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.snapshots)
}

func (s *fakePerformanceScorer) scoredAuthIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.scored))
	for _, candidate := range s.scored {
		out = append(out, candidate.AuthID)
	}
	return out
}

func TestSchedulerPerformanceShadowKeepsRoundRobinSelection(t *testing.T) {
	model := "gpt-5.4"
	registerSchedulerModels(t, "codex", model, "auth-a", "auth-b")
	scorer := &fakePerformanceScorer{
		candidates: map[string]performance.ScoreCandidate{
			"auth-a": {OutputTPSEWMA: 10, LatencyMsEWMA: 100, SampleReady: true},
			"auth-b": {OutputTPSEWMA: 40, LatencyMsEWMA: 40, SampleReady: true},
		},
	}
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "auth-a", Provider: "codex"},
		&Auth{ID: "auth-b", Provider: "codex"},
	)
	cfg := performance.DefaultConfig()
	cfg.Enabled = false
	cfg.ShadowLog = true
	scheduler.setPerformanceRoutingConfig(cfg)
	scheduler.setPerformanceScorer(scorer)

	got, errPick := scheduler.pickSingle(context.Background(), "codex", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	if got == nil {
		t.Fatal("pickSingle() auth = nil")
	}
	if got.ID != "auth-a" {
		t.Fatalf("pickSingle() auth.ID = %q, want %q", got.ID, "auth-a")
	}
	if count := scorer.snapshotCount(); count != 2 {
		t.Fatalf("snapshot count = %d, want %d", count, 2)
	}
	if gotIDs := scorer.scoredAuthIDs(); len(gotIDs) != 2 || gotIDs[0] != "auth-a" || gotIDs[1] != "auth-b" {
		t.Fatalf("scored auth IDs = %v, want [auth-a auth-b]", gotIDs)
	}
}

func TestSchedulerPerformanceDisabledSkipsScorer(t *testing.T) {
	model := "gpt-5.4"
	registerSchedulerModels(t, "codex", model, "auth-a", "auth-b")
	scorer := &fakePerformanceScorer{
		candidates: map[string]performance.ScoreCandidate{
			"auth-a": {OutputTPSEWMA: 10, LatencyMsEWMA: 100, SampleReady: true},
			"auth-b": {OutputTPSEWMA: 40, LatencyMsEWMA: 40, SampleReady: true},
		},
	}
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "auth-a", Provider: "codex"},
		&Auth{ID: "auth-b", Provider: "codex"},
	)
	cfg := performance.DefaultConfig()
	cfg.Enabled = false
	cfg.ShadowLog = false
	scheduler.setPerformanceRoutingConfig(cfg)
	scheduler.setPerformanceScorer(scorer)

	got, errPick := scheduler.pickSingle(context.Background(), "codex", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	if got == nil || got.ID != "auth-a" {
		t.Fatalf("pickSingle() auth = %v, want auth-a", got)
	}
	if count := scorer.snapshotCount(); count != 0 {
		t.Fatalf("snapshot count = %d, want 0", count)
	}
	if gotIDs := scorer.scoredAuthIDs(); len(gotIDs) != 0 {
		t.Fatalf("scored auth IDs = %v, want empty", gotIDs)
	}
}

func TestSchedulerPerformanceEnabledSelectsFasterSamePriorityAuth(t *testing.T) {
	model := "gpt-5.4"
	registerSchedulerModels(t, "codex", model, "auth-a", "auth-b")
	scorer := &fakePerformanceScorer{
		candidates: map[string]performance.ScoreCandidate{
			"auth-a": {OutputTPSEWMA: 10, LatencyMsEWMA: 100, SampleReady: true},
			"auth-b": {OutputTPSEWMA: 40, LatencyMsEWMA: 40, SampleReady: true},
		},
	}
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "auth-a", Provider: "codex"},
		&Auth{ID: "auth-b", Provider: "codex"},
	)
	cfg := performance.DefaultConfig()
	cfg.Enabled = true
	cfg.ShadowLog = true
	scheduler.setPerformanceRoutingConfig(cfg)
	scheduler.setPerformanceScorer(scorer)

	got, errPick := scheduler.pickSingle(context.Background(), "codex", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	if got == nil {
		t.Fatal("pickSingle() auth = nil")
	}
	if got.ID != "auth-b" {
		t.Fatalf("pickSingle() auth.ID = %q, want %q", got.ID, "auth-b")
	}
}

func TestSchedulerPerformanceRespectsTriedAuthExclusion(t *testing.T) {
	model := "gpt-5.4"
	registerSchedulerModels(t, "codex", model, "auth-a", "auth-b")
	scorer := &fakePerformanceScorer{
		candidates: map[string]performance.ScoreCandidate{
			"auth-a": {OutputTPSEWMA: 100, LatencyMsEWMA: 40, SampleReady: true},
			"auth-b": {OutputTPSEWMA: 10, LatencyMsEWMA: 100, SampleReady: true},
		},
	}
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "auth-a", Provider: "codex"},
		&Auth{ID: "auth-b", Provider: "codex"},
	)
	cfg := performance.DefaultConfig()
	cfg.Enabled = true
	scheduler.setPerformanceRoutingConfig(cfg)
	scheduler.setPerformanceScorer(scorer)

	got, errPick := scheduler.pickSingle(context.Background(), "codex", model, cliproxyexecutor.Options{}, map[string]struct{}{"auth-a": {}})
	if errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	if got == nil {
		t.Fatal("pickSingle() auth = nil")
	}
	if got.ID != "auth-b" {
		t.Fatalf("pickSingle() auth.ID = %q, want %q", got.ID, "auth-b")
	}
}

func TestSchedulerPerformanceAllColdUsesRoundRobin(t *testing.T) {
	model := "gpt-5.4"
	registerSchedulerModels(t, "codex", model, "auth-a", "auth-b")
	scorer := &fakePerformanceScorer{
		candidates: map[string]performance.ScoreCandidate{
			"auth-a": {},
			"auth-b": {},
		},
	}
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "auth-a", Provider: "codex"},
		&Auth{ID: "auth-b", Provider: "codex"},
	)
	cfg := performance.DefaultConfig()
	cfg.Enabled = true
	cfg.MinSamples = 5
	scheduler.setPerformanceRoutingConfig(cfg)
	scheduler.setPerformanceScorer(scorer)

	first, errPick := scheduler.pickSingle(context.Background(), "codex", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("first pickSingle() error = %v", errPick)
	}
	second, errPick := scheduler.pickSingle(context.Background(), "codex", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("second pickSingle() error = %v", errPick)
	}
	if first == nil || second == nil {
		t.Fatalf("picks = %v, %v; want auth-a, auth-b", first, second)
	}
	if first.ID != "auth-a" || second.ID != "auth-b" {
		t.Fatalf("pick IDs = %s, %s; want auth-a, auth-b", first.ID, second.ID)
	}
}

func TestManagerPerformanceRoutingConfigPropagatesToScheduler(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	cfg := performance.DefaultConfig()
	cfg.Enabled = true
	cfg.ShadowLog = false
	cfg.MinSamples = 9
	scorer := &fakePerformanceScorer{}

	manager.SetPerformanceRoutingConfig(cfg)
	manager.SetPerformanceScorer(scorer)

	if manager.scheduler == nil {
		t.Fatal("manager.scheduler = nil")
	}
	manager.scheduler.mu.Lock()
	gotCfg := manager.scheduler.performanceRoutingConfig
	gotScorer := manager.scheduler.performanceScorer
	manager.scheduler.mu.Unlock()
	if gotCfg.MinSamples != 9 || !gotCfg.Enabled || gotCfg.ShadowLog {
		t.Fatalf("scheduler performance config = %+v, want enabled with min_samples 9 and shadow_log false", gotCfg)
	}
	if gotScorer != scorer {
		t.Fatalf("scheduler performance scorer = %T, want fakePerformanceScorer", gotScorer)
	}
}
