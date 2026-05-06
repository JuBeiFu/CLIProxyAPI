package proxypool

import (
	"testing"
	"time"
)

func TestReportPassiveOutcomeMarksHeavyFirstByteAsUnhealthy(t *testing.T) {
	t.Parallel()

	manager := NewHealthManager()
	now := time.Now()

	manager.ReportPassiveOutcome("pool-a", "proxy-1", PassiveOutcome{
		Model:     "gpt-5.5",
		Endpoint:  "responses_stream",
		Total:     80 * time.Second,
		FirstByte: 45 * time.Second,
		CheckedAt: now,
	})

	result, ok := manager.Result("pool-a", "proxy-1")
	if !ok {
		t.Fatal("Result returned ok=false")
	}
	if result.Healthy {
		t.Fatal("Healthy = true, want false")
	}
	if result.UnhealthyUntil.IsZero() {
		t.Fatal("UnhealthyUntil is zero, want cooldown")
	}
}
