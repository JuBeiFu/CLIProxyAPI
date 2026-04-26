package management

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type statusQuotaRefreshExecutor struct {
	remainingRatio float64
	resetAt        time.Time
	err            error
	refreshes      atomic.Int32
}

func (e *statusQuotaRefreshExecutor) Identifier() string { return "codex" }

func (e *statusQuotaRefreshExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *statusQuotaRefreshExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *statusQuotaRefreshExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	e.refreshes.Add(1)
	if e.err != nil {
		return nil, e.err
	}
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata[coreauth.MetadataCodexFiveHourQuotaRemainingRatioKey] = e.remainingRatio
	updated.Metadata[coreauth.MetadataCodexFiveHourQuotaResetAtKey] = e.resetAt.Format(time.RFC3339)
	return updated, nil
}

func (e *statusQuotaRefreshExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *statusQuotaRefreshExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestPatchAuthFileStatus_EnableCodexQuotaRestRefreshesBeforeReuse(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	resetAt := time.Now().Add(90 * time.Minute).UTC()

	record := &coreauth.Auth{
		ID:             "quota-rest.json",
		FileName:       "quota-rest.json",
		Provider:       "codex",
		Disabled:       true,
		Status:         coreauth.StatusDisabled,
		StatusMessage:  "disabled after quota exhaustion",
		Unavailable:    true,
		NextRetryAfter: resetAt,
		Quota: coreauth.QuotaState{
			Exceeded:      true,
			Reason:        "usage_limit",
			NextRecoverAt: resetAt,
			UpdatedAt:     time.Now().UTC(),
		},
		Attributes: map[string]string{"path": "/tmp/quota-rest.json"},
		Metadata:   map[string]any{"type": "codex", "access_token": "tok"},
	}
	if _, errRegister := manager.Register(ctx, record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}
	exec := &statusQuotaRefreshExecutor{
		remainingRatio: 0.10,
		resetAt:        resetAt,
	}
	manager.RegisterExecutor(exec)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	body := `{"name":"quota-rest.json","disabled":false}`
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/status", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ginCtx.Request = req
	h.PatchAuthFileStatus(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if exec.refreshes.Load() == 0 {
		t.Fatalf("expected enabling quota-rested codex auth to refresh quota before reuse")
	}

	updated, ok := manager.GetByID("quota-rest.json")
	if !ok || updated == nil {
		t.Fatalf("expected auth record to exist after patch")
	}
	if updated.Disabled {
		t.Fatalf("expected operator disabled flag to be cleared after enable")
	}
	if updated.Status != coreauth.StatusError || !updated.Unavailable || !updated.Quota.Exceeded {
		t.Fatalf("expected low-quota auth to remain unavailable after refresh, status=%q unavailable=%v quota=%+v", updated.Status, updated.Unavailable, updated.Quota)
	}
	if updated.Quota.Reason != "codex_5h_quota_low" {
		t.Fatalf("quota reason = %q, want codex_5h_quota_low", updated.Quota.Reason)
	}
}

func TestPatchAuthFileStatus_EnableCodexQuotaMessageRefreshesBeforeReuse(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	record := &coreauth.Auth{
		ID:            "quota-message.json",
		FileName:      "quota-message.json",
		Provider:      "codex",
		Disabled:      true,
		Status:        coreauth.StatusDisabled,
		StatusMessage: "quota exhausted",
		Attributes:    map[string]string{"path": "/tmp/quota-message.json"},
		Metadata:      map[string]any{"type": "codex", "access_token": "tok"},
	}
	if _, errRegister := manager.Register(ctx, record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}
	exec := &statusQuotaRefreshExecutor{
		remainingRatio: 0.50,
		resetAt:        time.Now().Add(time.Hour).UTC(),
	}
	manager.RegisterExecutor(exec)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	body := `{"name":"quota-message.json","disabled":false}`
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/status", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ginCtx.Request = req
	h.PatchAuthFileStatus(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if exec.refreshes.Load() == 0 {
		t.Fatalf("expected enabling codex auth with quota status message to refresh quota before reuse")
	}
	updated, ok := manager.GetByID("quota-message.json")
	if !ok || updated == nil {
		t.Fatalf("expected auth record to exist after patch")
	}
	if updated.Status != coreauth.StatusActive || updated.Unavailable || updated.Quota.Exceeded {
		t.Fatalf("expected recovered quota refresh to return auth to active, status=%q unavailable=%v quota=%+v", updated.Status, updated.Unavailable, updated.Quota)
	}
}

func TestPatchAuthFileStatus_EnableCodexQuotaRefreshFailureKeepsAuthBlocked(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	record := &coreauth.Auth{
		ID:            "quota-refresh-fails.json",
		FileName:      "quota-refresh-fails.json",
		Provider:      "codex",
		Disabled:      true,
		Status:        coreauth.StatusDisabled,
		StatusMessage: "quota exhausted",
		Attributes:    map[string]string{"path": "/tmp/quota-refresh-fails.json"},
		Metadata:      map[string]any{"type": "codex", "access_token": "tok"},
	}
	if _, errRegister := manager.Register(ctx, record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}
	exec := &statusQuotaRefreshExecutor{err: errors.New("quota probe failed")}
	manager.RegisterExecutor(exec)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	body := `{"name":"quota-refresh-fails.json","disabled":false}`
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/status", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ginCtx.Request = req
	h.PatchAuthFileStatus(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if exec.refreshes.Load() == 0 {
		t.Fatalf("expected enabling codex auth with quota status to attempt quota refresh")
	}
	updated, ok := manager.GetByID("quota-refresh-fails.json")
	if !ok || updated == nil {
		t.Fatalf("expected auth record to exist after patch")
	}
	if updated.Disabled {
		t.Fatalf("expected operator disabled flag to be cleared after enable")
	}
	if !updated.Unavailable || updated.Status != coreauth.StatusError {
		t.Fatalf("expected failed quota refresh to keep auth blocked, status=%q unavailable=%v", updated.Status, updated.Unavailable)
	}
	if time.Until(updated.NextRetryAfter) < 4*time.Minute {
		t.Fatalf("expected failed quota refresh to keep auth blocked for refresh backoff, next_retry_after=%s", updated.NextRetryAfter)
	}
}
