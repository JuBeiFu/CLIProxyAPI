package responses

import (
	"fmt"
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

	rawJSON, _ = sjson.SetBytes(rawJSON, "stream", true)

	// Some clients feed the previous response output back into `input` when they
	// are not using `previous_response_id` incremental mode. The synthetic / real
	// reasoning items (ids like "rs_...") are output-only and will break upstream
	// requests with errors like:
	// "Item with id 'rs_...' not found. Items are not persisted when `store` is set to false."
	rawJSON = stripOutputOnlyReasoningFromInput(rawJSON)
	rawJSON = stripUnsupportedCodexInputIDs(rawJSON)

	// Preserve client preference when explicitly provided.
	if !gjson.GetBytes(rawJSON, "store").Exists() {
		rawJSON, _ = sjson.SetBytes(rawJSON, "store", false)
	}
	rawJSON, _ = sjson.SetBytes(rawJSON, "parallel_tool_calls", true)
	rawJSON, _ = sjson.SetBytes(rawJSON, "include", []string{"reasoning.encrypted_content"})
	rawJSON = normalizeCodexBuiltinTools(rawJSON)
	// Codex Responses rejects token limit fields, so strip them out before forwarding.
	rawJSON, _ = sjson.DeleteBytes(rawJSON, "max_output_tokens")
	rawJSON, _ = sjson.DeleteBytes(rawJSON, "max_completion_tokens")
	rawJSON, _ = sjson.DeleteBytes(rawJSON, "temperature")
	rawJSON, _ = sjson.DeleteBytes(rawJSON, "top_p")
	if v := gjson.GetBytes(rawJSON, "service_tier"); v.Exists() {
		if v.String() != "priority" {
			rawJSON, _ = sjson.DeleteBytes(rawJSON, "service_tier")
		}
	}

	rawJSON, _ = sjson.DeleteBytes(rawJSON, "truncation")
	rawJSON = applyResponsesCompactionCompatibility(rawJSON)

	// Delete the user field as it is not supported by the Codex upstream.
	rawJSON, _ = sjson.DeleteBytes(rawJSON, "user")

	// Convert role "system" to "developer" in input array to comply with Codex API requirements.
	rawJSON = convertSystemRoleToDeveloper(rawJSON)

	return rawJSON
}

func stripOutputOnlyReasoningFromInput(rawJSON []byte) []byte {
	input := gjson.GetBytes(rawJSON, "input")
	if !input.Exists() || !input.IsArray() {
		return rawJSON
	}

	items := input.Array()
	if len(items) == 0 {
		return rawJSON
	}

	result := []byte(`{"arr":[]}`)
	for _, item := range items {
		itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
		itemID := strings.TrimSpace(item.Get("id").String())

		if itemType == "reasoning" {
			continue
		}
		// Ignore reasoning item references accidentally copied into follow-up requests.
		if strings.HasPrefix(itemID, "rs_") && (itemType == "" || itemType == "item_reference") {
			continue
		}

		var err error
		result, err = sjson.SetRawBytes(result, "arr.-1", []byte(item.Raw))
		if err != nil {
			return rawJSON
		}
	}

	arr := gjson.GetBytes(result, "arr").Raw
	updated, err := sjson.SetRawBytes(rawJSON, "input", []byte(arr))
	if err != nil {
		return rawJSON
	}
	return updated
}

func stripUnsupportedCodexInputIDs(rawJSON []byte) []byte {
	input := gjson.GetBytes(rawJSON, "input")
	if !input.Exists() || !input.IsArray() {
		return rawJSON
	}

	result := rawJSON
	for idx, item := range input.Array() {
		itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
		itemID := strings.TrimSpace(item.Get("id").String())
		if itemID == "" {
			continue
		}

		keep := false
		switch itemType {
		case "message":
			keep = strings.HasPrefix(itemID, "msg")
		case "function_call":
			keep = strings.HasPrefix(itemID, "fc_") || strings.HasPrefix(itemID, "fc-")
		case "function_call_output":
			keep = true
		default:
			keep = false
		}
		if keep {
			continue
		}

		updated, err := sjson.DeleteBytes(result, fmt.Sprintf("input.%d.id", idx))
		if err != nil {
			return rawJSON
		}
		result = updated
	}
	return result
}

// applyResponsesCompactionCompatibility handles OpenAI Responses context_management.compaction
// for Codex upstream compatibility.
//
// Codex /responses currently rejects context_management with:
// {"detail":"Unsupported parameter: context_management"}.
//
// Compatibility strategy:
// 1) Remove context_management before forwarding to Codex upstream.
func applyResponsesCompactionCompatibility(rawJSON []byte) []byte {
	if !gjson.GetBytes(rawJSON, "context_management").Exists() {
		return rawJSON
	}

	rawJSON, _ = sjson.DeleteBytes(rawJSON, "context_management")
	return rawJSON
}

// convertSystemRoleToDeveloper traverses the input array and converts any message items
// with role "system" to role "developer". This is necessary because Codex API does not
// accept "system" role in the input array.
func convertSystemRoleToDeveloper(rawJSON []byte) []byte {
	inputResult := gjson.GetBytes(rawJSON, "input")
	if !inputResult.IsArray() {
		return rawJSON
	}

	inputArray := inputResult.Array()
	result := rawJSON

	// Directly modify role values for items with "system" role
	for i := 0; i < len(inputArray); i++ {
		rolePath := fmt.Sprintf("input.%d.role", i)
		if gjson.GetBytes(result, rolePath).String() == "system" {
			result, _ = sjson.SetBytes(result, rolePath, "developer")
		}
	}

	return result
}

func normalizeCodexBuiltinTools(rawJSON []byte) []byte {
	result := rawJSON

	tools := gjson.GetBytes(result, "tools")
	if tools.IsArray() {
		toolArray := tools.Array()
		for i := 0; i < len(toolArray); i++ {
			result = normalizeCodexBuiltinToolObject(result, fmt.Sprintf("tools.%d", i))
		}
	}

	if gjson.GetBytes(result, "tool_choice").IsObject() {
		result = normalizeCodexBuiltinToolObject(result, "tool_choice")
	}

	toolChoiceTools := gjson.GetBytes(result, "tool_choice.tools")
	if toolChoiceTools.IsArray() {
		toolArray := toolChoiceTools.Array()
		for i := 0; i < len(toolArray); i++ {
			result = normalizeCodexBuiltinToolObject(result, fmt.Sprintf("tool_choice.tools.%d", i))
		}
	}

	return result
}

func normalizeCodexBuiltinToolObject(rawJSON []byte, path string) []byte {
	result := rawJSON
	typePath := path + ".type"
	toolType := gjson.GetBytes(result, typePath).String()
	if normalized := normalizeCodexBuiltinToolType(toolType); normalized != "" {
		if updated, err := sjson.SetBytes(result, typePath, normalized); err == nil {
			result = updated
			toolType = normalized
		}
	}

	if toolType == "function" {
		functionPath := path + ".function"
		if function := gjson.GetBytes(result, functionPath); function.Exists() && function.IsObject() {
			if name := function.Get("name"); name.Exists() && strings.TrimSpace(name.String()) != "" {
				if updated, err := sjson.SetBytes(result, path+".name", name.String()); err == nil {
					result = updated
				}
			}
			if description := function.Get("description"); description.Exists() {
				if updated, err := sjson.SetBytes(result, path+".description", description.Value()); err == nil {
					result = updated
				}
			}
			if parameters := function.Get("parameters"); parameters.Exists() {
				if updated, err := sjson.SetRawBytes(result, path+".parameters", []byte(parameters.Raw)); err == nil {
					result = updated
				}
			}
			if strict := function.Get("strict"); strict.Exists() {
				if updated, err := sjson.SetBytes(result, path+".strict", strict.Value()); err == nil {
					result = updated
				}
			}
			if updated, err := sjson.DeleteBytes(result, functionPath); err == nil {
				result = updated
			}
		}
	}

	switch toolType {
	case "tool_search":
		// Codex app-server exposes tool_search as a native Responses tool, but
		// OpenAI-compatible upstreams validate it as a named function tool.
		if updated, err := sjson.SetBytes(result, typePath, "function"); err == nil {
			result = updated
		}
		if updated, err := sjson.SetBytes(result, path+".name", "tool_search"); err == nil {
			result = updated
		}
		if gjson.GetBytes(result, path+".execution").Exists() {
			if updated, err := sjson.DeleteBytes(result, path+".execution"); err == nil {
				result = updated
			}
		}
	}

	return result
}

func normalizeCodexBuiltinToolType(toolType string) string {
	switch toolType {
	case "web_search_preview", "web_search_preview_2025_03_11":
		return "web_search"
	default:
		return ""
	}
}
