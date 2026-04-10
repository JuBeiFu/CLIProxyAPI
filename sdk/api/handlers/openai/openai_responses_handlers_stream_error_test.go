package openai

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestForwardResponsesStreamTerminalErrorUsesResponsesErrorChunk(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{RequestLog: true}, nil)
	h := NewOpenAIResponsesAPIHandler(base)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatalf("expected gin writer to implement http.Flusher")
	}

	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: errors.New("unexpected EOF")}
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, data, errs, nil)
	body := recorder.Body.String()
	if !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("expected responses error chunk, got: %q", body)
	}
	if strings.Contains(body, `"error":{`) {
		t.Fatalf("expected streaming error chunk (top-level type), got HTTP error body: %q", body)
	}
	logged, exists := c.Get("API_RESPONSE_ERROR")
	if !exists {
		t.Fatalf("expected API_RESPONSE_ERROR to be recorded")
	}
	apiErrors, ok := logged.([]*interfaces.ErrorMessage)
	if !ok || len(apiErrors) != 1 {
		t.Fatalf("expected one API response error, got %#v", logged)
	}
	if _, exists := c.Get("API_RESPONSE_TIMESTAMP"); !exists {
		t.Fatalf("expected API_RESPONSE_TIMESTAMP to be recorded")
	}
}

func TestForwardResponsesStreamClosedWithoutCompletedEmitsErrorChunk(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{RequestLog: true}, nil)
	h := NewOpenAIResponsesAPIHandler(base)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatalf("expected gin writer to implement http.Flusher")
	}

	data := make(chan []byte, 1)
	errs := make(chan *interfaces.ErrorMessage)
	data <- []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\"}\n")
	close(data)
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, data, errs, nil)
	body := recorder.Body.String()
	if !strings.Contains(body, `stream closed before response.completed`) {
		t.Fatalf("expected missing response.completed error, got: %q", body)
	}
	logged, exists := c.Get("API_RESPONSE_ERROR")
	if !exists {
		t.Fatalf("expected API_RESPONSE_ERROR to be recorded")
	}
	apiErrors, ok := logged.([]*interfaces.ErrorMessage)
	if !ok || len(apiErrors) != 1 {
		t.Fatalf("expected one API response error, got %#v", logged)
	}
	if apiErrors[0].StatusCode != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want %d", apiErrors[0].StatusCode, http.StatusRequestTimeout)
	}
	value, exists := c.Get("API_RESPONSE_TIMESTAMP")
	if !exists {
		t.Fatalf("expected API_RESPONSE_TIMESTAMP to be recorded")
	}
	if _, ok := value.(time.Time); !ok {
		t.Fatalf("API_RESPONSE_TIMESTAMP has unexpected type %T", value)
	}
}

func TestForwardResponsesStreamCompletedDoesNotEmitSyntheticError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{RequestLog: true}, nil)
	h := NewOpenAIResponsesAPIHandler(base)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatalf("expected gin writer to implement http.Flusher")
	}

	data := make(chan []byte, 1)
	errs := make(chan *interfaces.ErrorMessage)
	data <- []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n")
	close(data)
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, data, errs, nil)
	body := recorder.Body.String()
	if strings.Contains(body, `stream closed before response.completed`) {
		t.Fatalf("unexpected synthetic error in completed stream: %q", body)
	}
	if _, exists := c.Get("API_RESPONSE_ERROR"); exists {
		t.Fatalf("did not expect API_RESPONSE_ERROR for completed stream")
	}
}
