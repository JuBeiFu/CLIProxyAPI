package responses

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
)

func ConvertOpenAIResponsesRequestToCodex(modelName string, inputRawJSON []byte, _ bool) []byte {
	return normalizeCodexRequestEnvelope(inputRawJSON)
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
		case "input":
			switch {
			case value.Type == gjson.String:
				appendJSONObjectFieldBytes(&buf, &wrote, keyName, buildCodexStringInputRaw(value))
			case value.IsArray():
				if normalized, changed := normalizeCodexInputArrayRaw(value); changed {
					appendJSONObjectFieldBytes(&buf, &wrote, keyName, normalized)
				} else {
					appendJSONObjectFieldRaw(&buf, &wrote, keyName, value.Raw)
				}
			default:
				appendJSONObjectFieldRaw(&buf, &wrote, keyName, value.Raw)
			}
			return true
		case "tools":
			if value.IsArray() {
				if normalized, changed := normalizeCodexBuiltinToolArrayRaw(value); changed {
					appendJSONObjectFieldBytes(&buf, &wrote, keyName, normalized)
					return true
				}
			}
			appendJSONObjectFieldRaw(&buf, &wrote, keyName, value.Raw)
			return true
		case "tool_choice":
			if value.IsObject() {
				if normalized, changed := normalizeCodexToolChoiceObjectRaw(value); changed {
					appendJSONObjectFieldBytes(&buf, &wrote, keyName, normalized)
					return true
				}
			}
			appendJSONObjectFieldRaw(&buf, &wrote, keyName, value.Raw)
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
	writeJSONString(buf, key)
	buf.WriteByte(':')
	buf.WriteString(rawValue)
	*wrote = true
}

func appendJSONObjectFieldBytes(buf *bytes.Buffer, wrote *bool, key string, rawValue []byte) {
	if *wrote {
		buf.WriteByte(',')
	}
	writeJSONString(buf, key)
	buf.WriteByte(':')
	buf.Write(rawValue)
	*wrote = true
}

func writeJSONString(buf *bytes.Buffer, value string) {
	buf.WriteByte('"')
	start := 0
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c != '\\' && c != '"' && c >= 0x20 {
			continue
		}
		if start < i {
			buf.WriteString(value[start:i])
		}
		switch c {
		case '\\', '"':
			buf.WriteByte('\\')
			buf.WriteByte(c)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			buf.WriteString(`\u00`)
			const hex = "0123456789abcdef"
			buf.WriteByte(hex[c>>4])
			buf.WriteByte(hex[c&0x0f])
		}
		start = i + 1
	}
	if start < len(value) {
		buf.WriteString(value[start:])
	}
	buf.WriteByte('"')
}

func buildCodexStringInputRaw(input gjson.Result) []byte {
	var buf bytes.Buffer
	buf.Grow(len(input.Raw) + 72)
	buf.WriteString(`[{"type":"message","role":"user","content":[{"type":"input_text","text":`)
	buf.WriteString(input.Raw)
	buf.WriteString(`}]}]`)
	return buf.Bytes()
}

func normalizeCodexInputArrayRaw(input gjson.Result) ([]byte, bool) {
	var buf bytes.Buffer
	buf.Grow(len(input.Raw))
	buf.WriteByte('[')
	wrote := false
	changed := false

	input.ForEach(func(_, item gjson.Result) bool {
		itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
		itemID := strings.TrimSpace(item.Get("id").String())

		if itemType == "reasoning" {
			changed = true
			return true
		}
		// Ignore reasoning item references accidentally copied into follow-up requests.
		if strings.HasPrefix(itemID, "rs_") && (itemType == "" || itemType == "item_reference") {
			changed = true
			return true
		}

		if wrote {
			buf.WriteByte(',')
		}
		if normalized, itemChanged := normalizeCodexInputObjectRaw(item, itemType, itemID); itemChanged {
			buf.Write(normalized)
			changed = true
		} else {
			buf.WriteString(item.Raw)
		}
		wrote = true
		return true
	})

	buf.WriteByte(']')
	if !changed {
		return nil, false
	}
	return buf.Bytes(), true
}

func normalizeCodexInputObjectRaw(item gjson.Result, itemType, itemID string) ([]byte, bool) {
	if !item.IsObject() {
		return nil, false
	}

	dropID := itemID != "" && !isSupportedCodexInputID(itemType, itemID)
	rewriteRole := item.Get("role").String() == "system"
	if !dropID && !rewriteRole {
		return nil, false
	}

	var buf bytes.Buffer
	buf.Grow(len(item.Raw))
	buf.WriteByte('{')
	wrote := false

	item.ForEach(func(key, value gjson.Result) bool {
		keyName := key.String()
		if keyName == "id" && dropID {
			return true
		}
		if keyName == "role" && value.String() == "system" {
			appendJSONObjectFieldRaw(&buf, &wrote, keyName, `"developer"`)
			return true
		}
		appendJSONObjectFieldRaw(&buf, &wrote, keyName, value.Raw)
		return true
	})

	buf.WriteByte('}')
	return buf.Bytes(), true
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
	root := gjson.ParseBytes(rawJSON)
	if !root.IsObject() {
		return rawJSON
	}

	var normalizedTools []byte
	toolsChanged := false
	if tools := root.Get("tools"); tools.IsArray() {
		normalizedTools, toolsChanged = normalizeCodexBuiltinToolArrayRaw(tools)
	}

	var normalizedToolChoice []byte
	toolChoiceChanged := false
	if toolChoice := root.Get("tool_choice"); toolChoice.IsObject() {
		normalizedToolChoice, toolChoiceChanged = normalizeCodexToolChoiceObjectRaw(toolChoice)
	}

	if !toolsChanged && !toolChoiceChanged {
		return rawJSON
	}

	var buf bytes.Buffer
	buf.Grow(len(rawJSON))
	buf.WriteByte('{')
	wrote := false

	root.ForEach(func(key, value gjson.Result) bool {
		keyName := key.String()
		switch keyName {
		case "tools":
			if toolsChanged {
				appendJSONObjectFieldBytes(&buf, &wrote, keyName, normalizedTools)
				return true
			}
		case "tool_choice":
			if toolChoiceChanged {
				appendJSONObjectFieldBytes(&buf, &wrote, keyName, normalizedToolChoice)
				return true
			}
		}
		appendJSONObjectFieldRaw(&buf, &wrote, keyName, value.Raw)
		return true
	})

	buf.WriteByte('}')
	return buf.Bytes()
}

func normalizeCodexBuiltinToolArrayRaw(tools gjson.Result) ([]byte, bool) {
	var buf bytes.Buffer
	buf.Grow(len(tools.Raw))
	buf.WriteByte('[')
	changed := false
	wrote := false

	tools.ForEach(func(_, tool gjson.Result) bool {
		normalized, toolChanged := normalizeCodexBuiltinToolObjectResult(tool, false)
		if wrote {
			buf.WriteByte(',')
		}
		if toolChanged {
			buf.Write(normalized)
			changed = true
		} else {
			buf.WriteString(tool.Raw)
		}
		wrote = true
		return true
	})

	buf.WriteByte(']')
	if !changed {
		return nil, false
	}
	return buf.Bytes(), true
}

func normalizeCodexBuiltinToolObjectRaw(rawJSON []byte) ([]byte, bool) {
	root := gjson.ParseBytes(rawJSON)
	if !root.IsObject() {
		return rawJSON, false
	}
	if normalized, changed := normalizeCodexBuiltinToolObjectResult(root, false); changed {
		return normalized, true
	}
	return rawJSON, false
}

func normalizeCodexToolChoiceObjectRaw(toolChoice gjson.Result) ([]byte, bool) {
	return normalizeCodexBuiltinToolObjectResult(toolChoice, true)
}

type codexToolObjectRewrite struct {
	changed        bool
	finalTypeRaw   string
	toolSearch     bool
	flattenFunc    bool
	nameRaw        string
	descriptionRaw string
	parametersRaw  string
	strictRaw      string
}

func normalizeCodexBuiltinToolObjectResult(tool gjson.Result, normalizeNestedTools bool) ([]byte, bool) {
	if !tool.IsObject() {
		return nil, false
	}

	rewrite := inspectCodexToolObjectRewrite(tool)

	var normalizedNestedTools []byte
	nestedToolsChanged := false
	if normalizeNestedTools {
		if nestedTools := tool.Get("tools"); nestedTools.IsArray() {
			normalizedNestedTools, nestedToolsChanged = normalizeCodexBuiltinToolArrayRaw(nestedTools)
		}
	}

	if !rewrite.changed && !nestedToolsChanged {
		return nil, false
	}

	var buf bytes.Buffer
	buf.Grow(len(tool.Raw))
	buf.WriteByte('{')
	wrote := false

	tool.ForEach(func(key, value gjson.Result) bool {
		keyName := key.String()
		switch {
		case rewrite.flattenFunc && keyName == "function":
			return true
		case rewrite.toolSearch && keyName == "execution":
			return true
		case rewrite.toolSearch && keyName == "name":
			return true
		case rewrite.flattenFunc && keyName == "name" && rewrite.nameRaw != "":
			return true
		case rewrite.flattenFunc && keyName == "description" && rewrite.descriptionRaw != "":
			return true
		case rewrite.flattenFunc && keyName == "parameters" && rewrite.parametersRaw != "":
			return true
		case rewrite.flattenFunc && keyName == "strict" && rewrite.strictRaw != "":
			return true
		case nestedToolsChanged && keyName == "tools":
			appendJSONObjectFieldBytes(&buf, &wrote, keyName, normalizedNestedTools)
			return true
		case rewrite.finalTypeRaw != "" && keyName == "type":
			appendJSONObjectFieldRaw(&buf, &wrote, keyName, rewrite.finalTypeRaw)
			return true
		default:
			appendJSONObjectFieldRaw(&buf, &wrote, keyName, value.Raw)
			return true
		}
	})

	if rewrite.nameRaw != "" {
		appendJSONObjectFieldRaw(&buf, &wrote, "name", rewrite.nameRaw)
	}
	if rewrite.descriptionRaw != "" {
		appendJSONObjectFieldRaw(&buf, &wrote, "description", rewrite.descriptionRaw)
	}
	if rewrite.parametersRaw != "" {
		appendJSONObjectFieldRaw(&buf, &wrote, "parameters", rewrite.parametersRaw)
	}
	if rewrite.strictRaw != "" {
		appendJSONObjectFieldRaw(&buf, &wrote, "strict", rewrite.strictRaw)
	}
	if rewrite.toolSearch {
		appendJSONObjectFieldRaw(&buf, &wrote, "name", `"tool_search"`)
	}

	buf.WriteByte('}')
	return buf.Bytes(), true
}

func inspectCodexToolObjectRewrite(tool gjson.Result) codexToolObjectRewrite {
	toolType := tool.Get("type").String()
	rewrite := codexToolObjectRewrite{}

	if normalized := normalizeCodexBuiltinToolType(toolType); normalized != "" {
		rewrite.finalTypeRaw = `"` + normalized + `"`
		rewrite.changed = true
		toolType = normalized
	}

	if toolType == "function" {
		if function := tool.Get("function"); function.Exists() && function.IsObject() {
			rewrite.changed = true
			rewrite.flattenFunc = true
			if name := function.Get("name"); name.Exists() && strings.TrimSpace(name.String()) != "" {
				rewrite.nameRaw = rawJSONString(name.String())
			}
			if description := function.Get("description"); description.Exists() {
				rewrite.descriptionRaw = description.Raw
			}
			if parameters := function.Get("parameters"); parameters.Exists() {
				rewrite.parametersRaw = parameters.Raw
			}
			if strict := function.Get("strict"); strict.Exists() {
				rewrite.strictRaw = strict.Raw
			}
		}
	}

	if toolType == "tool_search" {
		rewrite.changed = true
		rewrite.toolSearch = true
		rewrite.finalTypeRaw = `"function"`
	}

	return rewrite
}

func rawJSONString(value string) string {
	var buf bytes.Buffer
	buf.Grow(len(value) + 2)
	writeJSONString(&buf, value)
	return buf.String()
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
