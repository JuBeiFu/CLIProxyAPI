package performance

import (
	"sort"
	"strings"
	"sync"
	"time"
)

type Tracker struct {
	mu      sync.RWMutex
	cfg     Config
	entries map[Key]*entry
}

type entry struct {
	authIndex        string
	requestCount     int64
	successCount     int64
	failureCount     int64
	speedSamples     int64
	latencySamples   int64
	firstByteSamples int64
	failureSamples   int64
	outputTokens     int64
	outputTPSEWMA    float64
	latencyMsEWMA    float64
	firstByteMsEWMA  float64
	failureRateEWMA  float64
	lastSeen         time.Time
	events           []windowEvent
}

type windowEvent struct {
	at           time.Time
	outputTokens int64
	duration     time.Duration
}

func NewTracker(cfg Config) *Tracker {
	return &Tracker{
		cfg:     NormalizeConfig(cfg),
		entries: make(map[Key]*entry),
	}
}

func (t *Tracker) Configure(cfg Config) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.cfg = NormalizeConfig(cfg)
	t.mu.Unlock()
}

func (t *Tracker) Config() Config {
	if t == nil {
		return DefaultConfig()
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.cfg
}

func (t *Tracker) Record(sample Sample) {
	if t == nil {
		return
	}
	key, ok := sample.key()
	if !ok {
		return
	}
	if sample.RequestedAt.IsZero() {
		sample.RequestedAt = time.Now()
	}
	if sample.Latency < 0 {
		sample.Latency = 0
	}
	if sample.FirstByteLatency < 0 {
		sample.FirstByteLatency = 0
	}
	if sample.OutputTokens < 0 {
		sample.OutputTokens = 0
	}
	if sample.TotalTokens < 0 {
		sample.TotalTokens = 0
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	cfg := t.cfg
	e := t.entries[key]
	if e == nil {
		e = &entry{}
		t.entries[key] = e
	}
	if authIndex := strings.TrimSpace(sample.AuthIndex); authIndex != "" {
		e.authIndex = authIndex
	}
	e.requestCount++
	e.lastSeen = sample.RequestedAt
	if sample.Failed {
		e.failureCount++
		e.failureSamples++
		e.failureRateEWMA = applyEWMA(e.failureRateEWMA, 1, e.failureSamples, cfg.EWMAAlpha)
	} else {
		e.successCount++
		e.failureSamples++
		e.failureRateEWMA = applyEWMA(e.failureRateEWMA, 0, e.failureSamples, cfg.EWMAAlpha)
	}
	if sample.Latency > 0 {
		e.latencySamples++
		e.latencyMsEWMA = applyEWMA(e.latencyMsEWMA, durationMillis(sample.Latency), e.latencySamples, cfg.EWMAAlpha)
	}
	if sample.FirstByteLatency > 0 {
		e.firstByteSamples++
		e.firstByteMsEWMA = applyEWMA(e.firstByteMsEWMA, durationMillis(sample.FirstByteLatency), e.firstByteSamples, cfg.EWMAAlpha)
	}
	if !sample.Failed && sample.OutputTokens > 0 {
		duration := generationDuration(sample)
		if duration > 0 {
			tps := float64(sample.OutputTokens) / duration.Seconds()
			e.speedSamples++
			e.outputTokens += sample.OutputTokens
			e.outputTPSEWMA = applyEWMA(e.outputTPSEWMA, tps, e.speedSamples, cfg.EWMAAlpha)
			e.events = append(e.events, windowEvent{at: sample.RequestedAt, outputTokens: sample.OutputTokens, duration: duration})
		}
	}
	e.trimWindow(sample.RequestedAt, cfg.Window)
}

func (t *Tracker) Snapshot(filter SnapshotFilter, now time.Time) []Snapshot {
	if t == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	filter.Provider = normalize(filter.Provider)
	filter.Model = normalizeModel(filter.Model)
	filter.AuthID = strings.TrimSpace(filter.AuthID)

	t.mu.Lock()
	cfg := t.cfg
	out := make([]Snapshot, 0, len(t.entries))
	for key, e := range t.entries {
		if filter.Provider != "" && key.Provider != filter.Provider {
			continue
		}
		if filter.Model != "" && key.Model != filter.Model {
			continue
		}
		if filter.AuthID != "" && key.AuthID != filter.AuthID {
			continue
		}
		e.trimWindow(now, cfg.Window)
		snap := e.snapshot(key, cfg)
		if filter.ReadyOnly && !snap.SampleReady {
			continue
		}
		out = append(out, snap)
	}
	t.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].AuthID < out[j].AuthID
	})
	return out
}

func (s Sample) key() (Key, bool) {
	provider := normalize(s.Provider)
	authID := strings.TrimSpace(s.AuthID)
	model := normalizeModel(s.Model)
	if provider == "" || authID == "" || model == "" {
		return Key{}, false
	}
	return Key{Provider: provider, AuthID: authID, Model: model}, true
}

func (e *entry) snapshot(key Key, cfg Config) Snapshot {
	var windowTokens int64
	var windowDuration time.Duration
	for _, event := range e.events {
		windowTokens += event.outputTokens
		windowDuration += event.duration
	}
	windowTPS := 0.0
	if windowTokens > 0 && windowDuration > 0 {
		windowTPS = float64(windowTokens) / windowDuration.Seconds()
	}
	return Snapshot{
		Provider:           key.Provider,
		AuthID:             key.AuthID,
		AuthIndex:          e.authIndex,
		Model:              key.Model,
		RequestCount:       e.requestCount,
		SuccessCount:       e.successCount,
		FailureCount:       e.failureCount,
		OutputTokens:       e.outputTokens,
		OutputTPSEWMA:      e.outputTPSEWMA,
		LatencyMsEWMA:      e.latencyMsEWMA,
		FirstByteMsEWMA:    e.firstByteMsEWMA,
		FailureRateEWMA:    e.failureRateEWMA,
		WindowRequestCount: int64(len(e.events)),
		WindowOutputTokens: windowTokens,
		WindowOutputTPS:    windowTPS,
		SampleReady:        e.speedSamples >= cfg.MinSamples,
		LastSeen:           e.lastSeen,
	}
}

func (e *entry) trimWindow(now time.Time, window time.Duration) {
	if e == nil || window <= 0 || len(e.events) == 0 {
		return
	}
	cutoff := now.Add(-window)
	keep := e.events[:0]
	for _, event := range e.events {
		if !event.at.Before(cutoff) {
			keep = append(keep, event)
		}
	}
	clear(e.events[len(keep):])
	e.events = keep
}

func generationDuration(sample Sample) time.Duration {
	duration := sample.Latency
	if sample.FirstByteLatency > 0 && sample.Latency > sample.FirstByteLatency {
		duration = sample.Latency - sample.FirstByteLatency
	}
	if duration < 0 {
		return 0
	}
	return duration
}

func applyEWMA(previous, value float64, count int64, alpha float64) float64 {
	if count <= 1 {
		return value
	}
	return alpha*value + (1-alpha)*previous
}

func durationMillis(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func normalize(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeModel(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
