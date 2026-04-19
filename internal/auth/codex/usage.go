package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// codexUsageURL is the endpoint ChatGPT exposes for the codex CLI to read the
// current subscription state and rate-limit windows. The `plan_type` field in
// the response is the authoritative source for whether the account is actually
// on plus / pro / team / enterprise vs free at this moment in time — the same
// field in the id_token JWT is a cached snapshot and can lag by hours.
const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

// FetchWhamUsagePlanType calls /wham/usage with the given access_token and
// returns the live plan_type (e.g. "plus", "free", …). Empty string + nil
// error means the endpoint succeeded but did not include a plan_type; callers
// should treat that like a fetch failure and not overwrite any stored
// plan_type.
func (o *CodexAuth) FetchWhamUsagePlanType(ctx context.Context, accessToken string) (string, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return "", errors.New("codex: FetchWhamUsagePlanType: empty access token")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	if err != nil {
		return "", fmt.Errorf("codex: build /wham/usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex_cli_rs/0.48.0 (macOS 15.2; arm64) (libc unknown)")

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("codex: /wham/usage request failed: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		snippet := ""
		if readErr == nil && len(body) > 0 {
			snippet = string(body)
			if len(snippet) > 200 {
				snippet = snippet[:200]
			}
		}
		return "", fmt.Errorf("codex: /wham/usage status %d: %s", resp.StatusCode, snippet)
	}
	if readErr != nil {
		return "", fmt.Errorf("codex: read /wham/usage body: %w", readErr)
	}

	return strings.TrimSpace(gjson.GetBytes(body, "plan_type").String()), nil
}
