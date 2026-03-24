package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToGemini_FunctionCallOutputInvalidJSONFallsBackToString(t *testing.T) {
	inputJSON := []byte(`{
		"input": [
			{
				"type": "function_call",
				"call_id": "call_123",
				"name": "read_file",
				"arguments": "{\"path\":\"/tmp/demo.txt\"}"
			},
			{
				"type": "function_call_output",
				"call_id": "call_123",
				"output": "{invalid json"
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToGemini("gemini-2.5-pro", inputJSON, false)
	outputStr := string(output)

	result := gjson.Get(outputStr, "contents.1.parts.0.functionResponse.response.result")
	if !result.Exists() {
		t.Fatal("functionResponse.response.result should exist")
	}
	if result.Type != gjson.String {
		t.Fatalf("Expected string result for invalid JSON output, got type %v with raw %s", result.Type, result.Raw)
	}
	if got := result.String(); got != "{invalid json" {
		t.Fatalf("Expected invalid JSON output to be preserved as string, got %q", got)
	}
}
