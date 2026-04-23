package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestListAuthFilesCachesResponseAndInvalidatesOnAuthChange(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "alpha.json",
		FileName: "alpha.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "alpha.json",
		},
	}); err != nil {
		t.Fatalf("register alpha auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.listAuthFilesCacheTTL = time.Minute

	callList := func() []map[string]any {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
		h.ListAuthFiles(ctx)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d with body %s", rec.Code, rec.Body.String())
		}
		var payload struct {
			Files []map[string]any `json:"files"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode list response: %v", err)
		}
		return payload.Files
	}

	first := callList()
	if len(first) != 1 {
		t.Fatalf("expected one auth entry, got %d", len(first))
	}

	if _, err := manager.Update(context.Background(), &coreauth.Auth{
		ID:       "alpha.json",
		FileName: "alpha.json",
		Provider: "codex",
		Label:    "updated",
		Attributes: map[string]string{
			"path": "alpha.json",
		},
	}); err != nil {
		t.Fatalf("update alpha auth: %v", err)
	}

	second := callList()
	if len(second) != 1 {
		t.Fatalf("expected one auth entry after update, got %d", len(second))
	}
	if got, _ := second[0]["label"].(string); got != "updated" {
		t.Fatalf("expected updated label after cache invalidation, got %q", got)
	}
}
