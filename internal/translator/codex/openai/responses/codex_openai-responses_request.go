package responses

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func ConvertOpenAIResponsesRequestToCodex(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON

	inputResult := gjson.GetBytes(rawJSON, "input")
	if inputResult.Type == gjson.String {
		input, _ := sjson.SetBytes([]byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}]`), "0.content.0.text", inputResult.String())
		rawJSON, _ = sjson.SetRawBytes(rawJSON, "input", input)
	}

	// Some clients feed the previous response output back into `input` when they
	// are not using `previous_response_id` incremental mode. The synthetic / real
	// reasoning items (ids like "rs_...") are output-only and will break upstream
	// requests with errors like:
	// "Item with id 'rs_...' not found. Items are not persisted when `store` is set to false."
	rawJSON = normalizeCodexInputArray(rawJSON)
	rawJSON = normalizeCodexBuiltinTools(rawJSON)
	rawJSON = normalizeCodexRequestEnvelope(rawJSON)

	return rawJSON
}

func normalizeCodexRequestEnvelope(rawJSON []byte) []byte {
	root := gjson.ParseBytes(rawJSON)
	if !root.IsObject() {
		return rawJSON
	}

	var buf bytes.Buffer
	buf.Grow(len(rawJSON) + 96)
	buf.WriteByte('{')

	wrote := false
	sawStore := false
	sawStream := false
	sawParallel := false
	sawInclude := false

	root.ForEach(func(key, value gjson.Result) bool {
		keyName := key.String()
		switch keyName {
		case "max_output_tokens", "max_completion_tokens", "temperature", "top_p", "truncation", "user", "context_management":
			return true
		case "service_tier":
			if value.String() != "priority" {
				return true
			}
			appendJSONObjectFieldRaw(&buf, &wrote, keyName, value.Raw)
			return true
		case "stream":
			sawStream = true
			appendJSONObjectFieldRaw(&buf, &wrote, keyName, "true")
			return true
		case "store":
			sawStore = true
			appendJSONObjectFieldRaw(&buf, &wrote, keyName, value.Raw)
			return true
		case "parallel_tool_calls":
			sawParallel = true
			appendJSONObjectFieldRaw(&buf, &wrote, keyName, "true")
			return true
		case "include":
			sawInclude = true
			appendJSONObjectFieldRaw(&buf, &wrote, keyName, `["reasoning.encrypted_content"]`)
			return true
		default:
			appendJSONObjectFieldRaw(&buf, &wrote, keyName, value.Raw)
			return true
		}
	})

	if !sawStream {
		appendJSONObjectFieldRaw(&buf, &wrote, "stream", "true")
	}
	if !sawStore {
		appendJSONObjectFieldRaw(&buf, &wrote, "store", "false")
	}
	if !sawParallel {
		appendJSONObjectFieldRaw(&buf, &wrote, "parallel_tool_calls", "true")
	}
	if !sawInclude {
		appendJSONObjectFieldRaw(&buf, &wrote, "include", `["reasoning.encrypted_content"]`)
	}

	buf.WriteByte('}')
	return buf.Bytes()
}

func appendJSONObjectFieldRaw(buf *bytes.Buffer, wrote *bool, key, rawValue string) {
	if *wrote {
		buf.WriteByte(',')
	}
	buf.WriteString(strconv.Quote(key))
	buf.WriteByte(':')
	buf.WriteString(rawValue)
	*wrote = true
}

func normalizeCodexInputArray(rawJSON []byte) []byte {
	input := gjson.GetBytes(rawJSON, "input")
	if !input.Exists() || !input.IsArray() {
		return rawJSON
	}

	items := input.Array()
	if len(items) == 0 {
		return rawJSON
	}

	var buf bytes.Buffer
	buf.Grow(len(input.Raw))
	buf.WriteByte('[')
	wrote := false
	changed := false

	for _, item := range items {
		itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
		itemID := strings.TrimSpace(item.Get("id").String())

		if itemType == "reasoning" {
			changed = true
			continue
		}
		// Ignore reasoning item references accidentally copied into follow-up requests.
		if strings.HasPrefix(itemID, "rs_") && (itemType == "" || itemType == "item_reference") {
			changed = true
			continue
		}

		itemRaw := []byte(item.Raw)
		if itemID != "" && !isSupportedCodexInputID(itemType, itemID) {
			updated, err := sjson.DeleteBytes(itemRaw, "id")
			if err != nil {
				return rawJSON
			}
			itemRaw = updated
			changed = true
		}
		if item.Get("role").String() == "system" {
			updated, err := sjson.SetBytes(itemRaw, "role", "developer")
			if err != nil {
				return rawJSON
			}
			itemRaw = updated
			changed = true
		}

		if wrote {
			buf.WriteByte(',')
		}
		buf.Write(itemRaw)
		wrote = true
	}

	buf.WriteByte(']')
	if !changed {
		return rawJSON
	}

	updated, err := sjson.SetRawBytes(rawJSON, "input", buf.Bytes())
	if err != nil {
		return rawJSON
	}
	return updated
}

func isSupportedCodexInputID(itemType, itemID string) bool {
	switch itemType {
	case "message":
		return strings.HasPrefix(itemID, "msg")
	case "function_call":
		return strings.HasPrefix(itemID, "fc_") || strings.HasPrefix(itemID, "fc-")
	case "function_call_output":
		return true
	default:
		return false
	}
}

// normalizeCodexBuiltinTools rewrites legacy/preview built-in tool variants to the
// stable names expected by the current Codex upstream.
func normalizeCodexBuiltinTools(rawJSON []byte) []byte {
	result := rawJSON

	tools := gjson.GetBytes(result, "tools")
	if tools.IsArray() {
		if normalized, changed := normalizeCodexBuiltinToolArrayRaw(tools); changed {
			if updated, err := sjson.SetRawBytes(result, "tools", normalized); err == nil {
				result = updated
			}
		}
	}

	if toolChoice := gjson.GetBytes(result, "tool_choice"); toolChoice.IsObject() {
		if normalized, changed := normalizeCodexBuiltinToolObjectRaw([]byte(toolChoice.Raw)); changed {
			if updated, err := sjson.SetRawBytes(result, "tool_choice", normalized); err == nil {
				result = updated
			}
		}
	}

	toolChoiceTools := gjson.GetBytes(result, "tool_choice.tools")
	if toolChoiceTools.IsArray() {
		if normalized, changed := normalizeCodexBuiltinToolArrayRaw(toolChoiceTools); changed {
			if updated, err := sjson.SetRawBytes(result, "tool_choice.tools", normalized); err == nil {
				result = updated
			}
		}
	}

	return result
}

func normalizeCodexBuiltinToolArrayRaw(tools gjson.Result) ([]byte, bool) {
	toolArray := tools.Array()
	if len(toolArray) == 0 {
		return nil, false
	}

	var buf bytes.Buffer
	buf.Grow(len(tools.Raw))
	buf.WriteByte('[')
	changed := false

	for i, tool := range toolArray {
		normalized, toolChanged := normalizeCodexBuiltinToolObjectRaw([]byte(tool.Raw))
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(normalized)
		if toolChanged {
			changed = true
		}
	}

	buf.WriteByte(']')
	if !changed {
		return nil, false
	}
	return buf.Bytes(), true
}

func normalizeCodexBuiltinToolObjectRaw(rawJSON []byte) ([]byte, bool) {
	result := rawJSON
	changed := false

	toolType := gjson.GetBytes(result, "type").String()
	if normalized := normalizeCodexBuiltinToolType(toolType); normalized != "" {
		if updated, err := sjson.SetBytes(result, "type", normalized); err == nil {
			result = updated
			toolType = normalized
			changed = true
		}
	}

	if toolType == "function" {
		if function := gjson.GetBytes(result, "function"); function.Exists() && function.IsObject() {
			if name := function.Get("name"); name.Exists() && strings.TrimSpace(name.String()) != "" {
				if updated, err := sjson.SetBytes(result, "name", name.String()); err == nil {
					result = updated
					changed = true
				}
			}
			if description := function.Get("description"); description.Exists() {
				if updated, err := sjson.SetBytes(result, "description", description.Value()); err == nil {
					result = updated
					changed = true
				}
			}
			if parameters := function.Get("parameters"); parameters.Exists() {
				if updated, err := sjson.SetRawBytes(result, "parameters", []byte(parameters.Raw)); err == nil {
					result = updated
					changed = true
				}
			}
			if strict := function.Get("strict"); strict.Exists() {
				if updated, err := sjson.SetBytes(result, "strict", strict.Value()); err == nil {
					result = updated
					changed = true
				}
			}
			if updated, err := sjson.DeleteBytes(result, "function"); err == nil {
				result = updated
				changed = true
			}
		}
	}

	switch toolType {
	case "tool_search":
		if updated, err := sjson.SetBytes(result, "type", "function"); err == nil {
			result = updated
			changed = true
		}
		if updated, err := sjson.SetBytes(result, "name", "tool_search"); err == nil {
			result = updated
			changed = true
		}
		if gjson.GetBytes(result, "execution").Exists() {
			if updated, err := sjson.DeleteBytes(result, "execution"); err == nil {
				result = updated
				changed = true
			}
		}
	}

	return result, changed
}

// normalizeCodexBuiltinToolType centralizes the current known Codex Responses
// built-in tool alias compatibility. If Codex introduces more legacy aliases,
// extend this helper instead of adding path-specific rewrite logic elsewhere.
func normalizeCodexBuiltinToolType(toolType string) string {
	switch toolType {
	case "web_search_preview", "web_search_preview_2025_03_11":
		return "web_search"
	default:
		return ""
	}
}
