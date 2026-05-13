package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestPrecommitEventStreamWritesCommentAndHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatal("expected gin response writer to implement http.Flusher")
	}

	stop := PrecommitEventStream(c, flusher, nil)
	stop()

	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := rec.Body.String(); !strings.Contains(got, ": proxy-stream-start\n\n") {
		t.Fatalf("precommit comment missing from body: %q", got)
	}
}

func TestPrepareEventStreamHeadersWritesPrecommitDiagnosticComments(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatal("expected gin response writer to implement http.Flusher")
	}

	stop := PrecommitEventStream(c, flusher, nil)
	stop()

	headers := http.Header{}
	headers.Set("X-CLIProxy-Codex-Upstream-Proto", "HTTP/1.1")
	PrepareEventStreamHeaders(c, headers)

	if got := rec.Body.String(); !strings.Contains(got, ": cliproxy-header X-CLIProxy-Codex-Upstream-Proto: HTTP/1.1\n\n") {
		t.Fatalf("diagnostic comment missing from body: %q", got)
	}
}
