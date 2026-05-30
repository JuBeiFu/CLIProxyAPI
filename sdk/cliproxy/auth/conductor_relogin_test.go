package auth

import "testing"

func TestShouldAttemptRefreshForCodex401WithCreds(t *testing.T) {
	a := &Auth{Provider: "codex", Metadata: map[string]any{"email": "a@x.com", "openai_password": "p", "totp_secret": "s"}}
	err401 := &Error{HTTPStatus: 401, Message: "unauthorized"}
	if !shouldAttemptAccessTokenRefresh(a, err401) {
		t.Fatalf("expected true for codex 401 with creds")
	}
	noCreds := &Auth{Provider: "codex", Metadata: map[string]any{"email": "a@x.com"}}
	if shouldAttemptAccessTokenRefresh(noCreds, err401) {
		t.Fatalf("expected false when no login creds")
	}
}
