package gemini

import (
	"encoding/base64"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/geminisig"
	"github.com/tidwall/gjson"
)

// validGeminiSig builds a known native Gemini field-1 protobuf envelope
// thoughtSignature (LEN record whose opaque payload starts with 0x01).
func validGeminiSig() string {
	return base64.StdEncoding.EncodeToString([]byte{0x0A, 0x03, 0x01, 0x02, 0x03})
}

func TestConvertGeminiRequestToGemini_PreservesValidThoughtSignature(t *testing.T) {
	sig := validGeminiSig()
	input := []byte(`{"contents":[{"role":"model","parts":[{"text":"x","thoughtSignature":"` + sig + `"}]}]}`)
	out := ConvertGeminiRequestToGemini("m", input, false)
	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != sig {
		t.Fatalf("valid native thoughtSignature should be preserved, got %q want %q; out=%s", got, sig, out)
	}
}

func TestConvertGeminiRequestToGemini_PreservesValidFunctionCallSignature(t *testing.T) {
	sig := validGeminiSig()
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"Bash","args":{}},"thoughtSignature":"` + sig + `"}]}]}`)
	out := ConvertGeminiRequestToGemini("m", input, false)
	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != sig {
		t.Fatalf("valid functionCall thoughtSignature should be preserved, got %q want %q; out=%s", got, sig, out)
	}
}

func TestConvertGeminiRequestToGemini_SentinelForSyntheticFunctionCall(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"Bash","args":{}}}]}]}`)
	out := ConvertGeminiRequestToGemini("m", input, false)
	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != geminisig.SkipThoughtSignatureValidator {
		t.Fatalf("synthetic functionCall (no sig) should get the bypass sentinel, got %q", got)
	}
}

func TestConvertGeminiRequestToGemini_SentinelForInvalidSignature(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"Bash","args":{}},"thoughtSignature":"not-a-valid-sig"}]}]}`)
	out := ConvertGeminiRequestToGemini("m", input, false)
	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != geminisig.SkipThoughtSignatureValidator {
		t.Fatalf("invalid thoughtSignature should be replaced with the bypass sentinel, got %q", got)
	}
}

func TestConvertGeminiRequestToGemini_DropsFunctionResponseThoughtSignature(t *testing.T) {
	input := []byte(`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"Bash","response":{}},"thoughtSignature":"` + validGeminiSig() + `"}]}]}`)
	out := ConvertGeminiRequestToGemini("m", input, false)
	if gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").Exists() {
		t.Fatalf("user-turn functionResponse parts must not carry thoughtSignature; out=%s", out)
	}
}
