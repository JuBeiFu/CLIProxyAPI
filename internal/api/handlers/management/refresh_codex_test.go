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
	// The handler is now async: it returns the job id immediately, NOT the
	// aggregated result.
	var startResp struct {
		JobID   string `json:"job_id"`
		Started bool   `json:"started"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &startResp); err != nil {
		t.Fatalf("unmarshal start: %v", err)
	}
	if strings.TrimSpace(startResp.JobID) == "" {
		t.Fatalf("expected non-empty job_id, body=%s", w.Body.String())
	}

	// Poll the status handler until the job completes.
	var statusResp coreauth.RefreshCodexJobView
	done := false
	for i := 0; i < 50; i++ {
		sw := httptest.NewRecorder()
		sc, _ := gin.CreateTestContext(sw)
		sc.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/refresh/status?job_id="+startResp.JobID, nil)
		h.RefreshCodexAuthsStatusHandler(sc)
		if sw.Code != http.StatusOK {
			t.Fatalf("status handler code = %d, body=%s", sw.Code, sw.Body.String())
		}
		statusResp = coreauth.RefreshCodexJobView{}
		if err := json.Unmarshal(sw.Body.Bytes(), &statusResp); err != nil {
			t.Fatalf("unmarshal status: %v", err)
		}
		if statusResp.Status == coreauth.RefreshJobDone {
			done = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !done {
		t.Fatalf("job did not complete; last status=%+v", statusResp)
	}
	if statusResp.Total != 1 || statusResp.Recovered != 1 {
		t.Fatalf("total=%d recovered=%d, want 1/1", statusResp.Total, statusResp.Recovered)
	}
}
