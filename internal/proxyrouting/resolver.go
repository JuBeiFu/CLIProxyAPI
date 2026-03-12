package proxyrouting

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/proxystats"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type Selection struct {
	ProxyURL        string
	ProxyProfile    string
	SelectionSource string
	RoutingRule     string
	Provider        string
	PlanType        string
	AuthKind        string
	AuthID          string
	AuthIndex       string
}

func Resolve(cfg *config.Config, auth *coreauth.Auth) Selection {
	selection := Selection{}
	if auth != nil {
		selection.Provider = normalizeProvider(auth.Provider)
		selection.PlanType = normalizeIdentifier(auth.PlanType())
		selection.AuthKind = resolveAuthKind(auth)
		selection.AuthID = strings.TrimSpace(auth.ID)
		selection.AuthIndex = strings.TrimSpace(auth.EnsureIndex())

		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			selection.ProxyURL = proxyURL
			selection.SelectionSource = "auth-proxy-url"
			return selection
		}

		if profile := strings.TrimSpace(auth.ProxyProfile()); profile != "" {
			if proxyURL, profileName := resolveProfile(cfg, profile); proxyURL != "" {
				selection.ProxyURL = proxyURL
				selection.ProxyProfile = profileName
				selection.SelectionSource = "auth-proxy-profile"
				return selection
			}
		}
	}

	if cfg != nil {
		for index, rule := range cfg.ProxyRouting.Rules {
			if !matchesRule(rule, selection) {
				continue
			}
			if proxyURL := strings.TrimSpace(rule.ProxyURL); proxyURL != "" {
				selection.ProxyURL = proxyURL
				selection.SelectionSource = "proxy-routing-rule"
				selection.RoutingRule = resolveRuleName(rule.Name, index)
				return selection
			}
			if profile := strings.TrimSpace(rule.ProxyProfile); profile != "" {
				if proxyURL, profileName := resolveProfile(cfg, profile); proxyURL != "" {
					selection.ProxyURL = proxyURL
					selection.ProxyProfile = profileName
					selection.SelectionSource = "proxy-routing-rule"
					selection.RoutingRule = resolveRuleName(rule.Name, index)
					return selection
				}
			}
		}

		if proxyURL := strings.TrimSpace(cfg.ProxyURL); proxyURL != "" {
			selection.ProxyURL = proxyURL
			selection.SelectionSource = "global-proxy-url"
		}
	}

	return selection
}

func (s Selection) HasProxy() bool {
	return strings.TrimSpace(s.ProxyURL) != ""
}

func (s Selection) StatsMetadata() proxystats.RequestMetadata {
	return proxystats.RequestMetadata{
		ProxyProfile:    s.ProxyProfile,
		SelectionSource: s.SelectionSource,
		RoutingRule:     s.RoutingRule,
		Provider:        s.Provider,
		PlanType:        s.PlanType,
		AuthKind:        s.AuthKind,
		AuthID:          s.AuthID,
		AuthIndex:       s.AuthIndex,
	}
}

func resolveProfile(cfg *config.Config, profile string) (string, string) {
	if cfg == nil {
		return "", ""
	}
	target := normalizeIdentifier(profile)
	for _, item := range cfg.ProxyProfiles {
		if normalizeIdentifier(item.Name) != target {
			continue
		}
		proxyURL := strings.TrimSpace(item.ProxyURL)
		if proxyURL == "" {
			return "", ""
		}
		return proxyURL, strings.TrimSpace(item.Name)
	}
	return "", ""
}

func resolveAuthKind(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if value := strings.TrimSpace(auth.Attributes["auth_kind"]); value != "" {
			return normalizeIdentifier(value)
		}
	}
	kind, _ := auth.AccountInfo()
	return normalizeIdentifier(kind)
}

func matchesRule(rule config.ProxyRoutingRule, selection Selection) bool {
	if rule.Disabled {
		return false
	}
	if len(rule.Providers) > 0 && !containsValue(rule.Providers, selection.Provider, normalizeProvider) {
		return false
	}
	if len(rule.PlanTypes) > 0 && !containsValue(rule.PlanTypes, selection.PlanType, normalizeIdentifier) {
		return false
	}
	if len(rule.AuthKinds) > 0 && !containsValue(rule.AuthKinds, selection.AuthKind, normalizeIdentifier) {
		return false
	}
	return strings.TrimSpace(rule.ProxyURL) != "" || strings.TrimSpace(rule.ProxyProfile) != ""
}

func containsValue(values []string, target string, normalizer func(string) string) bool {
	if target == "" {
		return false
	}
	for _, value := range values {
		if normalizer(value) == target {
			return true
		}
	}
	return false
}

func resolveRuleName(name string, index int) string {
	trimmed := strings.TrimSpace(name)
	if trimmed != "" {
		return trimmed
	}
	return fmt.Sprintf("rule-%d", index+1)
}

func normalizeProvider(value string) string {
	normalized := normalizeIdentifier(value)
	switch normalized {
	case "claude-code", "claudecode":
		return "claude"
	case "codex-cli", "codexcode":
		return "codex"
	default:
		return normalized
	}
}

func normalizeIdentifier(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.ReplaceAll(value, "_", "-")
	value = strings.ReplaceAll(value, " ", "-")
	return value
}
