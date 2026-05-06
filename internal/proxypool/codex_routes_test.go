package proxypool

import (
	"testing"
	"time"
)

func TestRouteRegistryPromotesStandbyAfterRepeatedWins(t *testing.T) {
	t.Parallel()

	reg := NewCodexRouteRegistry(CodexRouteConfig{
		PromotionWinThreshold:        2,
		QuarantineFirstByteThreshold: 60 * time.Second,
	})
	now := time.Now()

	routeA := RouteDescriptor{Pool: "pool-a", Entry: "proxy-1"}
	routeB := RouteDescriptor{Pool: "pool-a", Entry: "proxy-2"}

	reg.UpsertCertifiedRoute("auth-1", routeA, now)
	reg.UpsertCertifiedRoute("auth-1", routeB, now)

	reg.MarkHedgeWinner("auth-1", routeB, now.Add(time.Second))
	reg.MarkHedgeWinner("auth-1", routeB, now.Add(2*time.Second))

	primary, standby, ok := reg.PrimaryAndStandby("auth-1")
	if !ok {
		t.Fatal("PrimaryAndStandby returned ok=false")
	}
	if primary.Entry != "proxy-2" || standby.Entry != "proxy-1" {
		t.Fatalf("primary=%q standby=%q, want proxy-2/proxy-1", primary.Entry, standby.Entry)
	}
}

func TestRouteRegistryQuarantinesSevereTail(t *testing.T) {
	t.Parallel()

	reg := NewCodexRouteRegistry(CodexRouteConfig{
		PromotionWinThreshold:        2,
		QuarantineFirstByteThreshold: 60 * time.Second,
	})
	now := time.Now()
	route := RouteDescriptor{Pool: "pool-a", Entry: "proxy-9"}

	reg.UpsertCertifiedRoute("auth-1", route, now)
	reg.RecordPassiveOutcome("auth-1", route, RoutePassiveOutcome{
		CheckedAt:  now.Add(time.Second),
		FirstByte:  75 * time.Second,
		Successful: true,
	})

	state := reg.RouteState("auth-1", route)
	if state != RouteStateQuarantined {
		t.Fatalf("state = %v, want %v", state, RouteStateQuarantined)
	}
}

func TestRouteRegistryCoolsHeavyTailAndPromotesStandby(t *testing.T) {
	t.Parallel()

	reg := NewCodexRouteRegistry(CodexRouteConfig{
		CoolingFirstByteThreshold:    30 * time.Second,
		PromotionWinThreshold:        2,
		QuarantineFirstByteThreshold: 60 * time.Second,
	})
	now := time.Now()
	primary := RouteDescriptor{Pool: "pool-a", Entry: "proxy-1"}
	standby := RouteDescriptor{Pool: "pool-a", Entry: "proxy-2"}

	reg.UpsertCertifiedRoute("auth-1", primary, now)
	reg.UpsertCertifiedRoute("auth-1", standby, now)
	reg.RecordPassiveOutcome("auth-1", primary, RoutePassiveOutcome{
		CheckedAt:  now.Add(time.Second),
		FirstByte:  45 * time.Second,
		Successful: true,
	})

	if got := reg.RouteState("auth-1", primary); got != RouteStateCooling {
		t.Fatalf("primary state = %v, want %v", got, RouteStateCooling)
	}
	if got := reg.RouteState("auth-1", standby); got != RouteStatePrimary {
		t.Fatalf("standby state = %v, want %v", got, RouteStatePrimary)
	}
	if _, _, ok := reg.PrimaryAndStandby("auth-1"); ok {
		t.Fatal("PrimaryAndStandby returned ok=true, want false until a new standby is certified")
	}
}

func TestRouteRegistryPromotesStandbyWhenProbeQuarantinesPrimary(t *testing.T) {
	t.Parallel()

	reg := NewCodexRouteRegistry(CodexRouteConfig{
		CoolingFirstByteThreshold:    30 * time.Second,
		PromotionWinThreshold:        2,
		QuarantineFirstByteThreshold: 60 * time.Second,
	})
	now := time.Now()
	primary := RouteDescriptor{Pool: "pool-a", Entry: "proxy-1"}
	standby := RouteDescriptor{Pool: "pool-a", Entry: "proxy-2"}

	reg.UpsertCertifiedRoute("auth-1", primary, now)
	reg.UpsertCertifiedRoute("auth-1", standby, now)
	reg.ApplyProbeOutcome("auth-1", primary, ProbeOutcome{
		CheckedAt:       now.Add(time.Second),
		RouteConsistent: false,
		TerminalReason:  "probe_mismatch",
	})

	if got := reg.RouteState("auth-1", primary); got != RouteStateQuarantined {
		t.Fatalf("primary state = %v, want %v", got, RouteStateQuarantined)
	}
	if got := reg.RouteState("auth-1", standby); got != RouteStatePrimary {
		t.Fatalf("standby state = %v, want %v", got, RouteStatePrimary)
	}
}

func TestRouteRegistryRepairPrefersExistingStandbyThenStableCandidate(t *testing.T) {
	t.Parallel()

	reg := NewCodexRouteRegistry(CodexRouteConfig{
		CoolingFirstByteThreshold:    30 * time.Second,
		PromotionWinThreshold:        2,
		QuarantineFirstByteThreshold: 60 * time.Second,
	})
	now := time.Now()
	primary := RouteDescriptor{Pool: "pool-a", Entry: "proxy-2"}
	standby := RouteDescriptor{Pool: "pool-a", Entry: "proxy-3"}
	extra := RouteDescriptor{Pool: "pool-a", Entry: "proxy-1"}

	reg.UpsertCertifiedRoute("auth-1", primary, now)
	reg.UpsertCertifiedRoute("auth-1", standby, now.Add(time.Second))
	reg.UpsertCertifiedRoute("auth-1", extra, now.Add(2*time.Second))

	reg.RecordPassiveOutcome("auth-1", primary, RoutePassiveOutcome{
		CheckedAt:  now.Add(3 * time.Second),
		FirstByte:  45 * time.Second,
		Successful: true,
	})

	gotPrimary, gotStandby, ok := reg.PrimaryAndStandby("auth-1")
	if !ok {
		t.Fatal("PrimaryAndStandby returned ok=false")
	}
	if gotPrimary.Entry != "proxy-3" {
		t.Fatalf("primary = %q, want %q", gotPrimary.Entry, "proxy-3")
	}
	if gotStandby.Entry != "proxy-1" {
		t.Fatalf("standby = %q, want %q", gotStandby.Entry, "proxy-1")
	}
}

func TestRouteRegistryIgnoresNon2xxPassiveFailures(t *testing.T) {
	t.Parallel()

	reg := NewCodexRouteRegistry(CodexRouteConfig{
		CoolingFirstByteThreshold:    30 * time.Second,
		PromotionWinThreshold:        2,
		QuarantineFirstByteThreshold: 60 * time.Second,
	})
	now := time.Now()
	primary := RouteDescriptor{Pool: "pool-a", Entry: "proxy-1"}
	standby := RouteDescriptor{Pool: "pool-a", Entry: "proxy-2"}

	reg.UpsertCertifiedRoute("auth-1", primary, now)
	reg.UpsertCertifiedRoute("auth-1", standby, now.Add(time.Second))
	reg.RecordPassiveOutcome("auth-1", primary, RoutePassiveOutcome{
		CheckedAt:  now.Add(2 * time.Second),
		StatusCode: 429,
		Successful: false,
		Error:      `{"error":{"type":"usage_limit_reached"}}`,
	})

	gotPrimary, gotStandby, ok := reg.PrimaryAndStandby("auth-1")
	if !ok {
		t.Fatal("PrimaryAndStandby returned ok=false")
	}
	if gotPrimary.Entry != "proxy-1" {
		t.Fatalf("primary = %q, want %q", gotPrimary.Entry, "proxy-1")
	}
	if gotStandby.Entry != "proxy-2" {
		t.Fatalf("standby = %q, want %q", gotStandby.Entry, "proxy-2")
	}
}

func TestRouteRegistryIgnoresContextLengthExceededForAssistedPenalty(t *testing.T) {
	t.Parallel()

	reg := NewCodexRouteRegistry(CodexRouteConfig{})
	route := RouteDescriptor{Pool: "pool-a", Entry: "proxy-1"}
	now := time.Now()
	reg.UpsertCertifiedRoute("auth-1", route, now)

	reg.RecordPassiveOutcome("auth-1", route, RoutePassiveOutcome{
		CheckedAt:  now.Add(time.Second),
		FirstByte:  500 * time.Millisecond,
		Total:      95 * time.Second,
		ReadBody:   90 * time.Second,
		StatusCode: 200,
		Successful: false,
		Error:      `{"error":{"code":"context_length_exceeded"}}`,
	})

	if got := reg.RouteState("auth-1", route); got != RouteStatePrimary {
		t.Fatalf("RouteState = %v, want %v", got, RouteStatePrimary)
	}
}

func TestRouteRegistryQuarantinesLongIncompleteTail(t *testing.T) {
	t.Parallel()

	reg := NewCodexRouteRegistry(CodexRouteConfig{
		CoolingFirstByteThreshold:    30 * time.Second,
		PromotionWinThreshold:        2,
		QuarantineFirstByteThreshold: 60 * time.Second,
	})
	now := time.Now()
	primary := RouteDescriptor{Pool: "pool-a", Entry: "proxy-1"}
	standby := RouteDescriptor{Pool: "pool-a", Entry: "proxy-2"}

	reg.UpsertCertifiedRoute("auth-1", primary, now)
	reg.UpsertCertifiedRoute("auth-1", standby, now.Add(time.Second))
	reg.RecordPassiveOutcome("auth-1", primary, RoutePassiveOutcome{
		CheckedAt:  now.Add(2 * time.Second),
		FirstByte:  900 * time.Millisecond,
		ReadBody:   5*time.Minute + 10*time.Second,
		StatusCode: 200,
		Successful: false,
		Error:      "stream error: stream ID 1173; INTERNAL_ERROR; received from peer",
	})

	if got := reg.RouteState("auth-1", primary); got != RouteStateQuarantined {
		t.Fatalf("primary state = %v, want %v", got, RouteStateQuarantined)
	}
	if got := reg.RouteState("auth-1", standby); got != RouteStatePrimary {
		t.Fatalf("standby state = %v, want %v", got, RouteStatePrimary)
	}
}
