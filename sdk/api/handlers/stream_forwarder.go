package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

const defaultStreamPrecommitDelay = 200 * time.Millisecond

const precommitHeaderCommentsWrittenKey = "__cliproxy_precommit_header_comments_written"

var precommitDiagnosticHeaderNames = []string{
	"X-CLIProxy-Codex-Upstream-Proto",
	"X-CLIProxy-Codex-Stream-Transport",
	"X-CLIProxy-Codex-HTTP2-Disabled",
}

func SetEventStreamHeaders(c *gin.Context) {
	if c == nil {
		return
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("Access-Control-Allow-Origin", "*")
}

func PrepareEventStreamHeaders(c *gin.Context, upstreamHeaders http.Header) {
	if c == nil {
		return
	}
	if c.Writer.Written() {
		WritePrecommitUpstreamHeaderComments(c, upstreamHeaders)
		return
	}
	SetEventStreamHeaders(c)
	WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
}

func WritePrecommitUpstreamHeaderComments(c *gin.Context, upstreamHeaders http.Header) {
	if c == nil || upstreamHeaders == nil || !c.Writer.Written() {
		return
	}
	if _, exists := c.Get(precommitHeaderCommentsWrittenKey); exists {
		return
	}
	c.Set(precommitHeaderCommentsWrittenKey, true)
	for _, name := range precommitDiagnosticHeaderNames {
		for _, value := range upstreamHeaders.Values(name) {
			value = sanitizeSSECommentValue(value)
			if value == "" {
				continue
			}
			_, _ = fmt.Fprintf(c.Writer, ": cliproxy-header %s: %s\n\n", name, value)
		}
	}
}

func sanitizeSSECommentValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	if len(value) > 256 {
		return value[:256]
	}
	return value
}

// PrecommitEventStream sends a harmless SSE comment before upstream bootstrap.
// Real data is still withheld by the auth manager until the stream is no longer retryable.
func PrecommitEventStream(c *gin.Context, flusher http.Flusher, cfg *config.SDKConfig) func() {
	if c == nil || flusher == nil || c.Writer.Written() {
		return func() {}
	}
	reqCtx := context.Background()
	if c.Request != nil {
		reqCtx = c.Request.Context()
	}
	SetEventStreamHeaders(c)
	_, _ = c.Writer.Write([]byte(": proxy-stream-start\n\n"))
	flusher.Flush()

	interval := StreamingKeepAliveInterval(cfg)
	if interval <= 0 {
		return func() {}
	}

	stopChan := make(chan struct{})
	var stopOnce sync.Once
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopChan:
				return
			case <-reqCtx.Done():
				return
			case <-ticker.C:
				_, _ = c.Writer.Write([]byte(": keep-alive\n\n"))
				flusher.Flush()
			}
		}
	}()

	return func() {
		stopOnce.Do(func() {
			close(stopChan)
		})
		wg.Wait()
	}
}

type streamStartResult struct {
	data    <-chan []byte
	headers http.Header
	errs    <-chan *interfaces.ErrorMessage
}

func (h *BaseAPIHandler) ExecuteStreamWithAuthManagerPrecommit(ctx context.Context, c *gin.Context, flusher http.Flusher, handlerType, modelName string, rawJSON []byte, alt string) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	return h.executeStreamWithAuthManagerPrecommit(ctx, c, flusher, handlerType, modelName, modelName, rawJSON, alt)
}

func (h *BaseAPIHandler) ExecuteStreamWithAuthManagerRequestedModelPrecommit(ctx context.Context, c *gin.Context, flusher http.Flusher, handlerType, modelName string, rawJSON []byte, alt string, requestedModel string) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	return h.executeStreamWithAuthManagerPrecommit(ctx, c, flusher, handlerType, modelName, requestedModel, rawJSON, alt)
}

func (h *BaseAPIHandler) executeStreamWithAuthManagerPrecommit(ctx context.Context, c *gin.Context, flusher http.Flusher, handlerType, modelName string, requestedModel string, rawJSON []byte, alt string) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	if ctx == nil {
		if c != nil && c.Request != nil {
			ctx = c.Request.Context()
		} else {
			ctx = context.Background()
		}
	}
	resultCh := make(chan streamStartResult, 1)
	go func() {
		data, headers, errs := h.executeStreamWithAuthManager(ctx, handlerType, modelName, requestedModel, rawJSON, alt)
		resultCh <- streamStartResult{data: data, headers: headers, errs: errs}
	}()

	if c == nil || c.Request == nil || flusher == nil {
		result := <-resultCh
		return result.data, result.headers, result.errs
	}

	timer := time.NewTimer(defaultStreamPrecommitDelay)
	timerC := timer.C
	defer timer.Stop()
	stopPrecommit := func() {}
	defer stopPrecommit()

	for {
		select {
		case result := <-resultCh:
			return result.data, result.headers, result.errs
		case <-timerC:
			timerC = nil
			stopPrecommit = PrecommitEventStream(c, flusher, h.Cfg)
		}
	}
}

type StreamForwardOptions struct {
	// KeepAliveInterval overrides the configured streaming keep-alive interval.
	// If nil, the configured default is used. If set to <= 0, keep-alives are disabled.
	KeepAliveInterval *time.Duration

	// WriteChunk writes a single data chunk to the response body. It should not flush.
	WriteChunk func(chunk []byte)

	// WriteTerminalError writes an error payload to the response body when streaming fails
	// after headers have already been committed. It should not flush.
	WriteTerminalError func(errMsg *interfaces.ErrorMessage)

	// WriteDone optionally writes a terminal marker when the upstream data channel closes
	// without an error (e.g. OpenAI's `[DONE]`). It should not flush.
	WriteDone func()

	// WriteKeepAlive optionally writes a keep-alive heartbeat. It should not flush.
	// When nil, a standard SSE comment heartbeat is used.
	WriteKeepAlive func()
}

func (h *BaseAPIHandler) ForwardStream(c *gin.Context, flusher http.Flusher, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage, opts StreamForwardOptions) {
	if c == nil {
		return
	}
	if cancel == nil {
		return
	}

	writeChunk := opts.WriteChunk
	if writeChunk == nil {
		writeChunk = func([]byte) {}
	}

	writeKeepAlive := opts.WriteKeepAlive
	if writeKeepAlive == nil {
		writeKeepAlive = func() {
			_, _ = c.Writer.Write([]byte(": keep-alive\n\n"))
		}
	}

	keepAliveInterval := StreamingKeepAliveInterval(h.Cfg)
	if opts.KeepAliveInterval != nil {
		keepAliveInterval = *opts.KeepAliveInterval
	}
	var keepAlive *time.Ticker
	var keepAliveC <-chan time.Time
	if keepAliveInterval > 0 {
		keepAlive = time.NewTicker(keepAliveInterval)
		defer keepAlive.Stop()
		keepAliveC = keepAlive.C
	}

	var terminalErr *interfaces.ErrorMessage
	for {
		select {
		case <-c.Request.Context().Done():
			cancel(c.Request.Context().Err())
			return
		case chunk, ok := <-data:
			if !ok {
				// Prefer surfacing a terminal error if one is pending.
				if terminalErr == nil {
					select {
					case errMsg, ok := <-errs:
						if ok && errMsg != nil {
							terminalErr = errMsg
						}
					default:
					}
				}
				if terminalErr != nil {
					if opts.WriteTerminalError != nil {
						opts.WriteTerminalError(terminalErr)
					}
					flusher.Flush()
					cancel(terminalErr.Error)
					return
				}
				if opts.WriteDone != nil {
					opts.WriteDone()
				}
				flusher.Flush()
				cancel(nil)
				return
			}
			writeChunk(chunk)
			flusher.Flush()
		case errMsg, ok := <-errs:
			if !ok {
				continue
			}
			if errMsg != nil {
				terminalErr = errMsg
				if opts.WriteTerminalError != nil {
					opts.WriteTerminalError(errMsg)
					flusher.Flush()
				}
			}
			var execErr error
			if errMsg != nil {
				execErr = errMsg.Error
			}
			cancel(execErr)
			return
		case <-keepAliveC:
			writeKeepAlive()
			flusher.Flush()
		}
	}
}
