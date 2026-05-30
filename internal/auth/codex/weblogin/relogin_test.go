package weblogin

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestSelectFlow(t *testing.T) {
	withTOTP := &cliproxyauth.Auth{Metadata: map[string]any{"email": "a@x.com", "openai_password": "p", "totp_secret": "S"}}
	cr, flow := extractCreds(withTOTP)
	if flow != flowPasswordTOTP || cr.Email != "a@x.com" {
		t.Fatalf("expected password+totp, got %v", flow)
	}
	emailOnly := &cliproxyauth.Auth{Metadata: map[string]any{"email": "b@x.com", "oauth2_client_id": "c", "oauth2_refresh_token": "r"}}
	_, flow2 := extractCreds(emailOnly)
	if flow2 != flowEmailOTP {
		t.Fatalf("expected email-otp, got %v", flow2)
	}
	none := &cliproxyauth.Auth{Metadata: map[string]any{"email": "c@x.com"}}
	if _, flow3 := extractCreds(none); flow3 != flowNone {
		t.Fatalf("expected none, got %v", flow3)
	}
}
