package executor

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexDefaultImageToolModel = "gpt-5.4-mini"

func shouldAutoAttachImageGenerationTool(model string) bool {
	return strings.EqualFold(strings.TrimSpace(model), codexDefaultImageToolModel)
}

func hasImageGenerationTool(body []byte) bool {
	toolsResult := gjson.GetBytes(body, "tools")
	if !toolsResult.Exists() || !toolsResult.IsArray() {
		return false
	}
	for _, tool := range toolsResult.Array() {
		if strings.EqualFold(strings.TrimSpace(tool.Get("type").String()), "image_generation") {
			return true
		}
	}
	return false
}

func maybeAttachImageGenerationTool(upstreamModel string, body []byte) []byte {
	if !shouldAutoAttachImageGenerationTool(upstreamModel) {
		return body
	}
	if hasImageGenerationTool(body) {
		return body
	}

	tool := []byte(`{"type":"image_generation"}`)
	toolsResult := gjson.GetBytes(body, "tools")
	if toolsResult.Exists() && toolsResult.IsArray() {
		if updated, err := sjson.SetRawBytes(body, "tools.-1", tool); err == nil {
			return updated
		}
		return body
	}
	if updated, err := sjson.SetRawBytes(body, "tools", []byte("["+string(tool)+"]")); err == nil {
		return updated
	}
	return body
}
