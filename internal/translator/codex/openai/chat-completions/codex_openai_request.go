// Package openai provides utilities to translate OpenAI Chat Completions
// request JSON into OpenAI Responses API request JSON using gjson/sjson.
// It supports tools, multimodal text/image inputs, and Structured Outputs.
// The package handles the conversion of OpenAI API requests into the format
// expected by the OpenAI Responses API, including proper mapping of messages,
// tools, and generation parameters.
package chat_completions

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertOpenAIRequestToCodex converts an OpenAI Chat Completions request JSON
// into an OpenAI Responses API request JSON. The transformation follows the
// examples defined in docs/2.md exactly, including tools, multi-turn dialog,
// multimodal text/image handling, and Structured Outputs mapping.
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the OpenAI Chat Completions API
//   - stream: A boolean indicating if the request is for a streaming response
//
// Returns:
//   - []byte: The transformed request data in OpenAI Responses API format
func ConvertOpenAIRequestToCodex(modelName string, inputRawJSON []byte, stream bool) []byte {
	rawJSON := inputRawJSON
	// Start with empty JSON object
	out := []byte(`{"instructions":""}`)

	// Stream must be set to true
	out, _ = sjson.SetBytes(out, "stream", stream)

	// Codex not support temperature, top_p, top_k, max_output_tokens, so comment them
	// if v := gjson.GetBytes(rawJSON, "temperature"); v.Exists() {
	// 	out, _ = sjson.SetBytes(out, "temperature", v.Value())
	// }
	// if v := gjson.GetBytes(rawJSON, "top_p"); v.Exists() {
	// 	out, _ = sjson.SetBytes(out, "top_p", v.Value())
	// }
	// if v := gjson.GetBytes(rawJSON, "top_k"); v.Exists() {
	// 	out, _ = sjson.SetBytes(out, "top_k", v.Value())
	// }

	// Map token limits
	// if v := gjson.GetBytes(rawJSON, "max_tokens"); v.Exists() {
	// 	out, _ = sjson.SetBytes(out, "max_output_tokens", v.Value())
	// }
	// if v := gjson.GetBytes(rawJSON, "max_completion_tokens"); v.Exists() {
	// 	out, _ = sjson.SetBytes(out, "max_output_tokens", v.Value())
	// }

	// Map reasoning effort
	if v := gjson.GetBytes(rawJSON, "reasoning_effort"); v.Exists() {
		out, _ = sjson.SetBytes(out, "reasoning.effort", v.Value())
	} else {
		out, _ = sjson.SetBytes(out, "reasoning.effort", "medium")
	}
	out, _ = sjson.SetBytes(out, "parallel_tool_calls", true)
	out, _ = sjson.SetBytes(out, "reasoning.summary", "auto")
	out, _ = sjson.SetBytes(out, "include", []string{"reasoning.encrypted_content"})

	// Model
	out, _ = sjson.SetBytes(out, "model", modelName)

	// Build tool name shortening map from original tools (if any)
	originalToolNameMap := map[string]string{}
	{
		tools := gjson.GetBytes(rawJSON, "tools")
		if tools.IsArray() && len(tools.Array()) > 0 {
			// Collect original tool names
			var names []string
			arr := tools.Array()
			for i := 0; i < len(arr); i++ {
				t := arr[i]
				if t.Get("type").String() == "function" {
					fn := t.Get("function")
					if fn.Exists() {
						if v := fn.Get("name"); v.Exists() {
							names = append(names, v.String())
						}
					}
				}
			}
			if len(names) > 0 {
				originalToolNameMap = buildShortNameMap(names)
			}
		}
	}

	// Extract system instructions from first system message (string or text object)
	messages := gjson.GetBytes(rawJSON, "messages")
	// if messages.IsArray() {
	// 	arr := messages.Array()
	// 	for i := 0; i < len(arr); i++ {
	// 		m := arr[i]
	// 		if m.Get("role").String() == "system" {
	// 			c := m.Get("content")
	// 			if c.Type == gjson.String {
	// 				out, _ = sjson.SetBytes(out, "instructions", c.String())
	// 			} else if c.IsObject() && c.Get("type").String() == "text" {
	// 				out, _ = sjson.SetBytes(out, "instructions", c.Get("text").String())
	// 			}
	// 			break
	// 		}
	// 	}
	// }

	// Map response_format and text settings to Responses API text.format
	rf := gjson.GetBytes(rawJSON, "response_format")
	text := gjson.GetBytes(rawJSON, "text")
	if rf.Exists() {
		// Always create text object when response_format provided
		if !gjson.GetBytes(out, "text").Exists() {
			out, _ = sjson.SetRawBytes(out, "text", []byte(`{}`))
		}

		rft := rf.Get("type").String()
		switch rft {
		case "text":
			out, _ = sjson.SetBytes(out, "text.format.type", "text")
		case "json_schema":
			js := rf.Get("json_schema")
			if js.Exists() {
				out, _ = sjson.SetBytes(out, "text.format.type", "json_schema")
				if v := js.Get("name"); v.Exists() {
					out, _ = sjson.SetBytes(out, "text.format.name", v.Value())
				}
				if v := js.Get("strict"); v.Exists() {
					out, _ = sjson.SetBytes(out, "text.format.strict", v.Value())
				}
				if v := js.Get("schema"); v.Exists() {
					out, _ = sjson.SetRawBytes(out, "text.format.schema", []byte(v.Raw))
				}
			}
		}

		// Map verbosity if provided
		if text.Exists() {
			if v := text.Get("verbosity"); v.Exists() {
				out, _ = sjson.SetBytes(out, "text.verbosity", v.Value())
			}
		}
	} else if text.Exists() {
		// If only text.verbosity present (no response_format), map verbosity
		if v := text.Get("verbosity"); v.Exists() {
			if !gjson.GetBytes(out, "text").Exists() {
				out, _ = sjson.SetRawBytes(out, "text", []byte(`{}`))
			}
			out, _ = sjson.SetBytes(out, "text.verbosity", v.Value())
		}
	}

	// Map tool_choice when present.
	// Chat Completions: "tool_choice" can be a string ("auto"/"none") or an object (e.g. {"type":"function","function":{"name":"..."}}).
	// Responses API: keep built-in tool choices as-is; flatten function choice to {"type":"function","name":"..."}.
	if tc := gjson.GetBytes(rawJSON, "tool_choice"); tc.Exists() {
		switch {
		case tc.Type == gjson.String:
			out, _ = sjson.SetBytes(out, "tool_choice", tc.String())
		case tc.IsObject():
			tcType := tc.Get("type").String()
			if tcType == "function" {
				name := tc.Get("function.name").String()
				if name != "" {
					if short, ok := originalToolNameMap[name]; ok {
						name = short
					} else {
						name = shortenNameIfNeeded(name)
					}
				}
				choice := []byte(`{}`)
				choice, _ = sjson.SetBytes(choice, "type", "function")
				if name != "" {
					choice, _ = sjson.SetBytes(choice, "name", name)
				}
				out, _ = sjson.SetRawBytes(out, "tool_choice", choice)
			} else if tcType != "" {
				// Built-in tool choices (e.g. {"type":"web_search"}) are already Responses-compatible.
				out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(tc.Raw))
			}
		}
	}

	out, _ = sjson.SetBytes(out, "store", false)

	// Large fields are attached last so later small-field mutations do not copy
	// the full transcript again.
	tools := gjson.GetBytes(rawJSON, "tools")
	if toolsJSON, ok := buildCodexTools(tools, originalToolNameMap); ok {
		out, _ = sjson.SetRawBytes(out, "tools", toolsJSON)
	}
	out, _ = sjson.SetRawBytes(out, "input", buildCodexInputItems(messages, originalToolNameMap))

	return out
}

func buildCodexInputItems(messages gjson.Result, originalToolNameMap map[string]string) []byte {
	if !messages.IsArray() {
		return []byte(`[]`)
	}

	var out bytes.Buffer
	out.Grow(len(messages.Raw) + 32)
	out.WriteByte('[')
	firstItem := true
	messages.ForEach(func(_, m gjson.Result) bool {
		role := m.Get("role").String()
		switch role {
		case "tool":
			appendArrayItem(&out, &firstItem)
			writeFunctionCallOutput(&out, m.Get("tool_call_id").String(), m.Get("content").String())
		default:
			content := m.Get("content")
			partCount := countMessageContentParts(role, content)
			if role != "assistant" || partCount > 0 {
				appendArrayItem(&out, &firstItem)
				writeMessageItem(&out, role, content)
			}
			if role == "assistant" {
				toolCalls := m.Get("tool_calls")
				if toolCalls.Exists() && toolCalls.IsArray() {
					toolCalls.ForEach(func(_, tc gjson.Result) bool {
						if tc.Get("type").String() != "function" {
							return true
						}
						appendArrayItem(&out, &firstItem)
						name := tc.Get("function.name").String()
						if short, ok := originalToolNameMap[name]; ok {
							name = short
						} else {
							name = shortenNameIfNeeded(name)
						}
						writeFunctionCall(&out, tc.Get("id").String(), name, tc.Get("function.arguments").String())
						return true
					})
				}
			}
		}
		return true
	})
	out.WriteByte(']')
	return out.Bytes()
}

func countMessageContentParts(role string, content gjson.Result) int {
	if content.Exists() && content.Type == gjson.String && content.Raw != `""` {
		return 1
	}
	if !content.Exists() || !content.IsArray() {
		return 0
	}

	partCount := 0
	content.ForEach(func(_, it gjson.Result) bool {
		switch it.Get("type").String() {
		case "text":
			partCount++
		case "image_url":
			if role == "user" {
				partCount++
			}
		case "file":
			if role == "user" && it.Get("file.file_data").String() != "" {
				partCount++
			}
		}
		return true
	})
	return partCount
}

func writeMessageContentParts(out *bytes.Buffer, role string, content gjson.Result) {
	out.WriteByte('[')
	firstPart := true
	partType := "input_text"
	if role == "assistant" {
		partType = "output_text"
	}

	if content.Exists() && content.Type == gjson.String && content.Raw != `""` {
		appendArrayItem(out, &firstPart)
		writeTextPart(out, partType, content.String())
	} else if content.Exists() && content.IsArray() {
		content.ForEach(func(_, it gjson.Result) bool {
			switch it.Get("type").String() {
			case "text":
				appendArrayItem(out, &firstPart)
				writeTextPart(out, partType, it.Get("text").String())
			case "image_url":
				if role == "user" {
					appendArrayItem(out, &firstPart)
					writeImagePart(out, it.Get("image_url.url"))
				}
			case "file":
				if role == "user" {
					fileData := it.Get("file.file_data").String()
					if fileData != "" {
						appendArrayItem(out, &firstPart)
						writeFilePart(out, fileData, it.Get("file.filename").String())
					}
				}
			}
			return true
		})
	}

	out.WriteByte(']')
}

func buildCodexTools(tools gjson.Result, originalToolNameMap map[string]string) ([]byte, bool) {
	if !tools.IsArray() || len(tools.Array()) == 0 {
		return nil, false
	}

	var out bytes.Buffer
	out.Grow(len(tools.Raw) + 16)
	out.WriteByte('[')
	firstItem := true
	tools.ForEach(func(_, t gjson.Result) bool {
		toolType := t.Get("type").String()
		if toolType != "" && toolType != "function" && t.IsObject() {
			appendArrayItem(&out, &firstItem)
			out.WriteString(t.Raw)
			return true
		}
		if toolType != "function" {
			return true
		}

		appendArrayItem(&out, &firstItem)
		writeToolDefinition(&out, t.Get("function"), originalToolNameMap)
		return true
	})
	out.WriteByte(']')
	return out.Bytes(), true
}

func appendArrayItem(out *bytes.Buffer, first *bool) {
	if !*first {
		out.WriteByte(',')
	}
	*first = false
}

func appendFieldPrefix(out *bytes.Buffer, first *bool, name string) {
	if !*first {
		out.WriteByte(',')
	}
	*first = false
	writeJSONString(out, name)
	out.WriteByte(':')
}

func appendStringField(out *bytes.Buffer, first *bool, name, value string) {
	appendFieldPrefix(out, first, name)
	writeJSONString(out, value)
}

func appendRawField(out *bytes.Buffer, first *bool, name, raw string) {
	appendFieldPrefix(out, first, name)
	out.WriteString(raw)
}

func writeJSONString(out *bytes.Buffer, value string) {
	buf := out.AvailableBuffer()
	buf = strconv.AppendQuote(buf, value)
	out.Write(buf)
}

func writeMessageItem(out *bytes.Buffer, role string, content gjson.Result) {
	if role == "system" {
		role = "developer"
	}
	first := true
	out.WriteByte('{')
	appendStringField(out, &first, "type", "message")
	appendStringField(out, &first, "role", role)
	appendFieldPrefix(out, &first, "content")
	writeMessageContentParts(out, role, content)
	out.WriteByte('}')
}

func writeTextPart(out *bytes.Buffer, partType, text string) {
	first := true
	out.WriteByte('{')
	appendStringField(out, &first, "type", partType)
	appendStringField(out, &first, "text", text)
	out.WriteByte('}')
}

func writeImagePart(out *bytes.Buffer, imageURL gjson.Result) {
	first := true
	out.WriteByte('{')
	appendStringField(out, &first, "type", "input_image")
	if imageURL.Exists() {
		appendStringField(out, &first, "image_url", imageURL.String())
	}
	out.WriteByte('}')
}

func writeFilePart(out *bytes.Buffer, fileData, filename string) {
	first := true
	out.WriteByte('{')
	appendStringField(out, &first, "type", "input_file")
	appendStringField(out, &first, "file_data", fileData)
	if filename != "" {
		appendStringField(out, &first, "filename", filename)
	}
	out.WriteByte('}')
}

func writeFunctionCallOutput(out *bytes.Buffer, callID, output string) {
	first := true
	out.WriteByte('{')
	appendStringField(out, &first, "type", "function_call_output")
	appendStringField(out, &first, "call_id", callID)
	appendStringField(out, &first, "output", output)
	out.WriteByte('}')
}

func writeFunctionCall(out *bytes.Buffer, callID, name, arguments string) {
	first := true
	out.WriteByte('{')
	appendStringField(out, &first, "type", "function_call")
	appendStringField(out, &first, "call_id", callID)
	appendStringField(out, &first, "name", name)
	appendStringField(out, &first, "arguments", arguments)
	out.WriteByte('}')
}

func writeToolDefinition(out *bytes.Buffer, fn gjson.Result, originalToolNameMap map[string]string) {
	first := true
	out.WriteByte('{')
	appendStringField(out, &first, "type", "function")
	if fn.Exists() {
		if v := fn.Get("name"); v.Exists() {
			name := v.String()
			if short, ok := originalToolNameMap[name]; ok {
				name = short
			} else {
				name = shortenNameIfNeeded(name)
			}
			appendStringField(out, &first, "name", name)
		}
		if v := fn.Get("description"); v.Exists() {
			appendRawField(out, &first, "description", v.Raw)
		}
		if v := fn.Get("parameters"); v.Exists() {
			appendRawField(out, &first, "parameters", v.Raw)
		}
		if v := fn.Get("strict"); v.Exists() {
			appendRawField(out, &first, "strict", v.Raw)
		}
	}
	out.WriteByte('}')
}

// shortenNameIfNeeded applies the simple shortening rule for a single name.
// If the name length exceeds 64, it will try to preserve the "mcp__" prefix and last segment.
// Otherwise it truncates to 64 characters.
func shortenNameIfNeeded(name string) string {
	const limit = 64
	if len(name) <= limit {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		// Keep prefix and last segment after '__'
		idx := strings.LastIndex(name, "__")
		if idx > 0 {
			candidate := "mcp__" + name[idx+2:]
			if len(candidate) > limit {
				return candidate[:limit]
			}
			return candidate
		}
	}
	return name[:limit]
}

// buildShortNameMap generates unique short names (<=64) for the given list of names.
// It preserves the "mcp__" prefix with the last segment when possible and ensures uniqueness
// by appending suffixes like "~1", "~2" if needed.
func buildShortNameMap(names []string) map[string]string {
	const limit = 64
	used := map[string]struct{}{}
	m := map[string]string{}

	baseCandidate := func(n string) string {
		if len(n) <= limit {
			return n
		}
		if strings.HasPrefix(n, "mcp__") {
			idx := strings.LastIndex(n, "__")
			if idx > 0 {
				cand := "mcp__" + n[idx+2:]
				if len(cand) > limit {
					cand = cand[:limit]
				}
				return cand
			}
		}
		return n[:limit]
	}

	makeUnique := func(cand string) string {
		if _, ok := used[cand]; !ok {
			return cand
		}
		base := cand
		for i := 1; ; i++ {
			suffix := "_" + strconv.Itoa(i)
			allowed := limit - len(suffix)
			if allowed < 0 {
				allowed = 0
			}
			tmp := base
			if len(tmp) > allowed {
				tmp = tmp[:allowed]
			}
			tmp = tmp + suffix
			if _, ok := used[tmp]; !ok {
				return tmp
			}
		}
	}

	for _, n := range names {
		cand := baseCandidate(n)
		uniq := makeUnique(cand)
		used[uniq] = struct{}{}
		m[n] = uniq
	}
	return m
}
