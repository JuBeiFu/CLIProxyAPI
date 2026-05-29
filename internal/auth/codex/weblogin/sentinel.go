package weblogin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

const sentinelReqURL = "https://sentinel.openai.com/backend-api/sentinel/req"

type sentinelChallenge struct {
	Token       string `json:"token"`
	ProofOfWork struct {
		Required   bool   `json:"required"`
		Seed       string `json:"seed"`
		Difficulty string `json:"difficulty"`
	} `json:"proofofwork"`
	Turnstile struct {
		Required bool   `json:"required"`
		DX       string `json:"dx"`
	} `json:"turnstile"`
}

// fetchSentinelChallenge POSTs to sentinel/req and returns the challenge.
func fetchSentinelChallenge(ctx context.Context, c *Client, flow, requirementsToken string) (*sentinelChallenge, error) {
	body, _ := json.Marshal(map[string]any{"p": requirementsToken, "flow": flow})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, sentinelReqURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://sentinel.openai.com")
	c.applyCommonHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sentinel/req status %d", resp.StatusCode)
	}
	var ch sentinelChallenge
	if err := json.NewDecoder(resp.Body).Decode(&ch); err != nil {
		return nil, err
	}
	return &ch, nil
}

// buildSimpleSentinelToken is the minimal token used by login authorize_continue (p="").
func buildSimpleSentinelToken(challengeToken, turnstile, flow string) string {
	m := map[string]any{"p": "", "t": turnstile, "c": challengeToken, "id": "", "flow": flow}
	b, _ := json.Marshal(m)
	return string(b)
}
