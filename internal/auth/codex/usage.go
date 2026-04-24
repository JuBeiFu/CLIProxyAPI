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
// on plus / pro / team / enterprise vs free at this moment in time; the same
// field in the id_token JWT is a cached snapshot and can lag by hours.
const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

type WhamUsageInfo struct {
	PlanType        string
	SupportedModels []string
}

// NewCodexAuthWithClient builds a CodexAuth that uses the supplied
// http.Client verbatim (no SetProxy wrapping). Use this when the caller has
// already built an auth-aware transport (e.g. via
// helps.NewProxyAwareHTTPClient) and needs the probe to travel through the
// SAME egress as the auth's real traffic. Egress IP materially changes what
// /wham/usage returns; routing a free-plan auth through its configured
// free-warp pool yields the true per-account plan_type, while hitting
// /wham/usage from a data-center IP returns a user-level aggregate that
// defaults to plus and hides per-account downgrades.
func NewCodexAuthWithClient(client *http.Client) *CodexAuth {
	if client == nil {
		client = &http.Client{}
	}
	return &CodexAuth{httpClient: client}
}

// FetchWhamUsagePlanType calls /wham/usage with the given access_token and
// returns the live plan_type (e.g. "plus", "free"). Empty string + nil
// error means the endpoint succeeded but did not include a plan_type;
// callers should treat that like a fetch failure.
//
// Do NOT send a Chatgpt-Account-Id header. Empirically (probed
// 2026-04-20 against multiple real auths), the bearer-only request returns
// the access_token's own user_id and that user's true plan_type; accounts
// that are actually free report "free" here. Adding Chatgpt-Account-Id
// switches the response to a workspace/account-id scoped view that defaults
// to "plus" for any user who owns a paid workspace anywhere, completely
// masking per-account downgrades. The earlier inverted comment in this file
// described the wrong direction; bearer-only is the live per-account truth.
func (o *CodexAuth) FetchWhamUsagePlanType(ctx context.Context, accessToken string) (string, error) {
	info, err := o.FetchWhamUsageInfo(ctx, accessToken)
	if err != nil {
		return "", err
	}
	return info.PlanType, nil
}

func (o *CodexAuth) FetchWhamUsageInfo(ctx context.Context, accessToken string) (WhamUsageInfo, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return WhamUsageInfo{}, errors.New("codex: FetchWhamUsageInfo: empty access token")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	if err != nil {
		return WhamUsageInfo{}, fmt.Errorf("codex: build /wham/usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal")

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return WhamUsageInfo{}, fmt.Errorf("codex: /wham/usage request failed: %w", err)
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
		return WhamUsageInfo{}, fmt.Errorf("codex: /wham/usage status %d: %s", resp.StatusCode, snippet)
	}
	if readErr != nil {
		return WhamUsageInfo{}, fmt.Errorf("codex: read /wham/usage body: %w", readErr)
	}

	return WhamUsageInfo{
		PlanType:        strings.TrimSpace(gjson.GetBytes(body, "plan_type").String()),
		SupportedModels: extractWhamUsageSupportedModels(body),
	}, nil
}

func extractWhamUsageSupportedModels(body []byte) []string {
	paths := []string{
		"models",
		"available_models",
		"model_ids",
		"entitlements.models",
		"entitlements.available_models",
		"entitlements.model_ids",
		"account.models",
		"account.available_models",
	}
	seen := make(map[string]struct{})
	out := make([]string, 0)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	for _, path := range paths {
		result := gjson.GetBytes(body, path)
		if !result.Exists() || !result.IsArray() {
			continue
		}
		result.ForEach(func(_, value gjson.Result) bool {
			switch value.Type {
			case gjson.String:
				add(value.String())
			case gjson.JSON:
				for _, key := range []string{"id", "name", "model", "model_id"} {
					if got := strings.TrimSpace(value.Get(key).String()); got != "" {
						add(got)
						break
					}
				}
			}
			return true
		})
	}
	return out
}
