package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/performance"
)

func TestGetAuthPerformanceReturnsSnapshotsAndConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.ApplyRoutingPerformanceDefaults()
	cfg.Routing.PerformanceAware = true
	cfg.Routing.PerformanceWeightTPS = 1.5
	cfg.Routing.PerformanceWeightLatency = 0.4
	cfg.Routing.PerformanceWeightFailure = 3
	cfg.Routing.PerformanceWeightInflight = 0.7
	tracker := performance.NewTracker(performance.Config{Enabled: true, ShadowLog: true, Window: 5 * time.Minute, MinSamples: 1, EWMAAlpha: 1})
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	tracker.Record(performance.Sample{Provider: "codex", AuthID: "auth-a", AuthIndex: "2", Model: "gpt-5.4", RequestedAt: now, Latency: time.Second, OutputTokens: 20})

	h := NewHandler(cfg, "", nil)
	h.SetPerformanceTracker(tracker)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-performance?provider=codex&model=gpt-5.4&ready_only=true", nil)
	c.Request = req

	h.GetAuthPerformance(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"auth_id":"auth-a"`) || !strings.Contains(body, `"output_tps_ewma":20`) {
		t.Fatalf("unexpected body: %s", body)
	}
	for _, want := range []string{`"weight_tps":1.5`, `"weight_latency":0.4`, `"weight_failure":3`, `"weight_inflight":0.7`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, "access_token") || strings.Contains(body, "refresh_token") || strings.Contains(body, "api_key") {
		t.Fatalf("body leaks secret-like fields: %s", body)
	}
}

func TestGetAuthPerformanceHandlesMissingTracker(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.ApplyRoutingPerformanceDefaults()
	h := NewHandler(cfg, "", nil)
	h.SetPerformanceTracker(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-performance", nil)

	h.GetAuthPerformance(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"performance":[]`) {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}
