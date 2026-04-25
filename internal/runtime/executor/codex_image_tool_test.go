package executor

import (
	"encoding/json"
	"runtime"
	"strconv"
	"strings"
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

func TestMaybeAttachImageGenerationToolLargePayloadAllocationsStayBounded(t *testing.T) {
	t.Setenv(codexAutoImageToolEnv, "1")
	body := buildLargeCodexBodyWithTools(64, 16*1024)
	_ = maybeAttachImageGenerationTool("gpt-5.4", body)

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	out := maybeAttachImageGenerationTool("gpt-5.4", body)
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(out)

	if !json.Valid(out) {
		t.Fatalf("output JSON is invalid: %s", out)
	}
	if !hasImageGenerationTool(out) {
		t.Fatalf("expected image_generation tool, got %s", gjson.GetBytes(out, "tools").Raw)
	}
	if gjson.GetBytes(out, "tools.#(type=\"image_generation\").size").Exists() {
		t.Fatalf("auto image_generation tool should omit size: %s", string(out))
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	limit := uint64(len(body) + 8192)
	if allocated > limit {
		t.Fatalf("allocated %d bytes for %d-byte body, want <= %d", allocated, len(body), limit)
	}
}

func buildLargeCodexBodyWithTools(messageCount, messageSize int) []byte {
	text := strings.Repeat("x", messageSize)
	var b strings.Builder
	b.Grow(messageCount*messageSize + 256)
	b.WriteString(`{"model":"gpt-5.4","stream":true,"input":[`)
	for i := 0; i < messageCount; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"type":"message","role":"user","content":[{"type":"input_text","text":`)
		b.WriteString(strconv.Quote(text))
		b.WriteString(`}]}`)
	}
	b.WriteString(`],"tools":[{"type":"function","name":"noop","parameters":{"type":"object","properties":{}}}]}`)
	return []byte(b.String())
}
