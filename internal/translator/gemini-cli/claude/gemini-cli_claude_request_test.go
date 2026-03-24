package claude

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToCLI_ToolChoice_SpecificTool(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-flash-preview",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "hi"}
				]
			}
		],
		"tools": [
			{
				"name": "json",
				"description": "A JSON tool",
				"input_schema": {
					"type": "object",
					"properties": {}
				}
			}
		],
		"tool_choice": {"type": "tool", "name": "json"}
	}`)

	output := ConvertClaudeRequestToCLI("gemini-3-flash-preview", inputJSON, false)

	if got := gjson.GetBytes(output, "request.toolConfig.functionCallingConfig.mode").String(); got != "ANY" {
		t.Fatalf("Expected request.toolConfig.functionCallingConfig.mode 'ANY', got '%s'", got)
	}
	allowed := gjson.GetBytes(output, "request.toolConfig.functionCallingConfig.allowedFunctionNames").Array()
	if len(allowed) != 1 || allowed[0].String() != "json" {
		t.Fatalf("Expected allowedFunctionNames ['json'], got %s", gjson.GetBytes(output, "request.toolConfig.functionCallingConfig.allowedFunctionNames").Raw)
	}
}

func TestConvertClaudeRequestToCLI_SanitizesToolSchemaForGemini(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-flash-preview",
		"messages": [],
		"tools": [
			{
				"name": "fetch_url",
				"description": "Fetch a URL",
				"eager_input_streaming": true,
				"input_schema": {
					"type": "object",
					"properties": {
						"url": {
							"type": "string",
							"format": "uri"
						},
						"tags": {
							"type": "array",
							"items": {
								"type": "string"
							},
							"uniqueItems": true
						}
					},
					"required": ["url"]
				}
			}
		]
	}`)

	output := ConvertClaudeRequestToCLI("gemini-3-flash-preview", inputJSON, false)

	tool := gjson.GetBytes(output, "request.tools.0.functionDeclarations.0")
	if !tool.Exists() {
		t.Fatal("sanitized tool declaration should exist")
	}
	if tool.Get("eager_input_streaming").Exists() {
		t.Fatal("eager_input_streaming should be removed")
	}
	if tool.Get("input_schema").Exists() {
		t.Fatal("input_schema should be removed")
	}
	schema := tool.Get("parametersJsonSchema")
	if !schema.Exists() {
		t.Fatal("parametersJsonSchema should exist")
	}
	if schema.Get("properties.url.format").Exists() {
		t.Fatalf("format should be stripped from Gemini schema, got %s", schema.Raw)
	}
	if schema.Get("properties.tags.uniqueItems").Exists() {
		t.Fatalf("uniqueItems should be stripped from Gemini schema, got %s", schema.Raw)
	}
	if got := schema.Get("properties.tags.description").String(); got != "uniqueItems: true" {
		t.Fatalf("expected uniqueItems hint in description, got %q", got)
	}
}
