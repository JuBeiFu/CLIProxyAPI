package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestGetSlowAuthsReturnsOnlyActiveByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)

	slowAuth := &coreauth.Auth{
		ID:                         "slow-1",
		Provider:                   "codex",
		Label:                      "slow key",
		Status:                     coreauth.StatusActive,
		SlowRequestPriorityPenalty: 4,
		SlowRequestWindowCount:     2,
		SlowRequestWindowStartedAt: time.Now().Add(-30 * time.Second),
		SlowRequestCooldownUntil:   time.Now().Add(3 * time.Minute),
		LastObservedLatency:        23 * time.Second,
	}
	normalAuth := &coreauth.Auth{
		ID:                  "normal-1",
		Provider:            "codex",
		Label:               "normal key",
		Status:              coreauth.StatusActive,
		LastObservedLatency: 800 * time.Millisecond,
	}

	for _, auth := range []*coreauth.Auth{slowAuth, normalAuth} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth %s: %v", auth.ID, err)
		}
	}

	h := &Handler{authManager: manager}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/slow-auths", nil)

	h.GetSlowAuths(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var body struct {
		Items []slowAuthView `json:"items"`
		Count int            `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if body.Count != 1 {
		t.Fatalf("expected 1 active slow auth, got %d", body.Count)
	}
	if len(body.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(body.Items))
	}
	if body.Items[0].ID != "slow-1" {
		t.Fatalf("expected slow-1, got %s", body.Items[0].ID)
	}
	if body.Items[0].LastObservedLatencyMS != 23000 {
		t.Fatalf("expected 23000ms latency, got %d", body.Items[0].LastObservedLatencyMS)
	}
}

func TestGetSlowAuthsSupportsInactiveAndCooldownFilters(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)

	cooldownAuth := &coreauth.Auth{
		ID:                       "cooldown-1",
		Provider:                 "claude",
		Status:                   coreauth.StatusError,
		SlowRequestCooldownUntil: time.Now().Add(5 * time.Minute),
		LastObservedLatency:      21 * time.Second,
	}
	inactiveAuth := &coreauth.Auth{
		ID:                  "inactive-1",
		Provider:            "claude",
		Status:              coreauth.StatusActive,
		LastObservedLatency: 1200 * time.Millisecond,
	}

	for _, auth := range []*coreauth.Auth{cooldownAuth, inactiveAuth} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth %s: %v", auth.ID, err)
		}
	}

	h := &Handler{authManager: manager}

	recAll := httptest.NewRecorder()
	ctxAll, _ := gin.CreateTestContext(recAll)
	ctxAll.Request = httptest.NewRequest(http.MethodGet, "/v0/management/slow-auths?active_only=false&provider=claude", nil)
	h.GetSlowAuths(ctxAll)

	var allBody struct {
		Items []slowAuthView `json:"items"`
		Count int            `json:"count"`
	}
	if err := json.Unmarshal(recAll.Body.Bytes(), &allBody); err != nil {
		t.Fatalf("decode all response: %v", err)
	}
	if allBody.Count != 2 {
		t.Fatalf("expected 2 auths with active_only=false, got %d", allBody.Count)
	}

	recCooldown := httptest.NewRecorder()
	ctxCooldown, _ := gin.CreateTestContext(recCooldown)
	ctxCooldown.Request = httptest.NewRequest(http.MethodGet, "/v0/management/slow-auths?cooldown_only=true", nil)
	h.GetSlowAuths(ctxCooldown)

	var cooldownBody struct {
		Items []slowAuthView `json:"items"`
		Count int            `json:"count"`
	}
	if err := json.Unmarshal(recCooldown.Body.Bytes(), &cooldownBody); err != nil {
		t.Fatalf("decode cooldown response: %v", err)
	}
	if cooldownBody.Count != 1 {
		t.Fatalf("expected 1 cooldown auth, got %d", cooldownBody.Count)
	}
	if cooldownBody.Items[0].ID != "cooldown-1" {
		t.Fatalf("expected cooldown-1, got %s", cooldownBody.Items[0].ID)
	}
}
