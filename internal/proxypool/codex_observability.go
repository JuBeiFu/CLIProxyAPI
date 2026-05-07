package proxypool

import (
	"net/url"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type CodexResolutionTelemetry struct {
	RuntimeEgressMode string
	DirectBindIP      string
	FailoverState     string
	FailoverReason    string
	StickyAuthID      string
	StickyLease       string
}

func CodexResolutionTelemetryFor(auth *coreauth.Auth, resolution Resolution, now time.Time) CodexResolutionTelemetry {
	var telemetry CodexResolutionTelemetry
	if now.IsZero() {
		now = time.Now()
	}

	authID := ""
	if auth != nil {
		authID = strings.TrimSpace(auth.ID)
	}
	snapshot := DefaultCodexFailoverManager().Snapshot(authID, now)

	telemetry.RuntimeEgressMode = codexRuntimeEgressMode(resolution)
	telemetry.DirectBindIP = bindIPFromResolution(resolution, snapshot)
	telemetry.FailoverState = strings.TrimSpace(snapshot.Mode)
	telemetry.FailoverReason = strings.TrimSpace(snapshot.Reason)
	if telemetry.FailoverState == "" {
		switch telemetry.RuntimeEgressMode {
		case "direct-v4":
			telemetry.FailoverState = CodexFailoverModeDirectV4
		case "direct-v6":
			telemetry.FailoverState = CodexFailoverModeDirectV6
		case "proxy-fallback":
			telemetry.FailoverState = CodexFailoverModeProxy
		}
	}

	leaseURL := strings.TrimSpace(snapshot.Lease.URL)
	if leaseURL == "" {
		leaseURL = strings.TrimSpace(resolution.ProxyURL)
	}
	telemetry.StickyLease = leaseURL
	if telemetry.RuntimeEgressMode == "direct-v6" && authID != "" {
		telemetry.StickyAuthID = authID
	}
	return telemetry
}

func codexRuntimeEgressMode(resolution Resolution) string {
	switch strings.TrimSpace(resolution.Source) {
	case "proxy-pool-fallback":
		return "proxy-fallback"
	case "direct-v6-sticky", "ipv6-bind-lease":
		return "direct-v6"
	case "direct", "direct-primary", "bound-direct", "request-route-direct", "assisted-direct-fallback":
		if isBindProxyURL(resolution.ProxyURL) {
			return "direct-v6"
		}
		return "direct-v4"
	default:
		if isBindProxyURL(resolution.ProxyURL) {
			return "direct-v6"
		}
		if strings.TrimSpace(resolution.ProxyURL) == "" {
			return "direct-v4"
		}
		return "assisted"
	}
}

func bindIPFromResolution(resolution Resolution, snapshot CodexFailoverSnapshot) string {
	if ip := bindIPFromProxyURL(resolution.ProxyURL); ip != "" {
		return ip
	}
	return strings.TrimSpace(snapshot.Lease.IP)
}

func isBindProxyURL(raw string) bool {
	return bindIPFromProxyURL(raw) != ""
}

func bindIPFromProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "bind") {
		return ""
	}
	return strings.TrimSpace(parsed.Hostname())
}
