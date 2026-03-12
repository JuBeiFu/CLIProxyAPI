package synthesizer

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestConfigSynthesizerAddsRoutingMetadataToAuthAttributes(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			CodexKey: []config.CodexKey{{
				APIKey:       "codex-key",
				BaseURL:      "https://api.openai.com",
				PlanType:     "team",
				ProxyProfile: "paid-egress",
			}},
		},
		Now:         time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("Synthesize() error = %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("len(auths) = %d, want 1", len(auths))
	}

	if got := auths[0].Attributes["plan_type"]; got != "team" {
		t.Fatalf("auths[0].Attributes[plan_type] = %q, want team", got)
	}
	if got := auths[0].Attributes["proxy_profile"]; got != "paid-egress" {
		t.Fatalf("auths[0].Attributes[proxy_profile] = %q, want paid-egress", got)
	}
}
