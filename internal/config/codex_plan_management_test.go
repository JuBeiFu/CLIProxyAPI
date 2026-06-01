package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestCodexPlanManagementDefaults(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("codex-plan-management:\n  enabled: true\n"), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cfg.ApplyCodexPlanManagementDefaults()

	if !cfg.CodexPlanManagement.Enabled {
		t.Fatal("expected enabled true")
	}
	if cfg.CodexPlanManagement.PlanUpgradeReprobeInterval != time.Hour {
		t.Fatalf("reprobe interval = %s, want 1h", cfg.CodexPlanManagement.PlanUpgradeReprobeInterval)
	}
	if len(cfg.CodexPlanManagement.FreeModelAllowlist) != 1 || cfg.CodexPlanManagement.FreeModelAllowlist[0] != "gpt-5.5" {
		t.Fatalf("allowlist = %v, want [gpt-5.5]", cfg.CodexPlanManagement.FreeModelAllowlist)
	}
	if !cfg.CodexPlanManagement.FreeModelGateEnabled {
		t.Fatal("expected free-model-gate-enabled default true")
	}
}

func TestCodexPlanManagementExplicitValues(t *testing.T) {
	raw := "codex-plan-management:\n  enabled: true\n  free-model-gate-enabled: false\n  free-model-allowlist: [\"gpt-5.5\", \"gpt-5.5-codex\"]\n  plan-upgrade-reprobe-interval: 30m\n"
	var cfg Config
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cfg.ApplyCodexPlanManagementDefaults()

	if cfg.CodexPlanManagement.FreeModelGateEnabled {
		t.Fatal("expected explicit free-model-gate-enabled false to be preserved")
	}
	if cfg.CodexPlanManagement.PlanUpgradeReprobeInterval != 30*time.Minute {
		t.Fatalf("reprobe interval = %s, want 30m", cfg.CodexPlanManagement.PlanUpgradeReprobeInterval)
	}
	if len(cfg.CodexPlanManagement.FreeModelAllowlist) != 2 {
		t.Fatalf("allowlist len = %d, want 2", len(cfg.CodexPlanManagement.FreeModelAllowlist))
	}
}

func TestCodexPlanManagementExplicitZeroIntervalPreserved(t *testing.T) {
	raw := "codex-plan-management:\n  enabled: true\n  plan-upgrade-reprobe-interval: 0s\n"
	var cfg Config
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cfg.ApplyCodexPlanManagementDefaults()

	if cfg.CodexPlanManagement.PlanUpgradeReprobeInterval != 0 {
		t.Fatalf("explicit interval 0 must be preserved (auto branch disabled), got %s", cfg.CodexPlanManagement.PlanUpgradeReprobeInterval)
	}
}
