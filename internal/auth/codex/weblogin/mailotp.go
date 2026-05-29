package weblogin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var otpRe = regexp.MustCompile(`\b(\d{6})\b`)

func extractOTPCode(text string) string {
	m := otpRe.FindStringSubmatch(text)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

// graphAccessToken exchanges an Outlook OAuth2 refresh_token for a Graph access_token.
func graphAccessToken(ctx context.Context, httpClient *http.Client, clientID, refreshToken string) (string, error) {
	form := url.Values{
		"client_id":     {clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {"https://graph.microsoft.com/Mail.Read offline_access"},
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://login.microsoftonline.com/common/oauth2/v2.0/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var t struct {
		AccessToken string `json:"access_token"`
	}
	json.NewDecoder(resp.Body).Decode(&t)
	if t.AccessToken == "" {
		return "", fmt.Errorf("graph token exchange failed (status %d)", resp.StatusCode)
	}
	return t.AccessToken, nil
}

// FetchOpenAIOTP polls the mailbox for the most recent OpenAI verification code.
func FetchOpenAIOTP(ctx context.Context, httpClient *http.Client, clientID, refreshToken string, since time.Time, timeout time.Duration) (string, error) {
	tok, err := graphAccessToken(ctx, httpClient, clientID, refreshToken)
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(timeout)
	q := "https://graph.microsoft.com/v1.0/me/messages?$top=5&$orderby=receivedDateTime%20desc&$select=subject,body,receivedDateTime,from"
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, q, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := httpClient.Do(req)
		if err == nil {
			var list struct {
				Value []struct {
					Subject          string `json:"subject"`
					ReceivedDateTime string `json:"receivedDateTime"`
					Body             struct {
						Content string `json:"content"`
					} `json:"body"`
					From struct {
						EmailAddress struct {
							Address string `json:"address"`
						} `json:"emailAddress"`
					} `json:"from"`
				} `json:"value"`
			}
			json.NewDecoder(resp.Body).Decode(&list)
			resp.Body.Close()
			for _, m := range list.Value {
				rt, _ := time.Parse(time.RFC3339, m.ReceivedDateTime)
				if rt.Before(since) {
					continue
				}
				from := strings.ToLower(m.From.EmailAddress.Address)
				if !strings.Contains(from, "openai") && !strings.Contains(from, "chatgpt") {
					continue
				}
				if code := extractOTPCode(m.Subject + " " + m.Body.Content); code != "" {
					return code, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return "", fmt.Errorf("otp not received within %s", timeout)
}
