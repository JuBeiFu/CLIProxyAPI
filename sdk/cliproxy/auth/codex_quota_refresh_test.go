package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type codexQuotaRefreshExecutor struct {
	updated *Auth
}

func (e *codexQuotaRefreshExecutor) Identifier() string { return "codex" }

func (e *codexQuotaRefreshExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *codexQuotaRefreshExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *codexQuotaRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	if e.updated == nil {
		return auth, nil
	}
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = map[string]any{}
	}
	for key, value := range e.updated.Metadata {
		updated.Metadata[key] = value
	}
	return updated, nil
}

func (e *codexQuotaRefreshExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *codexQuotaRefreshExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestManager_ShouldRefreshCodexAvailableAuthOnHotCadence(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	auth := &Auth{
		ID:              "codex-hot",
		Provider:        "codex",
		Status:          StatusActive,
		LastRefreshedAt: now.Add(-codexHotRefreshInterval),
		Metadata:        map[string]any{"type": "codex", "access_token": "tok"},
	}

	if !mgr.shouldRefresh(auth, now) {
		t.Fatal("expected available codex auth to refresh after hot interval")
	}

	auth.LastRefreshedAt = now.Add(-codexHotRefreshInterval + time.Second)
	if mgr.shouldRefresh(auth, now) {
		t.Fatal("expected available codex auth to wait until hot interval elapses")
	}
}

func TestManager_ShouldRefreshFrequentlyUsedCodexAuthOnFastCadence(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	auth := &Auth{
		ID:              "codex-fast",
		Provider:        "codex",
		Status:          StatusActive,
		LastRefreshedAt: now.Add(-codexFrequentRefreshInterval),
		Metadata:        map[string]any{"type": "codex", "access_token": "tok"},
	}
	for i := 0; i < codexFrequentActivityThreshold; i++ {
		mgr.recordAuthDispatch(auth.ID, now.Add(-time.Duration(i)*10*time.Second))
	}

	if !mgr.shouldRefresh(auth, now) {
		t.Fatal("expected frequently used codex auth to refresh after fast interval")
	}
}

func TestManager_ShouldRefreshCodexQuotaLimitedAuthOnColdCadence(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	auth := &Auth{
		ID:              "codex-cold",
		Provider:        "codex",
		Status:          StatusError,
		Unavailable:     true,
		LastRefreshedAt: now.Add(-codexColdRefreshInterval),
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        codexFiveHourQuotaLowReason,
			NextRecoverAt: now.Add(time.Hour),
		},
		Metadata: map[string]any{"type": "codex", "access_token": "tok"},
	}

	if !mgr.shouldRefresh(auth, now) {
		t.Fatal("expected quota-limited codex auth to refresh after cold interval")
	}

	auth.LastRefreshedAt = now.Add(-codexColdRefreshInterval + time.Second)
	if mgr.shouldRefresh(auth, now) {
		t.Fatal("expected quota-limited codex auth to wait until cold interval elapses")
	}
}

func TestManager_RefreshAuth_RestsCodexAuthWhenFiveHourQuotaBelowThreshold(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(nil, nil, nil)
	resetAt := time.Now().Add(90 * time.Minute).UTC()
	exec := &codexQuotaRefreshExecutor{updated: &Auth{Metadata: map[string]any{
		MetadataCodexFiveHourQuotaRemainingRatioKey: 0.19,
		MetadataCodexFiveHourQuotaResetAtKey:        resetAt.Format(time.RFC3339),
	}}}
	mgr.RegisterExecutor(exec)
	auth := &Auth{
		ID:       "codex-low-quota",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "codex", "access_token": "tok"},
	}
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.refreshAuth(ctx, auth.ID)

	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected auth to remain registered")
	}
	if stored.Disabled {
		t.Fatal("expected low quota rest to keep auth registered and operator-enabled")
	}
	if !stored.Unavailable || !stored.Quota.Exceeded {
		t.Fatalf("expected auth to enter quota rest, unavailable=%v quota=%+v", stored.Unavailable, stored.Quota)
	}
	if stored.Quota.Reason != codexFiveHourQuotaLowReason {
		t.Fatalf("Quota.Reason = %q, want %q", stored.Quota.Reason, codexFiveHourQuotaLowReason)
	}
	if stored.NextRetryAfter.Before(resetAt.Add(-time.Second)) || stored.NextRetryAfter.After(resetAt.Add(time.Second)) {
		t.Fatalf("NextRetryAfter = %s, want near %s", stored.NextRetryAfter, resetAt)
	}
}

func TestManager_RefreshAuth_ClearsCodexQuotaRestWhenRecovered(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(nil, nil, nil)
	exec := &codexQuotaRefreshExecutor{updated: &Auth{Metadata: map[string]any{
		MetadataCodexFiveHourQuotaRemainingRatioKey: 0.5,
	}}}
	mgr.RegisterExecutor(exec)
	auth := &Auth{
		ID:             "codex-recovered-quota",
		Provider:       "codex",
		Status:         StatusError,
		StatusMessage:  "codex_5h_quota_low: remaining_ratio=0.10",
		Unavailable:    true,
		NextRetryAfter: time.Now().Add(time.Hour),
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        codexFiveHourQuotaLowReason,
			NextRecoverAt: time.Now().Add(time.Hour),
		},
		Metadata: map[string]any{"type": "codex", "access_token": "tok"},
	}
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.refreshAuth(ctx, auth.ID)

	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected auth to remain registered")
	}
	if stored.Unavailable || stored.Quota.Exceeded || stored.Status != StatusActive {
		t.Fatalf("expected quota rest to clear, status=%q unavailable=%v quota=%+v", stored.Status, stored.Unavailable, stored.Quota)
	}
}

func TestManager_RefreshAuth_PreservesCodexUsageLimitBeforeUpstreamReset(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	auth := &Auth{
		ID:             "codex-recovered-usage-limit",
		Provider:       "codex",
		Status:         StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: next,
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "usage_limit",
			NextRecoverAt: next,
		},
		Metadata: map[string]any{"type": "codex", "access_token": "tok"},
	}
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	exec := &codexQuotaRefreshExecutor{updated: &Auth{Metadata: map[string]any{
		MetadataCodexFiveHourQuotaRemainingRatioKey: 0.5,
	}}}
	mgr.RegisterExecutor(exec)

	mgr.refreshAuth(ctx, auth.ID)

	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected auth to remain registered")
	}
	if !stored.Unavailable || !stored.Quota.Exceeded || stored.Status != StatusError {
		t.Fatalf("expected usage_limit quota state to remain active before upstream reset, status=%q unavailable=%v quota=%+v", stored.Status, stored.Unavailable, stored.Quota)
	}
	if stored.Quota.Reason != "usage_limit" {
		t.Fatalf("quota reason = %q, want usage_limit", stored.Quota.Reason)
	}
}

func TestManager_RefreshAuth_ClearsCodexUsageLimitAfterUpstreamReset(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:             "codex-recovered-usage-limit-after-reset",
		Provider:       "codex",
		Status:         StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: time.Now().Add(-time.Minute),
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "usage_limit",
			NextRecoverAt: time.Now().Add(-time.Minute),
		},
		Metadata: map[string]any{"type": "codex", "access_token": "tok"},
	}
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	exec := &codexQuotaRefreshExecutor{updated: &Auth{Metadata: map[string]any{
		MetadataCodexFiveHourQuotaRemainingRatioKey: 0.5,
	}}}
	mgr.RegisterExecutor(exec)

	mgr.refreshAuth(ctx, auth.ID)

	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected auth to remain registered")
	}
	if stored.Unavailable || stored.Quota.Exceeded || stored.Status != StatusActive {
		t.Fatalf("expected usage_limit quota state to clear after upstream reset and recovered quota refresh, status=%q unavailable=%v quota=%+v", stored.Status, stored.Unavailable, stored.Quota)
	}
}

func TestManager_MarkResultSuccessPreservesCodexQuotaRest(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	auth := &Auth{
		ID:             "codex-rest-success",
		Provider:       "codex",
		Status:         StatusError,
		StatusMessage:  "codex_5h_quota_low: remaining_ratio=0.10",
		Unavailable:    true,
		NextRetryAfter: next,
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        codexFiveHourQuotaLowReason,
			NextRecoverAt: next,
		},
		Metadata: map[string]any{"type": "codex"},
	}
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.MarkResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5.4",
		Success:  true,
	})

	stored, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected auth to remain registered")
	}
	if !stored.Unavailable || !stored.Quota.Exceeded || stored.Quota.Reason != codexFiveHourQuotaLowReason {
		t.Fatalf("expected quota rest to remain active, unavailable=%v quota=%+v", stored.Unavailable, stored.Quota)
	}
}
