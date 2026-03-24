package chat_completions

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestNoSpuriousEmptyAssistantMessage(t *testing.T) {
	input := []byte(`{
		"model": "gpt-4o",
		"messages": [
			{"role": "user", "content": "Call a tool"},
			{
				"role": "assistant",
				"content": null,
				"tool_calls": [
					{
						"id": "call_x",
						"type": "function",
						"function": {"name": "do_thing", "arguments": "{}"}
					}
				]
			},
			{"role": "tool", "tool_call_id": "call_x", "content": "done"}
		],
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "do_thing",
					"description": "Do a thing",
					"parameters": {"type": "object", "properties": {}}
				}
			}
		]
	}`)

	out := ConvertOpenAIRequestToCodex("gpt-4o", input, true)
	items := gjson.GetBytes(out, "input").Array()

	if len(items) != 3 {
		t.Fatalf("expected 3 input items (user + function_call + function_call_output), got %d: %s", len(items), gjson.GetBytes(out, "input").Raw)
	}
	if items[0].Get("type").String() != "message" || items[0].Get("role").String() != "user" {
		t.Fatalf("item 0: expected user message, got %s", items[0].Raw)
	}
	if items[1].Get("type").String() != "function_call" {
		t.Fatalf("item 1: expected function_call, got %s", items[1].Raw)
	}
	if items[2].Get("type").String() != "function_call_output" {
		t.Fatalf("item 2: expected function_call_output, got %s", items[2].Raw)
	}
}
