package performance

import (
	"context"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestPluginConvertsUsageRecordToSample(t *testing.T) {
	tracker := NewTracker(Config{Window: time.Minute, MinSamples: 1, EWMAAlpha: 1})
	plugin := NewPlugin(tracker)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	plugin.HandleUsage(context.Background(), coreusage.Record{
		Provider:         "codex",
		Model:            "gpt-5.4",
		AuthID:           "auth-a",
		AuthIndex:        "3",
		RequestedAt:      now,
		Latency:          5 * time.Second,
		FirstByteLatency: time.Second,
		Detail: coreusage.Detail{
			OutputTokens: 80,
			TotalTokens:  120,
			StatusCode:   200,
		},
	})

	got := tracker.Snapshot(SnapshotFilter{}, now)[0]
	if got.Provider != "codex" || got.AuthID != "auth-a" || got.AuthIndex != "3" || got.Model != "gpt-5.4" {
		t.Fatalf("unexpected snapshot identity: %+v", got)
	}
	if got.OutputTPSEWMA != 20 {
		t.Fatalf("OutputTPSEWMA = %v, want 20", got.OutputTPSEWMA)
	}
}

func TestPluginIgnoresNilTracker(t *testing.T) {
	plugin := NewPlugin(nil)
	plugin.HandleUsage(context.Background(), coreusage.Record{
		Provider: "codex",
		Model:    "gpt-5.4",
		AuthID:   "auth-a",
	})
}
