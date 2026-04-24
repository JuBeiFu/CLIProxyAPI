package performance

import (
	"math"
	"sort"
	"time"
)

type ScoreCandidate struct {
	Provider        string
	AuthID          string
	Model           string
	OutputTPSEWMA   float64
	LatencyMsEWMA   float64
	FailureRateEWMA float64
	Inflight        int
	SampleReady     bool
}

type Score struct {
	Provider        string
	AuthID          string
	Model           string
	Score           float64
	OutputTPSEWMA   float64
	LatencyMsEWMA   float64
	FailureRateEWMA float64
	Inflight        int
	SampleReady     bool
}

type Scorer struct {
	tracker *Tracker
}

func NewScorer(tracker *Tracker) *Scorer {
	return &Scorer{tracker: tracker}
}

func (s *Scorer) SnapshotCandidate(provider, authID, model string, inflight int) ScoreCandidate {
	provider = normalize(provider)
	model = normalizeModel(model)
	if s == nil || s.tracker == nil {
		return ScoreCandidate{Provider: provider, AuthID: authID, Model: model, Inflight: inflight}
	}
	snapshots := s.tracker.Snapshot(SnapshotFilter{Provider: provider, AuthID: authID, Model: model}, timeNow())
	if len(snapshots) == 0 {
		return ScoreCandidate{Provider: provider, AuthID: authID, Model: model, Inflight: inflight}
	}
	snap := snapshots[0]
	return ScoreCandidate{
		Provider:        snap.Provider,
		AuthID:          snap.AuthID,
		Model:           snap.Model,
		OutputTPSEWMA:   snap.OutputTPSEWMA,
		LatencyMsEWMA:   snap.LatencyMsEWMA,
		FailureRateEWMA: snap.FailureRateEWMA,
		Inflight:        inflight,
		SampleReady:     snap.SampleReady,
	}
}

func (s *Scorer) ScoreCandidates(candidates []ScoreCandidate, cfg Config) []Score {
	cfg = NormalizeConfig(cfg)
	out := make([]Score, 0, len(candidates))
	if len(candidates) == 0 {
		return out
	}
	minTPS, maxTPS := bounds(candidates, func(c ScoreCandidate) float64 { return c.OutputTPSEWMA }, true)
	minLatency, maxLatency := bounds(candidates, func(c ScoreCandidate) float64 { return c.LatencyMsEWMA }, true)
	readyScores := make([]float64, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = sanitizeScoreCandidate(candidate)
		score := Score{
			Provider:        normalize(candidate.Provider),
			AuthID:          candidate.AuthID,
			Model:           normalizeModel(candidate.Model),
			OutputTPSEWMA:   candidate.OutputTPSEWMA,
			LatencyMsEWMA:   candidate.LatencyMsEWMA,
			FailureRateEWMA: candidate.FailureRateEWMA,
			Inflight:        candidate.Inflight,
			SampleReady:     candidate.SampleReady,
		}
		if candidate.SampleReady {
			normalizedTPS := normalizeRange(candidate.OutputTPSEWMA, minTPS, maxTPS)
			normalizedLatency := normalizeRange(candidate.LatencyMsEWMA, minLatency, maxLatency)
			score.Score = normalizedTPS*cfg.WeightTPS - normalizedLatency*cfg.WeightLatency - candidate.FailureRateEWMA*cfg.WeightFailure - float64(candidate.Inflight)*cfg.WeightInflight
			readyScores = append(readyScores, score.Score)
		}
		out = append(out, score)
	}
	fallback := median(readyScores)
	for i := range out {
		if !out[i].SampleReady {
			out[i].Score = fallback
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Score > out[j].Score
	})
	return out
}

func sanitizeScoreCandidate(candidate ScoreCandidate) ScoreCandidate {
	candidate.OutputTPSEWMA = sanitizeNonNegative(candidate.OutputTPSEWMA)
	candidate.LatencyMsEWMA = sanitizeNonNegative(candidate.LatencyMsEWMA)
	candidate.FailureRateEWMA = sanitizeUnit(candidate.FailureRateEWMA)
	if candidate.Inflight < 0 {
		candidate.Inflight = 0
	}
	return candidate
}

func bounds(candidates []ScoreCandidate, value func(ScoreCandidate) float64, readyOnly bool) (float64, float64) {
	min := 0.0
	max := 0.0
	seen := false
	for _, candidate := range candidates {
		if readyOnly && !candidate.SampleReady {
			continue
		}
		v := sanitizeNonNegative(value(sanitizeScoreCandidate(candidate)))
		if !seen {
			min, max, seen = v, v, true
			continue
		}
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	return min, max
}

func sanitizeNonNegative(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0
	}
	return value
}

func sanitizeUnit(value float64) float64 {
	value = sanitizeNonNegative(value)
	if value > 1 {
		return 1
	}
	return value
}

func normalizeRange(value, min, max float64) float64 {
	if max <= min {
		if value > 0 {
			return 1
		}
		return 0
	}
	return (value - min) / (max - min)
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	cp := make([]float64, len(values))
	copy(cp, values)
	sort.Float64s(cp)
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid]
	}
	return (cp[mid-1] + cp[mid]) / 2
}

var timeNow = time.Now
