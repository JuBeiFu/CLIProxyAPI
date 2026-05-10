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

func TestReportPassiveOutcomeImmediatelyIsolatesNetworkUnreachableCodexRoute(t *testing.T) {
	t.Parallel()

	manager := NewHealthManager()
	now := time.Now()

	manager.ReportPassiveOutcome("pool-a", "proxy-v6", PassiveOutcome{
		Model:      "gpt-5.5",
		Endpoint:   "responses_stream",
		StatusCode: 503,
		Error:      `Post "https://chatgpt.com/backend-api/codex/responses": dial tcp [2602:294:0:eb::2da]:0->[2a06:98c1:3100::6812:202f]:443: connect: network is unreachable`,
		CheckedAt:  now,
	})

	result, ok := manager.Result("pool-a", "proxy-v6")
	if !ok {
		t.Fatal("Result returned ok=false")
	}
	if result.Healthy {
		t.Fatalf("Healthy = true, want false; result=%+v", result)
	}
	if result.UnhealthyUntil.IsZero() {
		t.Fatal("UnhealthyUntil is zero, want cooldown")
	}
}

func TestReportPassiveOutcomeCountsMediumIncompleteCodexStreamAsSlow(t *testing.T) {
	t.Parallel()

	manager := NewHealthManager()
	now := time.Now()
	outcome := PassiveOutcome{
		Model:      "gpt-5.5",
		Endpoint:   "responses_stream",
		FirstByte:  800 * time.Millisecond,
		ReadBody:   20 * time.Second,
		Total:      21 * time.Second,
		StatusCode: 200,
		Error:      "stream error: stream disconnected before completion: stream closed before response.completed",
	}

	manager.ReportPassiveOutcome("pool-a", "proxy-tail", outcome)
	if !manager.IsUsableAt("pool-a", "proxy-tail", now) {
		t.Fatal("expected first incomplete sample to keep entry usable")
	}

	second := outcome
	second.CheckedAt = now.Add(time.Second)
	manager.ReportPassiveOutcome("pool-a", "proxy-tail", second)

	if manager.IsUsableAt("pool-a", "proxy-tail", now.Add(time.Second)) {
		t.Fatal("expected repeated incomplete samples to mark entry unusable")
	}
}
