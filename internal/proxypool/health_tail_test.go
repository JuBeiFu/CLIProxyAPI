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

func TestReportPassiveOutcomeKeepsVeryLongSuccessfulCodexStreamHealthy(t *testing.T) {
	t.Parallel()

	manager := NewHealthManager()
	now := time.Now()

	manager.ReportPassiveOutcome("pool-a", "proxy-2", PassiveOutcome{
		Model:      "gpt-5.5",
		Endpoint:   "responses_stream",
		Total:      8 * time.Minute,
		FirstByte:  900 * time.Millisecond,
		ReadBody:   7*time.Minute + 59*time.Second,
		CheckedAt:  now,
		Successful: true,
	})

	result, ok := manager.Result("pool-a", "proxy-2")
	if !ok {
		t.Fatal("Result returned ok=false")
	}
	if !result.Healthy {
		t.Fatalf("Healthy = false, want true; result=%+v", result)
	}
	if !result.UnhealthyUntil.IsZero() {
		t.Fatalf("UnhealthyUntil = %v, want zero", result.UnhealthyUntil)
	}
}
