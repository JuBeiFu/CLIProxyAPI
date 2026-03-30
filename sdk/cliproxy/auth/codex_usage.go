package auth

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	internalcodex "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
)

const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

func hydrateCodexAccountID(auth *Auth) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return
	}
	if accountID := strings.TrimSpace(internalcodex.CredentialAccountID(auth.Metadata)); accountID != "" {
		auth.Metadata["account_id"] = accountID
	}
}

func codexUsageRefreshSchedule(auth *Auth) (time.Time, time.Time) {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return time.Time{}, time.Time{}
	}
	if auth.Disabled && !quotaProbeAutoDisabled(auth) {
		return time.Time{}, time.Time{}
	}

	var lastFetch time.Time
	var explicitNext time.Time
	if auth.Metadata != nil {
		lastFetch, _ = lookupMetadataTime(auth.Metadata, metadataCodexUsageLastKey)
		explicitNext, _ = lookupMetadataTime(auth.Metadata, metadataCodexUsageAfterKey)
	}

	dueFromUse := time.Time{}
	if !quotaProbeAutoDisabled(auth) {
		if lastUsed, ok := authLastUsedAt(auth); ok && !lastUsed.IsZero() {
			desired := lastUsed.Add(codexUsageRefreshDelay)
			if lastFetch.IsZero() || lastFetch.Before(desired) {
				dueFromUse = desired
			}
		}
	}

	if !explicitNext.IsZero() && !lastFetch.IsZero() && !lastFetch.Before(explicitNext) {
		explicitNext = time.Time{}
	}

	due := earliestNonZeroTime(dueFromUse, explicitNext)
	if !due.IsZero() && !lastFetch.IsZero() {
		minDue := lastFetch.Add(codexUsageRefreshDelay)
		if due.Before(minDue) {
			due = minDue
		}
	}

	return due, lastFetch
}

func earliestNonZeroTime(values ...time.Time) time.Time {
	best := time.Time{}
	for _, value := range values {
		if value.IsZero() {
			continue
		}
		if best.IsZero() || value.Before(best) {
			best = value
		}
	}
	return best
}

func parseCodexUsagePayload(data []byte) (map[string]any, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func codexUsageRemainingPercentFromPayload(payload map[string]any) (float64, bool) {
	windows := codexUsageRelevantWindows(payload)
	if len(windows) == 0 {
		return 0, false
	}

	best := math.MaxFloat64
	found := false
	for _, window := range windows {
		used, ok := codexUsageWindowUsedPercent(window)
		if !ok {
			continue
		}
		remaining := 100 - used
		if remaining < 0 {
			remaining = 0
		}
		if remaining > 100 {
			remaining = 100
		}
		if !found || remaining < best {
			best = remaining
		}
		found = true
	}
	if !found {
		return 0, false
	}
	return best, true
}

func codexUsageNextResetFromPayload(payload map[string]any, now time.Time) (time.Time, bool) {
	windows := codexUsageRelevantWindows(payload)
	if len(windows) == 0 {
		return time.Time{}, false
	}

	next := time.Time{}
	for _, window := range windows {
		resetAt, ok := codexUsageWindowResetAt(window, now)
		if !ok || resetAt.IsZero() {
			continue
		}
		if next.IsZero() || resetAt.Before(next) {
			next = resetAt
		}
	}
	if next.IsZero() {
		return time.Time{}, false
	}
	return next, true
}

func codexUsagePlanTypeFromPayload(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	for _, key := range []string{"plan_type", "planType"} {
		raw, ok := payload[key]
		if !ok {
			continue
		}
		if value, ok := raw.(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func retryAfterFromHTTPResponse(header http.Header, body []byte, now time.Time) *time.Duration {
	if header != nil {
		raw := strings.TrimSpace(header.Get("Retry-After"))
		if raw != "" {
			if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
				retryAfter := time.Duration(seconds) * time.Second
				return &retryAfter
			}
			if until, err := http.ParseTime(raw); err == nil && until.After(now) {
				retryAfter := until.Sub(now)
				return &retryAfter
			}
		}
	}

	payload, err := parseCodexUsagePayload(body)
	if err != nil || payload == nil {
		return nil
	}

	for _, path := range [][]string{{"error", "resets_at"}, {"resets_at"}} {
		if raw, ok := lookupNestedValue(payload, path...); ok {
			if unixSeconds, ok := normalizeInt64Value(raw); ok && unixSeconds > 0 {
				resetAt := time.Unix(unixSeconds, 0)
				if resetAt.After(now) {
					retryAfter := resetAt.Sub(now)
					return &retryAfter
				}
			}
		}
	}
	for _, path := range [][]string{{"error", "resets_in_seconds"}, {"resets_in_seconds"}, {"retry_after_seconds"}, {"retryAfterSeconds"}} {
		if raw, ok := lookupNestedValue(payload, path...); ok {
			if seconds, ok := normalizeFloat64Value(raw); ok && seconds > 0 {
				retryAfter := time.Duration(seconds * float64(time.Second))
				return &retryAfter
			}
		}
	}
	return nil
}

func codexUsageRelevantWindows(payload map[string]any) []map[string]any {
	if payload == nil {
		return nil
	}
	rateLimit := lookupFirstMap(payload, "rate_limit", "rateLimit")
	if rateLimit == nil {
		return nil
	}
	return collectWindowMaps(
		rateLimit["primary_window"],
		rateLimit["primaryWindow"],
		rateLimit["secondary_window"],
		rateLimit["secondaryWindow"],
	)
}

func collectWindowMaps(values ...any) []map[string]any {
	windows := make([]map[string]any, 0, len(values))
	for _, value := range values {
		window := asMap(value)
		if window == nil {
			continue
		}
		windows = append(windows, window)
	}
	return windows
}

func codexUsageWindowUsedPercent(window map[string]any) (float64, bool) {
	for _, key := range []string{"used_percent", "usedPercent"} {
		raw, ok := window[key]
		if !ok {
			continue
		}
		if value, ok := normalizeFloat64Value(raw); ok {
			return value, true
		}
	}
	return 0, false
}

func codexUsageWindowResetAt(window map[string]any, now time.Time) (time.Time, bool) {
	for _, key := range []string{"reset_at", "resetAt"} {
		raw, ok := window[key]
		if !ok {
			continue
		}
		if unixSeconds, ok := normalizeInt64Value(raw); ok && unixSeconds > 0 {
			return time.Unix(unixSeconds, 0), true
		}
	}
	for _, key := range []string{"reset_after_seconds", "resetAfterSeconds"} {
		raw, ok := window[key]
		if !ok {
			continue
		}
		if seconds, ok := normalizeFloat64Value(raw); ok && seconds > 0 {
			return now.Add(time.Duration(seconds * float64(time.Second))), true
		}
	}
	return time.Time{}, false
}

func lookupNestedValue(root map[string]any, keys ...string) (any, bool) {
	current := any(root)
	for _, key := range keys {
		next := asMap(current)
		if next == nil {
			return nil, false
		}
		value, ok := next[key]
		if !ok {
			return nil, false
		}
		current = value
	}
	return current, true
}

func lookupFirstMap(root map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if value, ok := root[key]; ok {
			if mapped := asMap(value); mapped != nil {
				return mapped
			}
		}
	}
	return nil
}

func asMap(value any) map[string]any {
	if value == nil {
		return nil
	}
	mapped, _ := value.(map[string]any)
	return mapped
}

func normalizeFloat64Value(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return 0, false
		}
		return typed, true
	case float32:
		return float64(typed), true
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
	case json.Number:
		if floatValue, err := typed.Float64(); err == nil {
			return floatValue, true
		}
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, false
		}
		if floatValue, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return floatValue, true
		}
	}
	return 0, false
}

func normalizeInt64Value(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		return int64(typed), true
	case json.Number:
		if intValue, err := typed.Int64(); err == nil {
			return intValue, true
		}
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, false
		}
		if intValue, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			return intValue, true
		}
	}
	if floatValue, ok := normalizeFloat64Value(value); ok {
		return int64(floatValue), true
	}
	return 0, false
}
