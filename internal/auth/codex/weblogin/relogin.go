package weblogin

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type flowKind int

const (
	flowNone flowKind = iota
	flowPasswordTOTP
	flowEmailOTP
)

type creds struct {
	Email            string
	Password         string
	TOTPSecret       string
	MailClientID     string
	MailRefreshToken string
}

func metaStr(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func extractCreds(auth *cliproxyauth.Auth) (creds, flowKind) {
	m := auth.Metadata
	cr := creds{
		Email:            metaStr(m, "email"),
		Password:         metaStr(m, "openai_password"),
		TOTPSecret:       metaStr(m, "totp_secret"),
		MailClientID:     metaStr(m, "oauth2_client_id"),
		MailRefreshToken: metaStr(m, "oauth2_refresh_token"),
	}
	switch {
	case cr.Email != "" && cr.Password != "" && cr.TOTPSecret != "":
		return cr, flowPasswordTOTP
	case cr.Email != "" && cr.MailClientID != "" && cr.MailRefreshToken != "":
		return cr, flowEmailOTP
	case cr.Email != "" && cr.Password != "":
		return cr, flowPasswordTOTP // password-only (no MFA enrolled)
	default:
		return cr, flowNone
	}
}

// HasReloginCreds reports whether the auth carries usable re-login credentials.
func HasReloginCreds(auth *cliproxyauth.Auth) bool {
	_, flow := extractCreds(auth)
	return flow != flowNone
}

// SessionLogin re-authenticates and returns the fresh chatgpt session. The caller
// replaces ONLY auth.Metadata["access_token"] with sess.AccessToken and leaves
// refresh_token/id_token intact (placeholders). NO Codex PKCE.
// Returns ErrAccountBanned (terminal) or ErrLoginTransient (retry).
func SessionLogin(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) (*SessionResult, error) {
	cr, flow := extractCreds(auth)
	if flow == flowNone {
		return nil, fmt.Errorf("%w: no re-login credentials in auth metadata", ErrLoginTransient)
	}
	c, err := NewClient(cfg, auth, randHex(16)) // device_id from crypto/rand (pkce.go)
	if err != nil {
		return nil, ErrLoginTransient
	}
	switch flow {
	case flowPasswordTOTP:
		return c.LoginPasswordTOTP(ctx, cr.Email, cr.Password, cr.TOTPSecret)
	case flowEmailOTP:
		return c.LoginEmailOTP(ctx, cr.Email, cr.MailClientID, cr.MailRefreshToken)
	}
	return nil, fmt.Errorf("%w: unsupported flow", ErrLoginTransient)
}
