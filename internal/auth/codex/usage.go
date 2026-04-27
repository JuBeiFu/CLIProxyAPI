package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	FiveHourQuota   *WhamQuotaWindow
}

type WhamQuotaWindow struct {
	Limit          float64
	Remaining      float64
	Used           float64
	RemainingRatio float64
	ResetAt        time.Time
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
		FiveHourQuota:   extractWhamUsageFiveHourQuota(body),
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

func extractWhamUsageFiveHourQuota(body []byte) *WhamQuotaWindow {
	return ParseWhamUsageFiveHourQuota(body)
}

func ParseWhamUsageFiveHourQuota(body []byte) *WhamQuotaWindow {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil
	}
	if quota := preferredWhamFiveHourQuota(root); quota != nil {
		return quota
	}
	var best *WhamQuotaWindow
	walkWhamUsageObjects(root, "", func(path string, obj map[string]any) {
		if best != nil {
			return
		}
		if !looksLikeFiveHourWindow(path, obj) {
			return
		}
		if quota := quotaWindowFromObject(obj); quota != nil {
			best = quota
		}
	})
	return best
}

func preferredWhamFiveHourQuota(root any) *WhamQuotaWindow {
	obj, ok := root.(map[string]any)
	if !ok {
		return nil
	}
	for _, rateLimitKey := range []string{"rate_limit", "rateLimit"} {
		rateLimit, ok := childWhamObject(obj, rateLimitKey)
		if !ok {
			continue
		}
		for _, windowKey := range []string{"primary_window", "primaryWindow"} {
			window, ok := childWhamObject(rateLimit, windowKey)
			if !ok || !looksLikeFiveHourWindow("rate_limit."+windowKey, window) {
				continue
			}
			if quota := quotaWindowFromObject(window); quota != nil {
				return quota
			}
		}
	}
	return nil
}

func childWhamObject(obj map[string]any, key string) (map[string]any, bool) {
	child, ok := obj[key]
	if !ok {
		return nil, false
	}
	typed, ok := child.(map[string]any)
	return typed, ok
}

func walkWhamUsageObjects(value any, path string, visit func(string, map[string]any)) {
	switch typed := value.(type) {
	case map[string]any:
		visit(path, typed)
		for key, child := range typed {
			nextPath := strings.TrimSpace(key)
			if path != "" {
				nextPath = path + "." + nextPath
			}
			walkWhamUsageObjects(child, nextPath, visit)
		}
	case []any:
		for _, child := range typed {
			walkWhamUsageObjects(child, path, visit)
		}
	}
}

func looksLikeFiveHourWindow(path string, obj map[string]any) bool {
	if containsFiveHourMarker(path) {
		return true
	}
	for key, value := range obj {
		if containsFiveHourMarker(key) {
			return true
		}
		if s, ok := value.(string); ok && containsFiveHourMarker(s) {
			return true
		}
		n, ok := numericValue(value)
		if !ok {
			continue
		}
		keyNorm := normalizeWhamKey(key)
		switch {
		case strings.Contains(keyNorm, "windowseconds") || strings.Contains(keyNorm, "periodseconds") || strings.Contains(keyNorm, "durationseconds"):
			if math.Abs(n-18000) < 0.001 {
				return true
			}
		case strings.Contains(keyNorm, "windowminutes") || strings.Contains(keyNorm, "periodminutes") || strings.Contains(keyNorm, "durationminutes"):
			if math.Abs(n-300) < 0.001 {
				return true
			}
		case strings.Contains(keyNorm, "windowhours") || strings.Contains(keyNorm, "periodhours") || strings.Contains(keyNorm, "durationhours"):
			if math.Abs(n-5) < 0.001 {
				return true
			}
		}
	}
	return false
}

func containsFiveHourMarker(value string) bool {
	norm := normalizeWhamKey(value)
	return strings.Contains(norm, "5h") ||
		strings.Contains(norm, "5hour") ||
		strings.Contains(norm, "fivehour")
}

func quotaWindowFromObject(obj map[string]any) *WhamQuotaWindow {
	numbers := make(map[string]float64)
	stringsByPath := make(map[string]string)
	collectWhamFields(obj, "", numbers, stringsByPath)

	ratio, ratioKey, hasRatio := firstWhamNumberWithKey(numbers,
		"remaining_ratio",
		"remainingRatio",
		"remaining_percent",
		"remainingPercent",
		"remaining_percentage",
		"percent_remaining",
		"ratio_remaining",
	)
	if hasRatio {
		ratio = normaliseWhamRatio(ratio, ratioKey)
	}

	limit, hasLimit := firstWhamNumber(numbers,
		"limit",
		"max",
		"maximum",
		"capacity",
		"total_limit",
		"message_limit",
		"request_limit",
	)
	remaining, hasRemaining := firstWhamNumber(numbers,
		"remaining",
		"available",
		"balance",
		"remaining_quota",
		"remaining_messages",
		"requests_remaining",
		"remaining_count",
	)
	used, hasUsed := firstWhamNumber(numbers,
		"used",
		"usage",
		"consumed",
		"current",
		"count",
		"used_count",
		"messages_used",
	)
	usedPercent, hasUsedPercent := firstWhamNumber(numbers,
		"used_percent",
		"usedPercent",
		"usage_percent",
		"usagePercent",
		"used_percentage",
		"usage_percentage",
		"percent_used",
	)

	if !hasRatio {
		switch {
		case hasLimit && limit > 0 && hasRemaining:
			ratio = remaining / limit
			hasRatio = true
		case hasLimit && limit > 0 && hasUsed:
			remaining = limit - used
			if remaining < 0 {
				remaining = 0
			}
			hasRemaining = true
			ratio = remaining / limit
			hasRatio = true
		case hasUsedPercent:
			usedRatio := normaliseWhamPercent(usedPercent)
			ratio = 1 - usedRatio
			hasRatio = true
		}
	}
	if !hasRatio || math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		return nil
	}
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}

	return &WhamQuotaWindow{
		Limit:          valueOrZero(limit, hasLimit),
		Remaining:      valueOrZero(remaining, hasRemaining),
		Used:           valueOrZero(used, hasUsed),
		RemainingRatio: ratio,
		ResetAt:        whamResetTime(numbers, stringsByPath, time.Now().UTC()),
	}
}

func normaliseWhamRatio(value float64, key string) float64 {
	if isWhamPercentKey(key) {
		return normaliseWhamPercent(value)
	}
	if value > 1 && value <= 100 {
		return value / 100
	}
	return value
}

func normaliseWhamPercent(value float64) float64 {
	if value >= 0 && value <= 100 {
		return value / 100
	}
	return value
}

func isWhamPercentKey(key string) bool {
	norm := normalizeWhamKey(key)
	return strings.Contains(norm, "percent") || strings.Contains(norm, "percentage")
}

func collectWhamFields(value any, path string, numbers map[string]float64, stringsByPath map[string]string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			nextPath := strings.TrimSpace(key)
			if path != "" {
				nextPath = path + "." + nextPath
			}
			collectWhamFields(child, nextPath, numbers, stringsByPath)
		}
	case []any:
		for _, child := range typed {
			collectWhamFields(child, path, numbers, stringsByPath)
		}
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" || path == "" {
			return
		}
		stringsByPath[path] = trimmed
		if parsed, err := strconv.ParseFloat(trimmed, 64); err == nil {
			numbers[path] = parsed
		}
	default:
		if n, ok := numericValue(typed); ok && path != "" {
			numbers[path] = n
		}
	}
}

func firstWhamNumber(fields map[string]float64, names ...string) (float64, bool) {
	value, _, ok := firstWhamNumberWithKey(fields, names...)
	return value, ok
}

func firstWhamNumberWithKey(fields map[string]float64, names ...string) (float64, string, bool) {
	for _, name := range names {
		want := normalizeWhamKey(name)
		for key, value := range fields {
			got := normalizeWhamKey(key)
			if got == want || strings.HasSuffix(got, want) {
				return value, key, true
			}
		}
	}
	return 0, "", false
}

func whamResetTime(numbers map[string]float64, stringsByPath map[string]string, now time.Time) time.Time {
	if raw, ok := firstWhamNumber(numbers,
		"resets_at",
		"reset_at",
		"resetsAt",
		"resetAt",
		"next_reset_at",
		"nextResetAt",
	); ok {
		return normaliseWhamUnix(raw)
	}
	for key, value := range stringsByPath {
		got := normalizeWhamKey(key)
		if got == "resetsat" || got == "resetat" || got == "nextresetat" || strings.HasSuffix(got, "resetsat") || strings.HasSuffix(got, "resetat") || strings.HasSuffix(got, "nextresetat") {
			if ts, ok := parseWhamTime(value); ok {
				return ts
			}
		}
	}
	if raw, ok := firstWhamNumber(numbers,
		"resets_in_seconds",
		"reset_in_seconds",
		"resets_in",
		"reset_in",
		"reset_after_seconds",
	); ok && raw > 0 {
		return now.Add(time.Duration(raw) * time.Second)
	}
	return time.Time{}
}

func parseWhamTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed.UTC(), true
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC(), true
	}
	if raw, err := strconv.ParseFloat(value, 64); err == nil {
		return normaliseWhamUnix(raw), true
	}
	return time.Time{}, false
}

func normaliseWhamUnix(raw float64) time.Time {
	if raw <= 0 {
		return time.Time{}
	}
	if raw > 1_000_000_000_000 {
		return time.UnixMilli(int64(raw)).UTC()
	}
	return time.Unix(int64(raw), 0).UTC()
}

func numericValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case float32:
		return float64(typed), true
	case float64:
		return typed, true
	case json.Number:
		if parsed, err := typed.Float64(); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func valueOrZero(value float64, ok bool) float64 {
	if !ok {
		return 0
	}
	return value
}

func normalizeWhamKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	replacer := strings.NewReplacer("_", "", "-", "", " ", "", ".", "", "/", "", ":", "")
	return replacer.Replace(value)
}
