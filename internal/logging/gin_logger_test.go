package logging

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

func TestGinLogrusRecoveryRepanicsErrAbortHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(GinLogrusRecovery())
	engine.GET("/abort", func(c *gin.Context) {
		panic(http.ErrAbortHandler)
	})

	req := httptest.NewRequest(http.MethodGet, "/abort", nil)
	recorder := httptest.NewRecorder()

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatalf("expected panic, got nil")
		}
		err, ok := recovered.(error)
		if !ok {
			t.Fatalf("expected error panic, got %T", recovered)
		}
		if !errors.Is(err, http.ErrAbortHandler) {
			t.Fatalf("expected ErrAbortHandler, got %v", err)
		}
		if err != http.ErrAbortHandler {
			t.Fatalf("expected exact ErrAbortHandler sentinel, got %v", err)
		}
	}()

	engine.ServeHTTP(recorder, req)
}

func TestGinLogrusRecoveryHandlesRegularPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(GinLogrusRecovery())
	engine.GET("/panic", func(c *gin.Context) {
		panic("boom")
	})

	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", recorder.Code)
	}
}

func TestGinLogrusLoggerPrefersInboundOneAPIRequestID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(GinLogrusLogger())

	const inbound = "oneapi-req-123"
	engine.POST("/v1/responses", func(c *gin.Context) {
		if got := GetGinRequestID(c); got != inbound {
			t.Fatalf("gin request id = %q, want %q", got, inbound)
		}
		if got := GetRequestID(c.Request.Context()); got != inbound {
			t.Fatalf("context request id = %q, want %q", got, inbound)
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("X-Oneapi-Request-Id", inbound)
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", recorder.Code)
	}
}

func TestGinLogrusLoggerSuccessfulAIRequestsLogAtDebug(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logger := log.StandardLogger()
	prevOut := logger.Out
	prevFormatter := logger.Formatter
	prevLevel := logger.GetLevel()
	prevReportCaller := logger.ReportCaller

	var buffer bytes.Buffer
	logger.SetOutput(&buffer)
	logger.SetFormatter(&log.TextFormatter{DisableTimestamp: true, DisableQuote: true})
	logger.SetLevel(log.DebugLevel)
	logger.SetReportCaller(false)
	defer func() {
		logger.SetOutput(prevOut)
		logger.SetFormatter(prevFormatter)
		logger.SetLevel(prevLevel)
		logger.SetReportCaller(prevReportCaller)
	}()

	engine := gin.New()
	engine.Use(GinLogrusLogger())
	engine.POST("/v1/responses", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", recorder.Code)
	}
	output := buffer.String()
	if !strings.Contains(output, "level=debug") {
		t.Fatalf("expected debug log level, output=%q", output)
	}
	if strings.Contains(output, "level=info") {
		t.Fatalf("unexpected info log level, output=%q", output)
	}
}
