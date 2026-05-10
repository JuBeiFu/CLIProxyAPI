package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestMaybeAttachImageGenerationToolAttachesMinimalToolForGPT54Mini(t *testing.T) {
	out := maybeAttachImageGenerationTool("gpt-5.4-mini", []byte(`{"model":"gpt-5.4-mini","input":"draw a wide poster"}`))

	tool := gjson.GetBytes(out, "tools.0")
	if got := tool.Get("type").String(); got != "image_generation" {
		t.Fatalf("tools.0.type = %q, want image_generation: %s", got, string(out))
	}
	for _, field := range []string{"size", "quality", "background", "model", "action"} {
		if tool.Get(field).Exists() {
			t.Fatalf("expected minimal image_generation tool without %s: %s", field, string(out))
		}
	}
}

func TestMaybeAttachImageGenerationToolAttachesMinimalToolForGPT54(t *testing.T) {
	out := maybeAttachImageGenerationTool("gpt-5.4", []byte(`{"model":"gpt-5.4","input":"draw a wide poster"}`))

	tool := gjson.GetBytes(out, "tools.0")
	if got := tool.Get("type").String(); got != "image_generation" {
		t.Fatalf("tools.0.type = %q, want image_generation: %s", got, string(out))
	}
}

func TestMaybeAttachImageGenerationToolAttachesMinimalToolForGPT55(t *testing.T) {
	out := maybeAttachImageGenerationTool("gpt-5.5", []byte(`{"model":"gpt-5.5","input":"draw a wide poster"}`))

	tool := gjson.GetBytes(out, "tools.0")
	if got := tool.Get("type").String(); got != "image_generation" {
		t.Fatalf("tools.0.type = %q, want image_generation: %s", got, string(out))
	}
}

func TestMaybeAttachImageGenerationToolPreservesExistingImageToolForGPT54Mini(t *testing.T) {
	out := maybeAttachImageGenerationTool("gpt-5.4-mini", []byte(`{"model":"gpt-5.4-mini","tools":[{"type":"image_generation","model":"gpt-image-2"}]}`))

	tools := gjson.GetBytes(out, "tools").Array()
	if got := len(tools); got != 1 {
		t.Fatalf("expected existing image_generation tool to be preserved without duplication, got %d: %s", got, string(out))
	}
	if got := tools[0].Get("model").String(); got != "gpt-image-2" {
		t.Fatalf("tools.0.model = %q, want gpt-image-2: %s", got, string(out))
	}
}

func TestMaybeAttachImageGenerationToolSkipsOtherModels(t *testing.T) {
	out := maybeAttachImageGenerationTool("gpt-4.1", []byte(`{"model":"gpt-4.1","input":"draw a wide poster"}`))

	if gjson.GetBytes(out, "tools").Exists() {
		t.Fatalf("gpt-4.1 must not auto-inject image_generation tools: %s", string(out))
	}
}
