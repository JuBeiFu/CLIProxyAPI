//go:build integration

package weblogin

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// TestLiveReLogin drives the real native-Go session login against OpenAI and then
// proves the fresh access_token is usable. Run from an IP/proxy that passes
// Cloudflare (e.g. a CLIProxyAPI deployment host).
//
//	OPAI_TEST_EMAIL=..  OPAI_TEST_PW=..  OPAI_TEST_TOTP=<b32>   (password+TOTP path)
//	OPAI_TEST_EMAIL=..  OPAI_TEST_MAIL_CID=..  OPAI_TEST_MAIL_RT=..  (email-OTP path)
//	OPAI_TEST_PROXY=socks5h://...  (optional per-account proxy)
//	go test -tags integration ./internal/auth/codex/weblogin/ -run TestLiveReLogin -v
func TestLiveReLogin(t *testing.T) {
	email := os.Getenv("OPAI_TEST_EMAIL")
	if email == "" {
		t.Skip("set OPAI_TEST_EMAIL (+ PW/TOTP or MAIL_CID/MAIL_RT)")
	}
	meta := map[string]any{
		"email":                email,
		"openai_password":      os.Getenv("OPAI_TEST_PW"),
		"totp_secret":          os.Getenv("OPAI_TEST_TOTP"),
		"oauth2_client_id":     os.Getenv("OPAI_TEST_MAIL_CID"),
		"oauth2_refresh_token": os.Getenv("OPAI_TEST_MAIL_RT"),
	}
	auth := &cliproxyauth.Auth{Provider: "codex", Metadata: meta}
	if px := os.Getenv("OPAI_TEST_PROXY"); px != "" {
		auth.ProxyURL = px
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	sess, err := SessionLogin(ctx, &config.Config{}, auth)
	if err != nil {
		t.Fatalf("SessionLogin failed: %v", err)
	}
	if sess.AccessToken == "" {
		t.Fatalf("empty access token")
	}
	t.Logf("OK login: access_token len=%d account=%s plan=%q", len(sess.AccessToken), sess.AccountID, sess.PlanType)

	// Usability probe: the fresh session access_token must actually work.
	c, err := NewClient(&config.Config{}, auth, randHex(16))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/backend-api/me", nil)
	req.Header.Set("Authorization", "Bearer "+sess.AccessToken)
	req.Header.Set("Accept", "application/json")
	c.applyCommonHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("usability probe error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("usability probe: /backend-api/me returned %d (fresh token NOT usable)", resp.StatusCode)
	}
	t.Logf("OK usability: /backend-api/me = 200 — fresh access_token works")
}
