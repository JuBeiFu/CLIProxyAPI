// Package geminisig provides minimal, local validation of Gemini thought
// signatures so native Gemini request translators can replay a client's real
// thoughtSignature instead of always substituting the bypass sentinel.
//
// The validator only inspects the transport-level protobuf envelope; it does not
// decrypt or prove provider provenance. The safety contract is one-directional:
// a signature is preserved ONLY when it decodes to a known native Gemini envelope
// (Gemini 2.5 field-1 / Gemini 3.x field-2 protobuf shapes). Anything else —
// missing, ASCII-UUID, cross-provider, or an unrecognized envelope — degrades to
// the documented "skip_thought_signature_validator" bypass sentinel, i.e. the
// previous always-sentinel behavior. This guarantees no replay-rejection
// regression: the worst case for an unrecognized-but-real signature is the
// sentinel, never a malformed value sent upstream.
//
// Ported from upstream internal/signature/gemini_validation.go (that package does
// not exist in this fork and lives under a different module major version).
package geminisig

import (
	"encoding/base64"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
)

const (
	// SkipThoughtSignatureValidator is Gemini's documented bypass sentinel for
	// synthetic / migrated function-call history.
	SkipThoughtSignatureValidator = "skip_thought_signature_validator"
	// contextEngineeringBypass is Gemini's second documented bypass sentinel.
	contextEngineeringBypass = "context_engineering_is_the_way_to_go"

	maxThoughtSignatureLen = 32 * 1024 * 1024
)

// IsBypassSentinel reports whether rawSignature is one of Gemini's documented
// bypass sentinels.
func IsBypassSentinel(rawSignature string) bool {
	switch strings.TrimSpace(rawSignature) {
	case SkipThoughtSignatureValidator, contextEngineeringBypass:
		return true
	default:
		return false
	}
}

// IsValidNativeThoughtSignature reports whether rawSignature decodes to a known
// native Gemini thought-signature protobuf envelope. Bypass sentinels are not
// considered "native" (they are not provider-issued signatures).
func IsValidNativeThoughtSignature(rawSignature string) bool {
	sig := strings.TrimSpace(rawSignature)
	if sig == "" || len(sig) > maxThoughtSignatureLen || IsBypassSentinel(sig) {
		return false
	}
	decoded, ok := decodeThoughtSignature(sig)
	if !ok || len(decoded) == 0 {
		return false
	}
	return isKnownGeminiEnvelope(decoded)
}

// ReplaySignatureOrBypass returns a Gemini-replayable thoughtSignature: the
// documented bypass sentinels are preserved as-is, a valid native Gemini
// signature is preserved, and everything else degrades to the skip sentinel.
func ReplaySignatureOrBypass(rawSignature string) string {
	sig := strings.TrimSpace(rawSignature)
	if IsBypassSentinel(sig) {
		return sig
	}
	if IsValidNativeThoughtSignature(sig) {
		return sig
	}
	return SkipThoughtSignatureValidator
}

func decodeThoughtSignature(sig string) ([]byte, bool) {
	if len(sig) > maxThoughtSignatureLen {
		return nil, false
	}
	if decoded, err := base64.StdEncoding.DecodeString(sig); err == nil {
		return decoded, true
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(sig); err == nil {
		return decoded, true
	}
	return nil, false
}

// isKnownGeminiEnvelope reports whether decoded matches a known native Gemini
// thought-signature protobuf envelope. ASCII-UUID payloads are deliberately NOT
// treated as known so they fall back to the bypass sentinel.
func isKnownGeminiEnvelope(decoded []byte) bool {
	if len(decoded) == 0 || isASCIIUUIDBytes(decoded) {
		return false
	}
	return isGeminiField1Envelope(decoded) || isGeminiField2Envelope(decoded)
}

// isGeminiField1Envelope matches the Gemini 2.5 shape: one or more repeated
// {field 1, LEN, value} records whose values are provider-opaque payloads.
func isGeminiField1Envelope(decoded []byte) bool {
	offset := 0
	records := 0
	for offset < len(decoded) {
		num, typ, n := protowire.ConsumeTag(decoded[offset:])
		if n < 0 || num != 1 || typ != protowire.BytesType {
			return false
		}
		offset += n
		value, vn := protowire.ConsumeBytes(decoded[offset:])
		if vn < 0 || !isLikelyOpaquePayload(value) {
			return false
		}
		records++
		offset += vn
	}
	return offset == len(decoded) && records > 0
}

// isGeminiField2Envelope matches the Gemini 3.x shape: {field 2, LEN, {field 1,
// LEN, value}} with a single provider-opaque payload.
func isGeminiField2Envelope(decoded []byte) bool {
	value, ok := consumeField2Field1Value(decoded)
	return ok && isLikelyOpaquePayload(value)
}

func consumeField2Field1Value(decoded []byte) ([]byte, bool) {
	num, typ, n := protowire.ConsumeTag(decoded)
	if n < 0 || num != 2 || typ != protowire.BytesType {
		return nil, false
	}
	offset := n
	container, cn := protowire.ConsumeBytes(decoded[offset:])
	if cn < 0 {
		return nil, false
	}
	offset += cn
	if offset != len(decoded) {
		return nil, false
	}

	num, typ, n = protowire.ConsumeTag(container)
	if n < 0 || num != 1 || typ != protowire.BytesType {
		return nil, false
	}
	containerOffset := n
	value, vn := protowire.ConsumeBytes(container[containerOffset:])
	if vn < 0 {
		return nil, false
	}
	containerOffset += vn
	if containerOffset != len(container) {
		return nil, false
	}
	return value, true
}

// isLikelyOpaquePayload mirrors upstream: observed Gemini 2.5/3.x envelopes wrap
// provider-opaque payloads that start with an internal version byte 0x01.
func isLikelyOpaquePayload(value []byte) bool {
	return len(value) > 0 && value[0] == 0x01
}

func isASCIIUUIDBytes(decoded []byte) bool {
	if len(decoded) != 36 {
		return false
	}
	for i, b := range decoded {
		switch i {
		case 8, 13, 18, 23:
			if b != '-' {
				return false
			}
		default:
			if !((b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')) {
				return false
			}
		}
	}
	return true
}
