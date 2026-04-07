package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertCodexResponseToOpenAI_StreamSetsModelFromResponseCreated(t *testing.T) {
	ctx := context.Background()
	var param any

	modelName := "gpt-5.3-codex"

	out := ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.created","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.3-codex"}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected no output for response.created, got %d chunks", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":"hello"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotModel := gjson.Get(out[0], "model").String()
	if gotModel != modelName {
		t.Fatalf("expected model %q, got %q", modelName, gotModel)
	}
}

func TestConvertCodexResponseToOpenAI_FirstChunkUsesRequestModelName(t *testing.T) {
	ctx := context.Background()
	var param any

	modelName := "gpt-5.3-codex"

	out := ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":"hello"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotModel := gjson.Get(out[0], "model").String()
	if gotModel != modelName {
		t.Fatalf("expected model %q, got %q", modelName, gotModel)
	}
}

func TestConvertCodexResponseToOpenAINonStreamFallsBackToAccumulatedStreamText(t *testing.T) {
	ctx := context.Background()
	var param any

	modelName := "gpt-5.4"

	created := ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.created","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4"}}`), &param)
	if len(created) != 0 {
		t.Fatalf("expected no output for response.created, got %d chunks", len(created))
	}

	delta := ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":"hello world"}`), &param)
	if len(delta) != 1 {
		t.Fatalf("expected 1 chunk for output_text.delta, got %d", len(delta))
	}

	completed := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":13,"total_tokens":20}}}`)
	result := ConvertCodexResponseToOpenAINonStream(ctx, modelName, nil, nil, completed, &param)
	if result == "" {
		t.Fatal("expected non-stream conversion output")
	}

	if got := gjson.Get(result, "choices.0.message.role").String(); got != "assistant" {
		t.Fatalf("message.role = %q, want assistant; result=%s", got, result)
	}
	if got := gjson.Get(result, "choices.0.message.content").String(); got != "hello world" {
		t.Fatalf("message.content = %q, want hello world; result=%s", got, result)
	}
	if got := gjson.Get(result, "choices.0.finish_reason").String(); got != "stop" {
		t.Fatalf("finish_reason = %q, want stop; result=%s", got, result)
	}
}
