package gemini

import (
	"encoding/base64"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/geminisig"
	"github.com/tidwall/gjson"
)

func validGeminiCLISig() string {
	return base64.StdEncoding.EncodeToString([]byte{0x0A, 0x03, 0x01, 0x02, 0x03})
}

func TestConvertGeminiRequestToGeminiCLI_PreservesValidThoughtSignature(t *testing.T) {
	sig := validGeminiCLISig()
	input := []byte(`{"contents":[{"role":"model","parts":[{"text":"x","thoughtSignature":"` + sig + `"}]}]}`)
	out := ConvertGeminiRequestToGeminiCLI("m", input, false)
	if got := gjson.GetBytes(out, "request.contents.0.parts.0.thoughtSignature").String(); got != sig {
		t.Fatalf("valid native thoughtSignature should be preserved, got %q want %q; out=%s", got, sig, out)
	}
}

func TestConvertGeminiRequestToGeminiCLI_SentinelForSyntheticFunctionCall(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"Bash","args":{}}}]}]}`)
	out := ConvertGeminiRequestToGeminiCLI("m", input, false)
	if got := gjson.GetBytes(out, "request.contents.0.parts.0.thoughtSignature").String(); got != geminisig.SkipThoughtSignatureValidator {
		t.Fatalf("synthetic functionCall should get the bypass sentinel, got %q; out=%s", got, out)
	}
}

func TestConvertGeminiRequestToGeminiCLI_DropsFunctionResponseThoughtSignature(t *testing.T) {
	// Use a valid functionCall -> functionResponse pair so the user turn is not
	// filtered out before the signature pass runs.
	input := []byte(`{"contents":[` +
		`{"role":"model","parts":[{"functionCall":{"name":"Bash","args":{}}}]},` +
		`{"role":"user","parts":[{"functionResponse":{"name":"Bash","response":{"output":"ok"}},"thoughtSignature":"` + validGeminiCLISig() + `"}]}` +
		`]}`)
	out := ConvertGeminiRequestToGeminiCLI("m", input, false)
	if gjson.GetBytes(out, "request.contents.1.parts.0.thoughtSignature").Exists() {
		t.Fatalf("user-turn functionResponse must not carry thoughtSignature; out=%s", out)
	}
}
