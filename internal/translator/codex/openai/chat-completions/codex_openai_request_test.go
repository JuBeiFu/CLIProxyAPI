package chat_completions

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToCodex_StripsUnsupportedRemoteImageURL(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gpt-5.4-mini",
		"messages":[
			{
				"role":"user",
				"content":[
					{
						"type":"image_url",
						"image_url":{"url":"https://example.com/demo.png"}
					},
					{
						"type":"text",
						"text":"describe this"
					}
				]
			}
		]
	}`)

	output := ConvertOpenAIRequestToCodex("gpt-5.4-mini", inputJSON, false)
	input := gjson.GetBytes(output, "input")
	if !input.IsArray() || len(input.Array()) != 1 {
		t.Fatalf("expected one input message, got %s", input.Raw)
	}
	content := input.Array()[0].Get("content")
	if !content.IsArray() {
		t.Fatalf("expected content array, got %s", content.Raw)
	}
	if len(content.Array()) != 1 {
		t.Fatalf("expected remote image url part to be stripped, got %s", content.Raw)
	}
	if got := content.Array()[0].Get("type").String(); got != "input_text" {
		t.Fatalf("expected remaining part type input_text, got %q", got)
	}
}

