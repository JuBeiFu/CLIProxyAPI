package weblogin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
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

// fnv1a32 implements the OpenAI sentinel FNV-1a 32-bit hash with finalisation avalanche.
// Go uint32 wraps automatically; no explicit masking needed.
func fnv1a32(s string) string {
	var h uint32 = 2166136261
	for _, ch := range s {
		h ^= uint32(ch)
		h *= 16777619
	}
	h ^= h >> 16
	h *= 2246822507
	h ^= h >> 13
	h *= 3266489909
	h ^= h >> 16
	return fmt.Sprintf("%08x", h)
}

// b64Config JSON-marshals cfg with compact separators (Go default) and base64-encodes it.
func b64Config(cfg []any) string {
	raw, _ := json.Marshal(cfg)
	return base64.StdEncoding.EncodeToString(raw)
}

// buildConfig returns the 25-slot sentinel PoW config array matching opai's snapshot.
// Slot 3 (attempt counter) and slot 9 (elapsed ms) are overwritten per-iteration by solvePoW.
func buildConfig(ua, sdkURL, sid string) []any {
	return []any{
		2073,                               // 0 screen sum
		"Fri Jan 01 2026 00:00:00 GMT+0000", // 1 date string (overwritten once before loop)
		4294967296,                         // 2
		0,                                  // 3 overwritten with i
		ua,                                 // 4
		sdkURL,                             // 5
		"prod-xxxx",                        // 6 data-build
		"en-US",                            // 7
		"en-US,en;q=0.9",                   // 8
		0,                                  // 9 overwritten with elapsed ms
		"storage−[object StorageManager]", // 10 U+2212 MINUS SIGN
		"location",                         // 11
		"addEventListener",                 // 12
		1234.5,                             // 13 perf.now
		sid,                                // 14
		"",                                 // 15
		8,                                  // 16 hardwareConcurrency
		1.7e12,                             // 17 timeOrigin
		0, 0, 0, 0, 0, 0, 0,               // 18-24
	}
}

const powMaxAttempts = 500000

// solvePoW runs the FNV-1a PoW loop and returns "gAAAAAB<b64>~S" on success,
// or "gAAAAAC<b64("e")>" on exhaustion.
func solvePoW(seed, difficulty, ua, sdkURL, sid string) string {
	cfg := buildConfig(ua, sdkURL, sid)
	cfg[1] = time.Now().UTC().Format("Mon Jan 02 2006 15:04:05 GMT-0700")
	diffLen := len(difficulty)
	start := time.Now()
	for i := 0; i < powMaxAttempts; i++ {
		cfg[3] = i
		cfg[9] = int(time.Since(start).Milliseconds())
		data := b64Config(cfg)
		if fnv1a32(seed+data)[:diffLen] <= difficulty {
			return "gAAAAAB" + data + "~S"
		}
	}
	return "gAAAAAC" + base64.StdEncoding.EncodeToString([]byte(`"e"`))
}

// BuildSentinelToken fetches a challenge for `flow`, solves PoW if required, and
// returns the full openai-sentinel-token header value.
func BuildSentinelToken(ctx context.Context, c *Client, flow string) (string, error) {
	ch, err := fetchSentinelChallenge(ctx, c, flow, "")
	if err != nil {
		return "", err
	}
	if !ch.ProofOfWork.Required {
		return buildSimpleSentinelToken(ch.Token, ch.Turnstile.DX, flow), nil
	}
	p := solvePoW(ch.ProofOfWork.Seed, ch.ProofOfWork.Difficulty, c.userAgent, c.sdkJSURL, c.sid)
	m := map[string]any{"p": p, "t": ch.Turnstile.DX, "c": ch.Token, "id": c.deviceID, "flow": flow}
	b, _ := json.Marshal(m)
	return string(b), nil
}
