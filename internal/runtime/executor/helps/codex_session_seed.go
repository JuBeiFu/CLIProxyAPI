package helps

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
)

const CodexContentSessionKeyPrefix = "compat_cs_"

// DeriveCodexContentSessionKey creates a stable fallback key from semantic
// request fields when the client did not provide an explicit Codex session key.
func DeriveCodexContentSessionKey(body []byte, namespace string) string {
	seed := deriveCodexContentSessionSeed(body, namespace)
	if seed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(seed))
	return CodexContentSessionKeyPrefix + hex.EncodeToString(sum[:16])
}

func deriveCodexContentSessionSeed(body []byte, namespace string) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}

	var b strings.Builder
	hasContent := false
	appendPart := func(name, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteByte('|')
		}
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(value)
		if name != "namespace" && name != "model" {
			hasContent = true
		}
	}

	appendPart("namespace", namespace)
	appendPart("model", gjson.GetBytes(body, "model").String())

	for _, path := range []string{"reasoning", "text", "tool_choice", "tools", "functions"} {
		if value := gjson.GetBytes(body, path); value.Exists() {
			appendPart(path, normalizeCodexContentSeedJSON(value.Raw))
		}
	}
	if value := gjson.GetBytes(body, "instructions"); value.Exists() {
		appendPart("instructions", normalizeCodexContentSeedJSON(value.Raw))
	}

	appendCodexMessageSeedParts(&b, &hasContent, body)
	if !hasContent {
		return ""
	}
	return b.String()
}

func appendCodexMessageSeedParts(b *strings.Builder, hasContent *bool, body []byte) {
	appendPart := func(name, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteByte('|')
		}
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(value)
		*hasContent = true
	}

	if messages := gjson.GetBytes(body, "messages"); messages.IsArray() {
		firstUser := ""
		systemParts := make([]string, 0, 2)
		messages.ForEach(func(_, item gjson.Result) bool {
			role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
			content := codexContentSeedMessageContent(item)
			switch role {
			case "system", "developer":
				if content != "" {
					systemParts = append(systemParts, role+":"+content)
				}
			case "user":
				if firstUser == "" {
					firstUser = content
				}
			}
			return true
		})
		if len(systemParts) > 0 {
			appendPart("messages.system", strings.Join(systemParts, "\n"))
		}
		appendPart("messages.first_user", firstUser)
		return
	}

	if input := gjson.GetBytes(body, "input"); input.Exists() {
		switch {
		case input.Type == gjson.String:
			appendPart("input", input.String())
		case input.IsArray():
			firstUser := ""
			systemParts := make([]string, 0, 2)
			input.ForEach(func(_, item gjson.Result) bool {
				role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
				content := codexContentSeedMessageContent(item)
				switch role {
				case "system", "developer":
					if content != "" {
						systemParts = append(systemParts, role+":"+content)
					}
				case "user":
					if firstUser == "" {
						firstUser = content
					}
				default:
					if firstUser == "" && content != "" {
						firstUser = content
					}
				}
				return true
			})
			if len(systemParts) > 0 {
				appendPart("input.system", strings.Join(systemParts, "\n"))
			}
			appendPart("input.first_user", firstUser)
		default:
			appendPart("input", normalizeCodexContentSeedJSON(input.Raw))
		}
	}
}

func codexContentSeedMessageContent(item gjson.Result) string {
	content := item.Get("content")
	if content.Exists() {
		switch {
		case content.Type == gjson.String:
			return content.String()
		case content.IsArray():
			parts := make([]string, 0, 2)
			content.ForEach(func(_, part gjson.Result) bool {
				if text := strings.TrimSpace(part.Get("text").String()); text != "" {
					parts = append(parts, text)
					return true
				}
				if text := strings.TrimSpace(part.Get("input_text").String()); text != "" {
					parts = append(parts, text)
					return true
				}
				if text := strings.TrimSpace(part.Get("output_text").String()); text != "" {
					parts = append(parts, text)
					return true
				}
				if raw := strings.TrimSpace(part.Raw); raw != "" {
					parts = append(parts, normalizeCodexContentSeedJSON(raw))
				}
				return true
			})
			return strings.Join(parts, "\n")
		default:
			return normalizeCodexContentSeedJSON(content.Raw)
		}
	}
	if text := strings.TrimSpace(item.Get("text").String()); text != "" {
		return text
	}
	return normalizeCodexContentSeedJSON(item.Raw)
}

func normalizeCodexContentSeedJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return raw
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return string(normalized)
}
