package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestServer_ManagementProxyPoolsRoute_ReturnsConfiguredPools(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-secret")
	gin.SetMode(gin.TestMode)

	server := NewServer(
		&config.Config{
			AuthDir: ".",
			Port:    8317,
			SDKConfig: config.SDKConfig{
				DefaultProxyPool: "shared-egress",
				ProxyPools: []config.ProxyPool{
					{
						Name:             "shared-egress",
						FallbackToDirect: true,
						Entries: []config.ProxyPoolEntry{
							{Name: "proxy-a", URL: "http://proxy-a.local:8080"},
						},
					},
				},
			},
		},
		coreauth.NewManager(nil, nil, nil),
		nil,
		"config.yaml",
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/management/proxy-pools", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Authorization", "Bearer test-secret")
	server.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to parse response body: %v", err)
	}
	if _, ok := payload["proxy-health"]; !ok {
		t.Fatalf("expected proxy-health in response, got %v", payload)
	}
}
