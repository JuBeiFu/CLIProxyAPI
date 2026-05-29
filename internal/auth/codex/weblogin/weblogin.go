package weblogin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type SessionResult struct {
	AccessToken string
	AccountID   string
	PlanType    string
}

func parseSession(body []byte) (*SessionResult, error) {
	var s struct {
		AccessToken string `json:"accessToken"`
		User        struct {
			ID string `json:"id"`
		} `json:"user"`
		Account struct {
			PlanType string `json:"planType"`
		} `json:"account"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, err
	}
	if s.AccessToken == "" {
		return nil, ErrLoginTransient
	}
	return &SessionResult{AccessToken: s.AccessToken, AccountID: s.User.ID, PlanType: s.Account.PlanType}, nil
}

// LoginPasswordTOTP runs the password (+TOTP) login and returns the chatgpt session.
func (c *Client) LoginPasswordTOTP(ctx context.Context, email, password, totpSecret string) (*SessionResult, error) {
	// 1. prime cookies
	if _, err := c.getJSON(ctx, "https://chatgpt.com/", nil); err != nil {
		return nil, ErrLoginTransient
	}
	// 2. csrf
	var csrf struct {
		CSRFToken string `json:"csrfToken"`
	}
	st, err := c.getJSON(ctx, "https://chatgpt.com/api/auth/csrf", &csrf)
	if err != nil || st == 403 || csrf.CSRFToken == "" {
		return nil, ErrLoginTransient // CF gate or no token
	}
	// 3. signin/openai -> authorize url
	form := url.Values{"csrfToken": {csrf.CSRFToken}, "callbackUrl": {"https://chatgpt.com/"}, "json": {"true"}}
	signinURL := "https://chatgpt.com/api/auth/signin/openai?" +
		url.Values{"login_hint": {email}, "ext-oai-did": {c.deviceID}, "prompt": {"login"}}.Encode()
	var signin struct {
		URL string `json:"url"`
	}
	if _, _, err = c.postForm(ctx, signinURL, form, &signin); err != nil || signin.URL == "" {
		return nil, ErrLoginTransient
	}
	// 4. follow authorize (cookiejar + auto-redirect sets login_session)
	if _, err = c.getJSON(ctx, signin.URL, nil); err != nil {
		return nil, ErrLoginTransient
	}
	// 5. authorize/continue (sentinel: authorize_continue)
	senTok, err := BuildSentinelToken(ctx, c, "authorize_continue")
	if err != nil {
		return nil, ErrLoginTransient
	}
	contBody := map[string]any{"username": map[string]string{"value": email, "kind": "email"}}
	c.postJSON(ctx, "https://auth.openai.com/api/accounts/authorize/continue",
		map[string]string{"openai-sentinel-token": senTok}, contBody, nil) // best-effort; may already be at password
	// 6. password/verify (sentinel: password_verify)
	pwSen, err := BuildSentinelToken(ctx, c, "password_verify")
	if err != nil {
		return nil, ErrLoginTransient
	}
	var pwResp struct {
		Page struct {
			Type    string `json:"type"`
			Payload struct {
				FactorID string `json:"factor_id"`
			} `json:"payload"`
		} `json:"page"`
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	pwStatus, pwBody, err := c.postJSON(ctx, "https://auth.openai.com/api/accounts/password/verify",
		map[string]string{"openai-sentinel-token": pwSen}, map[string]string{"password": password}, &pwResp)
	if err != nil {
		return nil, ErrLoginTransient
	}
	if isAccountLevelReject(pwStatus, pwBody) {
		return nil, ErrAccountBanned
	}
	// 7+8. MFA if challenged
	if pwResp.Page.Type == "mfa_challenge" && pwResp.Page.Payload.FactorID != "" {
		if totpSecret == "" {
			return nil, fmt.Errorf("%w: account requires TOTP but no secret stored", ErrLoginTransient)
		}
		fid := pwResp.Page.Payload.FactorID
		c.postJSON(ctx, "https://auth.openai.com/api/accounts/mfa/issue_challenge", nil,
			map[string]any{"id": fid, "type": "totp", "force_fresh_challenge": false}, nil)
		code, terr := TOTPNow(totpSecret)
		if terr != nil {
			return nil, fmt.Errorf("%w: totp gen: %v", ErrLoginTransient, terr)
		}
		var mfaResp struct {
			ContinueURL string `json:"continue_url"`
		}
		mst, mbody, _ := c.postJSON(ctx, "https://auth.openai.com/api/accounts/mfa/verify", nil,
			map[string]any{"id": fid, "type": "totp", "code": code}, &mfaResp)
		if isAccountLevelReject(mst, mbody) {
			return nil, ErrAccountBanned
		}
		if mfaResp.ContinueURL != "" {
			c.getJSON(ctx, mfaResp.ContinueURL, nil)
		}
	}
	// 11. session
	var raw json.RawMessage
	if _, err = c.getJSON(ctx, "https://chatgpt.com/api/auth/session", &raw); err != nil {
		return nil, ErrLoginTransient
	}
	return parseSession(raw)
}

// isAccountLevelReject detects deactivated/disabled account responses (terminal).
func isAccountLevelReject(status int, body []byte) bool {
	low := strings.ToLower(string(body))
	for _, needle := range []string{"account_deactivated", "deactivated", "account has been disabled", "access_denied"} {
		if strings.Contains(low, needle) {
			return true
		}
	}
	return false
}

// LoginEmailOTP runs the email-OTP login: the password step is replaced by the
// email-verification code fetched from the mailbox via Graph.
func (c *Client) LoginEmailOTP(ctx context.Context, email, mailClientID, mailRefreshToken string) (*SessionResult, error) {
	if _, err := c.getJSON(ctx, "https://chatgpt.com/", nil); err != nil {
		return nil, ErrLoginTransient
	}
	var csrf struct {
		CSRFToken string `json:"csrfToken"`
	}
	if st, err := c.getJSON(ctx, "https://chatgpt.com/api/auth/csrf", &csrf); err != nil || st == 403 || csrf.CSRFToken == "" {
		return nil, ErrLoginTransient
	}
	form := url.Values{"csrfToken": {csrf.CSRFToken}, "callbackUrl": {"https://chatgpt.com/"}, "json": {"true"}}
	signinURL := "https://chatgpt.com/api/auth/signin/openai?" +
		url.Values{"login_hint": {email}, "ext-oai-did": {c.deviceID}, "prompt": {"login"}}.Encode()
	var signin struct {
		URL string `json:"url"`
	}
	if _, _, err := c.postForm(ctx, signinURL, form, &signin); err != nil || signin.URL == "" {
		return nil, ErrLoginTransient
	}
	if _, err := c.getJSON(ctx, signin.URL, nil); err != nil {
		return nil, ErrLoginTransient
	}
	// authorize/continue triggers the email-OTP send
	sentAt := time.Now().Add(-30 * time.Second)
	sen, err := BuildSentinelToken(ctx, c, "authorize_continue")
	if err != nil {
		return nil, ErrLoginTransient
	}
	contBody := map[string]any{"username": map[string]string{"value": email, "kind": "email"}}
	cst, cbody, _ := c.postJSON(ctx, "https://auth.openai.com/api/accounts/authorize/continue",
		map[string]string{"openai-sentinel-token": sen}, contBody, nil)
	if isAccountLevelReject(cst, cbody) {
		return nil, ErrAccountBanned
	}
	// fetch + submit the email code
	code, err := FetchOpenAIOTP(ctx, mailClientID, mailRefreshToken, sentAt, 90*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLoginTransient, err)
	}
	var otpResp struct {
		ContinueURL string `json:"continue_url"`
	}
	ost, obody, _ := c.postJSON(ctx, "https://auth.openai.com/api/accounts/email-otp/validate", nil,
		map[string]string{"email": email, "code": code}, &otpResp)
	if isAccountLevelReject(ost, obody) {
		return nil, ErrAccountBanned
	}
	if otpResp.ContinueURL != "" {
		c.getJSON(ctx, otpResp.ContinueURL, nil)
	}
	var raw json.RawMessage
	if _, err := c.getJSON(ctx, "https://chatgpt.com/api/auth/session", &raw); err != nil {
		return nil, ErrLoginTransient
	}
	return parseSession(raw)
}
