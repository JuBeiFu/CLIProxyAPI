package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestCodexRouteManagementDefaults(t *testing.T) {
	cfg := Config{}
	cfg.ApplyRoutingPerformanceDefaults()

	if cfg.CodexRouteManagement.Enabled {
		t.Fatalf("Enabled = true, want false")
	}
	if cfg.CodexRouteManagement.FirstPayloadHedgeAfter != 4*time.Second {
		t.Fatalf("FirstPayloadHedgeAfter = %v, want 4s", cfg.CodexRouteManagement.FirstPayloadHedgeAfter)
	}
	if cfg.CodexRouteManagement.KeepaliveProbeInterval != 5*time.Minute {
		t.Fatalf("KeepaliveProbeInterval = %v, want 5m", cfg.CodexRouteManagement.KeepaliveProbeInterval)
	}
	if cfg.CodexRouteManagement.DegradedProbeInterval != 45*time.Second {
		t.Fatalf("DegradedProbeInterval = %v, want 45s", cfg.CodexRouteManagement.DegradedProbeInterval)
	}
	if cfg.CodexRouteManagement.RecoveringProbeInterval != 30*time.Second {
		t.Fatalf("RecoveringProbeInterval = %v, want 30s", cfg.CodexRouteManagement.RecoveringProbeInterval)
	}
	if cfg.CodexRouteManagement.SlowFirstByteThreshold != 4*time.Second {
		t.Fatalf("SlowFirstByteThreshold = %v, want 4s", cfg.CodexRouteManagement.SlowFirstByteThreshold)
	}
	if cfg.CodexRouteManagement.HeavyFirstByteThreshold != 10*time.Second {
		t.Fatalf("HeavyFirstByteThreshold = %v, want 10s", cfg.CodexRouteManagement.HeavyFirstByteThreshold)
	}
	if cfg.CodexRouteManagement.CoolingFirstByteThreshold != 30*time.Second {
		t.Fatalf("CoolingFirstByteThreshold = %v, want 30s", cfg.CodexRouteManagement.CoolingFirstByteThreshold)
	}
	if cfg.CodexRouteManagement.QuarantineFirstByteThreshold != 60*time.Second {
		t.Fatalf("QuarantineFirstByteThreshold = %v, want 60s", cfg.CodexRouteManagement.QuarantineFirstByteThreshold)
	}
	if cfg.CodexRouteManagement.PromotionWinThreshold != 2 {
		t.Fatalf("PromotionWinThreshold = %d, want 2", cfg.CodexRouteManagement.PromotionWinThreshold)
	}
}

func TestCodexRouteManagementYAMLFields(t *testing.T) {
	var cfg Config
	data := []byte(`
codex-route-management:
  enabled: true
  first-payload-hedge-after: 6s
  keepalive-probe-interval: 7m
  degraded-probe-interval: 50s
  recovering-probe-interval: 25s
  slow-first-byte-threshold: 5s
  heavy-first-byte-threshold: 11s
  cooling-first-byte-threshold: 35s
  quarantine-first-byte-threshold: 70s
  promotion-win-threshold: 3
`)
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	cfg.ApplyRoutingPerformanceDefaults()

	if !cfg.CodexRouteManagement.Enabled {
		t.Fatal("Enabled = false, want true")
	}
	if cfg.CodexRouteManagement.FirstPayloadHedgeAfter != 6*time.Second {
		t.Fatalf("FirstPayloadHedgeAfter = %v, want 6s", cfg.CodexRouteManagement.FirstPayloadHedgeAfter)
	}
	if cfg.CodexRouteManagement.KeepaliveProbeInterval != 7*time.Minute {
		t.Fatalf("KeepaliveProbeInterval = %v, want 7m", cfg.CodexRouteManagement.KeepaliveProbeInterval)
	}
	if cfg.CodexRouteManagement.PromotionWinThreshold != 3 {
		t.Fatalf("PromotionWinThreshold = %d, want 3", cfg.CodexRouteManagement.PromotionWinThreshold)
	}
}

func TestCodexRouteManagementInvalidValuesUseDefaults(t *testing.T) {
	cfg := Config{}
	cfg.CodexRouteManagement.FirstPayloadHedgeAfter = -1
	cfg.CodexRouteManagement.KeepaliveProbeInterval = 0
	cfg.CodexRouteManagement.DegradedProbeInterval = -1
	cfg.CodexRouteManagement.RecoveringProbeInterval = -1
	cfg.CodexRouteManagement.SlowFirstByteThreshold = 0
	cfg.CodexRouteManagement.HeavyFirstByteThreshold = -1
	cfg.CodexRouteManagement.CoolingFirstByteThreshold = 0
	cfg.CodexRouteManagement.QuarantineFirstByteThreshold = -1
	cfg.CodexRouteManagement.PromotionWinThreshold = 0
	cfg.ApplyRoutingPerformanceDefaults()

	if cfg.CodexRouteManagement.FirstPayloadHedgeAfter != 4*time.Second {
		t.Fatalf("FirstPayloadHedgeAfter = %v, want 4s", cfg.CodexRouteManagement.FirstPayloadHedgeAfter)
	}
	if cfg.CodexRouteManagement.KeepaliveProbeInterval != 5*time.Minute {
		t.Fatalf("KeepaliveProbeInterval = %v, want 5m", cfg.CodexRouteManagement.KeepaliveProbeInterval)
	}
	if cfg.CodexRouteManagement.QuarantineFirstByteThreshold != 60*time.Second {
		t.Fatalf("QuarantineFirstByteThreshold = %v, want 60s", cfg.CodexRouteManagement.QuarantineFirstByteThreshold)
	}
	if cfg.CodexRouteManagement.PromotionWinThreshold != 2 {
		t.Fatalf("PromotionWinThreshold = %d, want 2", cfg.CodexRouteManagement.PromotionWinThreshold)
	}
}
