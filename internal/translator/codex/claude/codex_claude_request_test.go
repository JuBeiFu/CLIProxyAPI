package claude

import (
	"encoding/base64"
	"testing"

	"github.com/tidwall/gjson"
)

// validCodexReasoningSignatureForTest builds a Fernet-shaped GPT/Codex reasoning
// signature: "gAAAA" base64url prefix, decoded payload of exactly 73 bytes
// (1 version + 8 ts + 16 IV + 16 ciphertext + 32 HMAC), version byte 0x80.
func validCodexReasoningSignatureForTest() string {
	raw := make([]byte, 73)
	raw[0] = 0x80
	// Bytes 1..3 stay zero so the base64url encoding keeps the required "gAAAA"
	// prefix; the rest (IV/ciphertext/HMAC region) can be arbitrary.
	for i := 4; i < len(raw); i++ {
		raw[i] = byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// TestConvertClaudeRequestToCodex_ThinkingSignatureRoundTrip verifies that a
// Claude assistant thinking block carrying a valid Codex reasoning signature is
// forwarded back upstream as a reasoning item with encrypted_content, instead of
// being silently dropped. Dropping it produces turn-2 requests with reasoning
// items absent — the behavioral fingerprint OpenAI risk-control flags to ban
// OAuth accounts (and triggers hard 400 "invalid signature in thinking block").
func TestConvertClaudeRequestToCodex_ThinkingSignatureRoundTrip(t *testing.T) {
	sig := validCodexReasoningSignatureForTest()
	input := `{
		"model": "claude-3-opus",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "let me reason", "signature": "` + sig + `"},
				{"type": "text", "text": "the answer"}
			]},
			{"role": "user", "content": "thanks"}
		]
	}`

	result := ConvertClaudeRequestToCodex("test-model", []byte(input), false)
	inputs := gjson.ParseBytes(result).Get("input").Array()

	reasoningIdx, assistantMsgIdx := -1, -1
	for i, item := range inputs {
		switch item.Get("type").String() {
		case "reasoning":
			if reasoningIdx == -1 {
				reasoningIdx = i
				if got := item.Get("encrypted_content").String(); got != sig {
					t.Fatalf("reasoning.encrypted_content = %q, want %q", got, sig)
				}
				if c := item.Get("content"); c.Type != gjson.Null {
					t.Fatalf("reasoning.content should be JSON null, got %q (raw %s)", c.Type, c.Raw)
				}
				if n := len(item.Get("summary").Array()); n != 0 {
					t.Fatalf("reasoning.summary should be empty array, got %s", item.Get("summary").Raw)
				}
			}
		case "message":
			if item.Get("role").String() == "assistant" && assistantMsgIdx == -1 {
				assistantMsgIdx = i
			}
		}
	}

	if reasoningIdx == -1 {
		t.Fatalf("expected a reasoning item carrying encrypted_content, got input: %s", gjson.ParseBytes(result).Get("input").Raw)
	}
	if assistantMsgIdx != -1 && reasoningIdx > assistantMsgIdx {
		t.Fatalf("reasoning item (idx %d) must precede the assistant message (idx %d); input: %s", reasoningIdx, assistantMsgIdx, gjson.ParseBytes(result).Get("input").Raw)
	}
}

// TestConvertClaudeRequestToCodex_ThinkingSignatureInvalidDropped verifies that a
// thinking block whose signature is NOT a valid Fernet-shaped Codex reasoning
// token is dropped rather than forwarded — a fabricated/cross-provider signature
// must never be replayed to upstream (it would be rejected and is itself a tell).
func TestConvertClaudeRequestToCodex_ThinkingSignatureInvalidDropped(t *testing.T) {
	cases := []struct {
		name string
		sig  string
	}{
		{"empty", ""},
		{"not base64url", "this is not a signature!!"},
		{"wrong prefix (claude-like)", "ErABCkYIARgCKkDabc123"},
		{"gAAAA prefix but decoded too short", base64.RawURLEncoding.EncodeToString(append([]byte{0x80}, make([]byte, 39)...))},
		{"cross-provider claude# envelope", "claude#" + validCodexReasoningSignatureForTest()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := `{
				"model": "claude-3-opus",
				"messages": [
					{"role": "assistant", "content": [
						{"type": "thinking", "thinking": "x", "signature": "` + tc.sig + `"},
						{"type": "text", "text": "answer"}
					]}
				]
			}`
			result := ConvertClaudeRequestToCodex("test-model", []byte(input), false)
			for _, item := range gjson.ParseBytes(result).Get("input").Array() {
				if item.Get("type").String() == "reasoning" {
					t.Fatalf("invalid signature %q must not produce a reasoning item; input: %s", tc.sig, gjson.ParseBytes(result).Get("input").Raw)
				}
			}
		})
	}
}

func TestConvertClaudeRequestToCodex_SystemMessageScenarios(t *testing.T) {
	tests := []struct {
		name             string
		inputJSON        string
		wantHasDeveloper bool
		wantTexts        []string
	}{
		{
			name: "No system field",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantHasDeveloper: false,
		},
		{
			name: "Empty string system field",
			inputJSON: `{
				"model": "claude-3-opus",
				"system": "",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantHasDeveloper: false,
		},
		{
			name: "String system field",
			inputJSON: `{
				"model": "claude-3-opus",
				"system": "Be helpful",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantHasDeveloper: true,
			wantTexts:        []string{"Be helpful"},
		},
		{
			name: "Array system field with filtered billing header",
			inputJSON: `{
				"model": "claude-3-opus",
				"system": [
					{"type": "text", "text": "x-anthropic-billing-header: tenant-123"},
					{"type": "text", "text": "Block 1"},
					{"type": "text", "text": "Block 2"}
				],
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantHasDeveloper: true,
			wantTexts:        []string{"Block 1", "Block 2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ConvertClaudeRequestToCodex("test-model", []byte(tt.inputJSON), false)
			resultJSON := gjson.ParseBytes(result)
			inputs := resultJSON.Get("input").Array()

			hasDeveloper := len(inputs) > 0 && inputs[0].Get("role").String() == "developer"
			if hasDeveloper != tt.wantHasDeveloper {
				t.Fatalf("got hasDeveloper = %v, want %v. Output: %s", hasDeveloper, tt.wantHasDeveloper, resultJSON.Get("input").Raw)
			}

			if !tt.wantHasDeveloper {
				return
			}

			content := inputs[0].Get("content").Array()
			if len(content) != len(tt.wantTexts) {
				t.Fatalf("got %d system content items, want %d. Content: %s", len(content), len(tt.wantTexts), inputs[0].Get("content").Raw)
			}

			for i, wantText := range tt.wantTexts {
				if gotType := content[i].Get("type").String(); gotType != "input_text" {
					t.Fatalf("content[%d] type = %q, want %q", i, gotType, "input_text")
				}
				if gotText := content[i].Get("text").String(); gotText != wantText {
					t.Fatalf("content[%d] text = %q, want %q", i, gotText, wantText)
				}
			}
		})
	}
}

func TestConvertClaudeRequestToCodex_ParallelToolCalls(t *testing.T) {
	tests := []struct {
		name                  string
		inputJSON             string
		wantParallelToolCalls bool
	}{
		{
			name: "Default to true when tool_choice.disable_parallel_tool_use is absent",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantParallelToolCalls: true,
		},
		{
			name: "Disable parallel tool calls when client opts out",
			inputJSON: `{
				"model": "claude-3-opus",
				"tool_choice": {"disable_parallel_tool_use": true},
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantParallelToolCalls: false,
		},
		{
			name: "Keep parallel tool calls enabled when client explicitly allows them",
			inputJSON: `{
				"model": "claude-3-opus",
				"tool_choice": {"disable_parallel_tool_use": false},
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantParallelToolCalls: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ConvertClaudeRequestToCodex("test-model", []byte(tt.inputJSON), false)
			resultJSON := gjson.ParseBytes(result)

			if got := resultJSON.Get("parallel_tool_calls").Bool(); got != tt.wantParallelToolCalls {
				t.Fatalf("parallel_tool_calls = %v, want %v. Output: %s", got, tt.wantParallelToolCalls, string(result))
			}
		})
	}
}
