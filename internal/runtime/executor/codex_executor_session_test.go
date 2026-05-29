package executor

import (
	"testing"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestJWTExpiryRFC3339(t *testing.T) {
	// {"exp":4102444800} = 2100-01-01
	jwt := "eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjQxMDI0NDQ4MDB9.sig"
	if got := jwtExpiryRFC3339(jwt); got != "2100-01-01T00:00:00Z" {
		t.Fatalf("got %q", got)
	}
	if jwtExpiryRFC3339("not.a.jwt") != "" {
		t.Fatalf("expected empty for bad exp")
	}
}

func TestApplySessionAccessTokenKeepsRefresh(t *testing.T) {
	st := &codexauth.CodexTokenStorage{AccessToken: "OLD", RefreshToken: "RT", IDToken: "ID"}
	auth := &cliproxyauth.Auth{
		Storage:  st,
		Metadata: map[string]any{"access_token": "OLD", "refresh_token": "RT", "id_token": "ID"},
	}
	applySessionAccessToken(auth, auth.Storage, "NEW", "2100-01-01T00:00:00Z", time.Now())
	if auth.Metadata["access_token"] != "NEW" || auth.Metadata["refresh_token"] != "RT" || auth.Metadata["id_token"] != "ID" {
		t.Fatalf("metadata wrong: %+v", auth.Metadata)
	}
	if st.AccessToken != "NEW" || st.RefreshToken != "RT" || st.IDToken != "ID" {
		t.Fatalf("storage wrong: %+v", st)
	}
}
