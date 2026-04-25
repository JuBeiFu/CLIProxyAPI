package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestMaybeAttachImageGenerationToolOmitsDefaultSize(t *testing.T) {
	t.Setenv(codexAutoImageToolEnv, "")

	out := maybeAttachImageGenerationTool("gpt-5.4", []byte(`{"model":"gpt-5.4","input":"draw a wide poster"}`))

	tool := gjson.GetBytes(out, "tools.0")
	if got := tool.Get("type").String(); got != "image_generation" {
		t.Fatalf("tools.0.type = %q, want image_generation: %s", got, string(out))
	}
	if tool.Get("size").Exists() {
		t.Fatalf("auto image_generation tool should omit size: %s", string(out))
	}
	if got := tool.Get("quality").String(); got != "high" {
		t.Fatalf("tools.0.quality = %q, want high: %s", got, string(out))
	}
	if got := tool.Get("background").String(); got != "auto" {
		t.Fatalf("tools.0.background = %q, want auto: %s", got, string(out))
	}
}
