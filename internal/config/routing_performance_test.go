package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRoutingPerformanceDefaults(t *testing.T) {
	cfg := Config{}
	cfg.ApplyRoutingPerformanceDefaults()

	if cfg.Routing.PerformanceAware {
		t.Fatalf("PerformanceAware = true, want false")
	}
	if !cfg.Routing.PerformanceShadowLog {
		t.Fatalf("PerformanceShadowLog = false, want true")
	}
	if cfg.Routing.PerformanceWindowSeconds != 300 {
		t.Fatalf("PerformanceWindowSeconds = %d, want 300", cfg.Routing.PerformanceWindowSeconds)
	}
	if cfg.Routing.PerformanceMinSamples != 5 {
		t.Fatalf("PerformanceMinSamples = %d, want 5", cfg.Routing.PerformanceMinSamples)
	}
	if cfg.Routing.PerformanceEWMAAlpha != 0.25 {
		t.Fatalf("PerformanceEWMAAlpha = %v, want 0.25", cfg.Routing.PerformanceEWMAAlpha)
	}
}

func TestRoutingPerformanceExplicitShadowFalse(t *testing.T) {
	var cfg Config
	data := []byte(`
routing:
  performance-shadow-log: false
`)
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	cfg.ApplyRoutingPerformanceDefaults()

	if cfg.Routing.PerformanceShadowLog {
		t.Fatalf("PerformanceShadowLog = true, want false from YAML")
	}
}

func TestRoutingPerformanceYAMLFields(t *testing.T) {
	var cfg Config
	data := []byte(`
routing:
  strategy: fill-first
  performance-aware: true
  performance-shadow-log: false
  performance-window-seconds: 120
  performance-min-samples: 9
  performance-ewma-alpha: 0.4
  performance-weight-tps: 1.5
  performance-weight-latency: 0.3
  performance-weight-failure: 3.0
  performance-weight-inflight: 0.8
`)
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	cfg.ApplyRoutingPerformanceDefaults()

	if cfg.Routing.Strategy != "fill-first" || !cfg.Routing.PerformanceAware || cfg.Routing.PerformanceShadowLog {
		t.Fatalf("unexpected routing config: %+v", cfg.Routing)
	}
	if cfg.Routing.PerformanceWindowSeconds != 120 || cfg.Routing.PerformanceMinSamples != 9 || cfg.Routing.PerformanceEWMAAlpha != 0.4 {
		t.Fatalf("unexpected numeric config: %+v", cfg.Routing)
	}
	if cfg.Routing.PerformanceWeightTPS != 1.5 || cfg.Routing.PerformanceWeightLatency != 0.3 || cfg.Routing.PerformanceWeightFailure != 3.0 || cfg.Routing.PerformanceWeightInflight != 0.8 {
		t.Fatalf("unexpected weights: %+v", cfg.Routing)
	}
}

func TestRoutingPerformanceInvalidNumbersUseDefaults(t *testing.T) {
	cfg := Config{}
	cfg.Routing.PerformanceWindowSeconds = -1
	cfg.Routing.PerformanceMinSamples = 0
	cfg.Routing.PerformanceEWMAAlpha = 9
	cfg.Routing.PerformanceWeightTPS = -1
	cfg.Routing.PerformanceWeightLatency = -1
	cfg.Routing.PerformanceWeightFailure = -1
	cfg.Routing.PerformanceWeightInflight = -1
	cfg.ApplyRoutingPerformanceDefaults()

	if cfg.Routing.PerformanceWindowSeconds != 300 {
		t.Fatalf("PerformanceWindowSeconds = %d, want 300", cfg.Routing.PerformanceWindowSeconds)
	}
	if cfg.Routing.PerformanceMinSamples != 5 {
		t.Fatalf("PerformanceMinSamples = %d, want 5", cfg.Routing.PerformanceMinSamples)
	}
	if cfg.Routing.PerformanceEWMAAlpha != 1 {
		t.Fatalf("PerformanceEWMAAlpha = %v, want clamped 1", cfg.Routing.PerformanceEWMAAlpha)
	}
	if cfg.Routing.PerformanceWeightTPS != 1 || cfg.Routing.PerformanceWeightLatency != 0.25 || cfg.Routing.PerformanceWeightFailure != 2 || cfg.Routing.PerformanceWeightInflight != 0.5 {
		t.Fatalf("unexpected default weights: %+v", cfg.Routing)
	}
}

func TestConfigUnmarshalYAMLNilNode(t *testing.T) {
	var cfg Config
	if err := cfg.UnmarshalYAML(nil); err != nil {
		t.Fatalf("UnmarshalYAML(nil): %v", err)
	}
}

func TestSaveConfigPreserveCommentsDoesNotAddRoutingPerformanceDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := []byte("routing:\n  strategy: round-robin\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg := &Config{}
	cfg.Routing.Strategy = "round-robin"
	cfg.ApplyRoutingPerformanceDefaults()

	if err := SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatalf("SaveConfigPreserveComments: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "performance-aware") || strings.Contains(text, "performance-shadow-log") || strings.Contains(text, "performance-window-seconds") {
		t.Fatalf("routing performance defaults were added to config:\n%s", text)
	}
}

func TestSaveConfigPreserveCommentsDropsRuntimeGeneratedProxyEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := []byte("proxy-pools:\n  - name: free-egress\n    entries:\n      - name: existing\n        url: http://example.com\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg := &Config{
		SDKConfig: SDKConfig{
			ProxyPools: []ProxyPool{
				{
					Name: "free-egress",
					Entries: []ProxyPoolEntry{
						{Name: "existing", URL: "http://example.com"},
						{Name: "generated", URL: "bind://[2602:294:0:eb::100]", runtimeGenerated: true},
					},
					IPv6BindRanges: []IPv6BindRange{
						{NamePrefix: "free-v6-", Start: "2602:294:0:eb::100"},
					},
				},
			},
		},
	}
	if err := SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatalf("SaveConfigPreserveComments: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "generated") || strings.Contains(text, "bind://[2602:294:0:eb::100]") {
		t.Fatalf("runtime generated proxy entries were persisted:\n%s", text)
	}
	if !strings.Contains(text, "ipv6-bind-ranges") {
		t.Fatalf("expected ipv6-bind-ranges to remain in persisted config:\n%s", text)
	}
}
