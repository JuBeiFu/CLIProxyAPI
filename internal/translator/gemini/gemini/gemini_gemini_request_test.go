package gemini

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestBackfillEmptyFunctionResponseNames_Single(t *testing.T) {
	input := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "Bash", "args": {"cmd": "ls"}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "", "response": {"output": "file1.txt"}}}
				]
			}
		]
	}`)

	out := backfillEmptyFunctionResponseNames(input)
	if got := gjson.GetBytes(out, "contents.1.parts.0.functionResponse.name").String(); got != "Bash" {
		t.Fatalf("Expected backfilled name 'Bash', got %q", got)
	}
}

func TestConvertGeminiRequestToGemini_BackfillsEmptyName(t *testing.T) {
	input := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "Bash", "args": {"cmd": "ls"}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "", "response": {"output": "file1.txt"}}}
				]
			}
		]
	}`)

	out := ConvertGeminiRequestToGemini("", input, false)
	if got := gjson.GetBytes(out, "contents.1.parts.0.functionResponse.name").String(); got != "Bash" {
		t.Fatalf("Expected backfilled name 'Bash', got %q", got)
	}
}
