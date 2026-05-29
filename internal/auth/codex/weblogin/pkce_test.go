package weblogin

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestPKCEChallengeMatchesVerifier(t *testing.T) {
	p := NewPKCE()
	sum := sha256.Sum256([]byte(p.Verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if p.Challenge != want {
		t.Fatalf("challenge mismatch")
	}
	if len(p.State) < 16 || len(p.DeviceID) < 16 {
		t.Fatalf("state/device too short")
	}
}
