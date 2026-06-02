package geminisig

import (
	"encoding/base64"
	"testing"
)

// b64 encodes raw bytes the way a real Gemini thoughtSignature is wire-encoded
// (standard base64), so the validator's decode path is exercised.
func b64(raw []byte) string { return base64.StdEncoding.EncodeToString(raw) }

// field-1 envelope: protobuf {field 1, LEN, value=<opaque payload starting 0x01>}
// tag = (1<<3)|2 = 0x0A.
func validField1Sig() string { return b64([]byte{0x0A, 0x03, 0x01, 0x02, 0x03}) }

// field-2 envelope: {field 2, LEN, container={field 1, LEN, value=<0x01...>}}
// outer tag = (2<<3)|2 = 0x12, inner tag = 0x0A.
func validField2Sig() string { return b64([]byte{0x12, 0x05, 0x0A, 0x03, 0x01, 0x02, 0x03}) }

func TestIsValidNativeThoughtSignature(t *testing.T) {
	cases := []struct {
		name string
		sig  string
		want bool
	}{
		{"field1 envelope", validField1Sig(), true},
		{"field2 envelope", validField2Sig(), true},
		{"ascii uuid", b64([]byte("12345678-1234-1234-1234-1234567890ab")), false},
		{"skip sentinel", SkipThoughtSignatureValidator, false},
		{"context-engineering sentinel", contextEngineeringBypass, false},
		{"empty", "", false},
		{"not base64", "!!! not base64 !!!", false},
		{"field1 but payload not 0x01", b64([]byte{0x0A, 0x03, 0x02, 0x02, 0x03}), false},
		{"empty decoded", b64([]byte{}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsValidNativeThoughtSignature(tc.sig); got != tc.want {
				t.Fatalf("IsValidNativeThoughtSignature(%q) = %v, want %v", tc.sig, got, tc.want)
			}
		})
	}
}

func TestReplaySignatureOrBypass(t *testing.T) {
	cases := []struct {
		name string
		sig  string
		want string
	}{
		{"valid field1 preserved", validField1Sig(), validField1Sig()},
		{"valid field2 preserved", validField2Sig(), validField2Sig()},
		{"skip sentinel preserved", SkipThoughtSignatureValidator, SkipThoughtSignatureValidator},
		{"context-engineering sentinel preserved", contextEngineeringBypass, contextEngineeringBypass},
		{"empty -> sentinel", "", SkipThoughtSignatureValidator},
		{"uuid -> sentinel", b64([]byte("12345678-1234-1234-1234-1234567890ab")), SkipThoughtSignatureValidator},
		{"garbage -> sentinel", "not-a-real-sig", SkipThoughtSignatureValidator},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReplaySignatureOrBypass(tc.sig); got != tc.want {
				t.Fatalf("ReplaySignatureOrBypass(%q) = %q, want %q", tc.sig, got, tc.want)
			}
		})
	}
}
