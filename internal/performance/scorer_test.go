package performance

import (
	"math"
	"testing"
)

func TestScoreCandidatesPrefersHigherTPS(t *testing.T) {
	tracker := NewTracker(Config{MinSamples: 1, EWMAAlpha: 1, WeightTPS: 1, WeightLatency: 0.25, WeightFailure: 2, WeightInflight: 0.5})
	scorer := NewScorer(tracker)
	candidates := []ScoreCandidate{
		{Provider: "codex", AuthID: "slow", Model: "gpt-5.4", OutputTPSEWMA: 10, LatencyMsEWMA: 1000, SampleReady: true},
		{Provider: "codex", AuthID: "fast", Model: "gpt-5.4", OutputTPSEWMA: 40, LatencyMsEWMA: 1000, SampleReady: true},
	}

	scores := scorer.ScoreCandidates(candidates, DefaultConfig())
	if len(scores) != 2 {
		t.Fatalf("scores = %d, want 2", len(scores))
	}
	if scores[0].AuthID != "fast" {
		t.Fatalf("top auth = %s, want fast; scores=%+v", scores[0].AuthID, scores)
	}
}

func TestScoreCandidatesPenalizesLatencyFailureAndInflight(t *testing.T) {
	scorer := NewScorer(NewTracker(DefaultConfig()))
	cfg := DefaultConfig()
	candidates := []ScoreCandidate{
		{Provider: "codex", AuthID: "clean", Model: "gpt-5.4", OutputTPSEWMA: 30, LatencyMsEWMA: 1000, FailureRateEWMA: 0, Inflight: 0, SampleReady: true},
		{Provider: "codex", AuthID: "busy", Model: "gpt-5.4", OutputTPSEWMA: 30, LatencyMsEWMA: 5000, FailureRateEWMA: 0.5, Inflight: 3, SampleReady: true},
	}

	scores := scorer.ScoreCandidates(candidates, cfg)
	if scores[0].AuthID != "clean" {
		t.Fatalf("top auth = %s, want clean; scores=%+v", scores[0].AuthID, scores)
	}
}

func TestScoreCandidatesColdFallbackAndAllCold(t *testing.T) {
	scorer := NewScorer(NewTracker(DefaultConfig()))
	cfg := DefaultConfig()
	mixed := []ScoreCandidate{
		{Provider: "codex", AuthID: "ready-a", Model: "gpt-5.4", OutputTPSEWMA: 10, LatencyMsEWMA: 1000, SampleReady: true},
		{Provider: "codex", AuthID: "ready-b", Model: "gpt-5.4", OutputTPSEWMA: 30, LatencyMsEWMA: 1000, SampleReady: true},
		{Provider: "codex", AuthID: "cold", Model: "gpt-5.4", OutputTPSEWMA: 0, LatencyMsEWMA: 0, SampleReady: false},
	}
	scores := scorer.ScoreCandidates(mixed, cfg)
	cold, ok := findScore(scores, "cold")
	if !ok {
		t.Fatalf("cold candidate missing: %+v", scores)
	}
	if cold.SampleReady {
		t.Fatalf("cold SampleReady = true, want false")
	}
	if cold.Score == 0 {
		t.Fatalf("cold fallback score = 0, want median ready score")
	}

	allCold := []ScoreCandidate{
		{Provider: "codex", AuthID: "cold-a", Model: "gpt-5.4"},
		{Provider: "codex", AuthID: "cold-b", Model: "gpt-5.4"},
	}
	coldScores := scorer.ScoreCandidates(allCold, cfg)
	if coldScores[0].Score != 0 || coldScores[1].Score != 0 {
		t.Fatalf("all cold scores = %+v, want zero fallback scores", coldScores)
	}
}

func TestScoreCandidatesSanitizesInvalidMetrics(t *testing.T) {
	scorer := NewScorer(NewTracker(DefaultConfig()))
	candidates := []ScoreCandidate{
		{Provider: "codex", AuthID: "clean", Model: "gpt-5.4", OutputTPSEWMA: 10, LatencyMsEWMA: 1000, FailureRateEWMA: 0, Inflight: 0, SampleReady: true},
		{Provider: "codex", AuthID: "bad", Model: "gpt-5.4", OutputTPSEWMA: math.Inf(1), LatencyMsEWMA: math.NaN(), FailureRateEWMA: -1, Inflight: -3, SampleReady: true},
	}

	scores := scorer.ScoreCandidates(candidates, DefaultConfig())
	for _, score := range scores {
		if math.IsNaN(score.Score) || math.IsInf(score.Score, 0) {
			t.Fatalf("score contains non-finite value: %+v", score)
		}
		if score.Inflight < 0 || score.FailureRateEWMA < 0 || score.FailureRateEWMA > 1 || score.OutputTPSEWMA < 0 || score.LatencyMsEWMA < 0 {
			t.Fatalf("score contains unsanitized metrics: %+v", score)
		}
	}
	if scores[0].AuthID != "clean" {
		t.Fatalf("top auth = %s, want clean; scores=%+v", scores[0].AuthID, scores)
	}
}

func TestScoreCandidatesPreservesInputOrderOnTies(t *testing.T) {
	scorer := NewScorer(NewTracker(DefaultConfig()))
	candidates := []ScoreCandidate{
		{Provider: "codex", AuthID: "auth-b", Model: "gpt-5.4", OutputTPSEWMA: 20, LatencyMsEWMA: 1000, SampleReady: true},
		{Provider: "codex", AuthID: "auth-a", Model: "gpt-5.4", OutputTPSEWMA: 20, LatencyMsEWMA: 1000, SampleReady: true},
	}

	scores := scorer.ScoreCandidates(candidates, DefaultConfig())
	if scores[0].AuthID != "auth-b" || scores[1].AuthID != "auth-a" {
		t.Fatalf("tie order = %s,%s; want auth-b,auth-a", scores[0].AuthID, scores[1].AuthID)
	}
}

func TestScoreCandidatesPenalizesExtremeGPT54LatencyMoreAggressively(t *testing.T) {
	scorer := NewScorer(NewTracker(DefaultConfig()))
	candidates := []ScoreCandidate{
		{Provider: "codex", AuthID: "extreme-latency", Model: "gpt-5.4", OutputTPSEWMA: 45, LatencyMsEWMA: 260000, SampleReady: true},
		{Provider: "codex", AuthID: "healthy", Model: "gpt-5.4", OutputTPSEWMA: 20, LatencyMsEWMA: 20000, SampleReady: true},
	}

	scores := scorer.ScoreCandidates(candidates, DefaultConfig())
	if scores[0].AuthID != "healthy" {
		t.Fatalf("top auth = %s, want healthy; scores=%+v", scores[0].AuthID, scores)
	}
}

func findScore(scores []Score, authID string) (Score, bool) {
	for _, score := range scores {
		if score.AuthID == authID {
			return score, true
		}
	}
	return Score{}, false
}
