package helps

import "testing"

func TestOpenAILoginHostUsesUtls(t *testing.T) {
	for _, h := range []string{"chatgpt.com", "auth.openai.com", "sentinel.openai.com"} {
		if !hostUsesUtls(h) {
			t.Fatalf("expected %s to route through utls", h)
		}
	}
	if hostUsesUtls("example.com") {
		t.Fatalf("example.com must not use utls")
	}
	if !hostUsesUtls("api.anthropic.com") {
		t.Fatalf("anthropic must still use utls")
	}
}
