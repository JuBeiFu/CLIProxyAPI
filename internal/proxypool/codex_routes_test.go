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
