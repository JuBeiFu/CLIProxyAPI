package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestRefreshCodexAuthsHandler_RecoversCooling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)

	rec := &coreauth.Auth{
		ID: "c1.json", FileName: "c1.json", Provider: "codex",
		Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour),
		// "codex_5h_quota_low" is the reason the Task-6 5h-ratio recovery path keys
		// on: statusQuotaRefreshExecutor.Refresh writes a healthy 5h ratio (0.9),
		// which clears this cooldown. A plain "usage_limit" reason would instead
		// stay gated on its future NextRecoverAt timer and not recover here.
		Quota:      coreauth.QuotaState{Exceeded: true, Reason: "codex_5h_quota_low", NextRecoverAt: time.Now().Add(time.Hour)},
		Metadata:   map[string]any{"type": "codex", "access_token": "tok"},
		Attributes: map[string]string{"path": "/tmp/c1.json"},
	}
	if _, err := manager.Register(ctx, rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Register the executor AFTER Register so the fire-and-forget codex probe
	// inside Register (gated on executorFor(provider) != nil) does NOT fire and
	// race the handler. This keeps the auth deterministically cooling until the
	// handler's explicit RefreshCodexAuths runs the recovery.
	manager.RegisterExecutor(&statusQuotaRefreshExecutor{remainingRatio: 0.9, resetAt: time.Now().Add(time.Hour)})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	body := `{"provider":"codex","cooling_only":true}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/refresh", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.RefreshCodexAuthsHandler(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp coreauth.RefreshCodexResult
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Total != 1 || resp.Recovered != 1 {
		t.Fatalf("total=%d recovered=%d, want 1/1", resp.Total, resp.Recovered)
	}
}
