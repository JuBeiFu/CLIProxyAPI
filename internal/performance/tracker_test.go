package performance

import (
	"testing"
	"time"
)

func TestTrackerRecordsSuccessfulSampleAndEWMA(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	tracker := NewTracker(Config{
		Window:     5 * time.Minute,
		MinSamples: 2,
		EWMAAlpha:  0.5,
	})

	tracker.Record(Sample{
		Provider:     "codex",
		AuthID:       "auth-a",
		AuthIndex:    "7",
		Model:        "gpt-5.4",
		RequestedAt:  now,
		Latency:      2 * time.Second,
		OutputTokens: 40,
		TotalTokens:  60,
		StatusCode:   200,
	})
	tracker.Record(Sample{
		Provider:     "codex",
		AuthID:       "auth-a",
		AuthIndex:    "7",
		Model:        "gpt-5.4",
		RequestedAt:  now.Add(time.Second),
		Latency:      4 * time.Second,
		OutputTokens: 40,
		TotalTokens:  80,
		StatusCode:   200,
	})

	snapshots := tracker.Snapshot(SnapshotFilter{}, now.Add(2*time.Second))
	if len(snapshots) != 1 {
		t.Fatalf("snapshot count = %d, want 1", len(snapshots))
	}
	got := snapshots[0]
	if got.Provider != "codex" || got.AuthID != "auth-a" || got.AuthIndex != "7" || got.Model != "gpt-5.4" {
		t.Fatalf("unexpected identity: %+v", got)
	}
	if got.RequestCount != 2 || got.SuccessCount != 2 || got.FailureCount != 0 {
		t.Fatalf("unexpected counters: %+v", got)
	}
	if got.OutputTokens != 80 || got.WindowOutputTokens != 80 || got.WindowRequestCount != 2 {
		t.Fatalf("unexpected token/window counters: %+v", got)
	}
	if got.OutputTPSEWMA != 15 {
		t.Fatalf("OutputTPSEWMA = %v, want 15", got.OutputTPSEWMA)
	}
	if got.LatencyMsEWMA != 3000 {
		t.Fatalf("LatencyMsEWMA = %v, want 3000", got.LatencyMsEWMA)
	}
	if !got.SampleReady {
		t.Fatalf("SampleReady = false, want true")
	}
}

func TestTrackerRecordsFailureWithoutSpeedInflation(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	tracker := NewTracker(Config{Window: time.Minute, MinSamples: 1, EWMAAlpha: 0.25})

	tracker.Record(Sample{
		Provider:     "codex",
		AuthID:       "auth-a",
		Model:        "gpt-5.4",
		RequestedAt:  now,
		Latency:      time.Second,
		OutputTokens: 100,
		Failed:       true,
		StatusCode:   502,
	})

	got := tracker.Snapshot(SnapshotFilter{}, now)[0]
	if got.RequestCount != 1 || got.SuccessCount != 0 || got.FailureCount != 1 {
		t.Fatalf("unexpected counters: %+v", got)
	}
	if got.OutputTPSEWMA != 0 {
		t.Fatalf("OutputTPSEWMA = %v, want 0", got.OutputTPSEWMA)
	}
	if got.FailureRateEWMA != 1 {
		t.Fatalf("FailureRateEWMA = %v, want 1", got.FailureRateEWMA)
	}
	if got.SampleReady {
		t.Fatalf("SampleReady = true, want false for zero successful speed samples")
	}
}

func TestTrackerFailureRateEWMAAfterCleanSample(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	tracker := NewTracker(Config{Window: time.Minute, MinSamples: 1, EWMAAlpha: 0.25})
	tracker.Record(Sample{Provider: "codex", AuthID: "auth-a", Model: "gpt-5.4", RequestedAt: now, Latency: time.Second, OutputTokens: 10})
	tracker.Record(Sample{Provider: "codex", AuthID: "auth-a", Model: "gpt-5.4", RequestedAt: now.Add(time.Second), Latency: time.Second, Failed: true, StatusCode: 502})

	got := tracker.Snapshot(SnapshotFilter{}, now.Add(time.Second))[0]
	if got.FailureRateEWMA != 0.25 {
		t.Fatalf("FailureRateEWMA = %v, want 0.25", got.FailureRateEWMA)
	}
}

func TestTrackerUsesGenerationDurationWhenFirstByteLatencyExists(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	tracker := NewTracker(Config{Window: time.Minute, MinSamples: 1, EWMAAlpha: 1})

	tracker.Record(Sample{
		Provider:         "codex",
		AuthID:           "auth-a",
		Model:            "gpt-5.4",
		RequestedAt:      now,
		Latency:          5 * time.Second,
		FirstByteLatency: time.Second,
		OutputTokens:     80,
	})

	got := tracker.Snapshot(SnapshotFilter{}, now)[0]
	if got.OutputTPSEWMA != 20 {
		t.Fatalf("OutputTPSEWMA = %v, want 20", got.OutputTPSEWMA)
	}
	if got.FirstByteMsEWMA != 1000 {
		t.Fatalf("FirstByteMsEWMA = %v, want 1000", got.FirstByteMsEWMA)
	}
}

func TestTrackerFiltersAndReadyOnly(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	tracker := NewTracker(Config{Window: time.Minute, MinSamples: 2, EWMAAlpha: 1})
	tracker.Record(Sample{Provider: "codex", AuthID: "ready", Model: "gpt-5.4", RequestedAt: now, Latency: time.Second, OutputTokens: 10})
	tracker.Record(Sample{Provider: "codex", AuthID: "ready", Model: "gpt-5.4", RequestedAt: now, Latency: time.Second, OutputTokens: 10})
	tracker.Record(Sample{Provider: "codex", AuthID: "cold", Model: "gpt-5.4", RequestedAt: now, Latency: time.Second, OutputTokens: 10})
	tracker.Record(Sample{Provider: "gemini", AuthID: "other", Model: "gemini-2.5", RequestedAt: now, Latency: time.Second, OutputTokens: 10})

	filtered := tracker.Snapshot(SnapshotFilter{Provider: "codex", Model: "gpt-5.4", ReadyOnly: true}, now)
	if len(filtered) != 1 || filtered[0].AuthID != "ready" {
		t.Fatalf("filtered snapshots = %+v, want ready codex auth only", filtered)
	}
	one := tracker.Snapshot(SnapshotFilter{Provider: "codex", Model: "gpt-5.4", AuthID: "cold"}, now)
	if len(one) != 1 || one[0].AuthID != "cold" {
		t.Fatalf("auth_id filter = %+v, want cold auth", one)
	}
}

func TestTrackerWindowExpiresOldEvents(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	tracker := NewTracker(Config{Window: 10 * time.Second, MinSamples: 1, EWMAAlpha: 1})
	tracker.Record(Sample{Provider: "codex", AuthID: "auth-a", Model: "gpt-5.4", RequestedAt: now.Add(-20 * time.Second), Latency: time.Second, OutputTokens: 10})
	tracker.Record(Sample{Provider: "codex", AuthID: "auth-a", Model: "gpt-5.4", RequestedAt: now, Latency: time.Second, OutputTokens: 30})

	got := tracker.Snapshot(SnapshotFilter{}, now)[0]
	if got.WindowRequestCount != 1 || got.WindowOutputTokens != 30 || got.WindowOutputTPS != 30 {
		t.Fatalf("unexpected window values: %+v", got)
	}
}

func TestTrackerWindowExpiresOutOfOrderEvents(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	tracker := NewTracker(Config{Window: 10 * time.Second, MinSamples: 1, EWMAAlpha: 1})
	tracker.Record(Sample{Provider: "codex", AuthID: "auth-a", Model: "gpt-5.4", RequestedAt: now, Latency: time.Second, OutputTokens: 30})
	tracker.Record(Sample{Provider: "codex", AuthID: "auth-a", Model: "gpt-5.4", RequestedAt: now.Add(-20 * time.Second), Latency: time.Second, OutputTokens: 10})

	got := tracker.Snapshot(SnapshotFilter{}, now)[0]
	if got.WindowRequestCount != 1 || got.WindowOutputTokens != 30 || got.WindowOutputTPS != 30 {
		t.Fatalf("unexpected window values: %+v", got)
	}
}

func TestTrackerIgnoresSamplesMissingIdentity(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	tracker := NewTracker(Config{Window: time.Minute, MinSamples: 1, EWMAAlpha: 1})
	tracker.Record(Sample{Provider: "", AuthID: "auth-a", Model: "gpt-5.4", RequestedAt: now, Latency: time.Second, OutputTokens: 10})
	tracker.Record(Sample{Provider: "codex", AuthID: "", Model: "gpt-5.4", RequestedAt: now, Latency: time.Second, OutputTokens: 10})
	tracker.Record(Sample{Provider: "codex", AuthID: "auth-a", Model: "", RequestedAt: now, Latency: time.Second, OutputTokens: 10})

	if got := tracker.Snapshot(SnapshotFilter{}, now); len(got) != 0 {
		t.Fatalf("snapshots = %+v, want empty", got)
	}
}
