package gemini

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertGeminiRequestToGeminiCLI_FiltersEmptyParts(t *testing.T) {
	inputJSON := []byte(`{
		"contents": [
			{
				"role": "user",
				"parts": []
			},
			{
				"role": "model",
				"parts": [
					{
						"text": "hello"
					}
				]
			},
			{
				"role": "user"
			}
		]
	}`)

	output := ConvertGeminiRequestToGeminiCLI("", inputJSON, false)

	contents := gjson.GetBytes(output, "request.contents").Array()
	if len(contents) != 1 {
		t.Fatalf("Expected only non-empty contents to remain, got %d entries: %s", len(contents), gjson.GetBytes(output, "request.contents").Raw)
	}
	if got := gjson.GetBytes(output, "request.contents.0.role").String(); got != "model" {
		t.Fatalf("Expected remaining content role 'model', got %q", got)
	}
	if got := gjson.GetBytes(output, "request.contents.0.parts.0.text").String(); got != "hello" {
		t.Fatalf("Expected remaining content text 'hello', got %q", got)
	}
}

func TestConvertGeminiRequestToGeminiCLI_BackfillsEmptyFunctionResponseName(t *testing.T) {
	inputJSON := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "Bash", "args": {"cmd": "ls"}}}
				]
			},
			{
				"role": "function",
				"parts": [
					{"functionResponse": {"name": "", "response": {"result": "file1.txt"}}}
				]
			}
		]
	}`)

	output := ConvertGeminiRequestToGeminiCLI("", inputJSON, false)

	if got := gjson.GetBytes(output, "request.contents.1.parts.0.functionResponse.name").String(); got != "Bash" {
		t.Fatalf("Expected backfilled name 'Bash', got %q", got)
	}
}
