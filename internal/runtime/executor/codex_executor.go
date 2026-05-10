package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/proxypool"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/tiktoken-go/tokenizer"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	codexUserAgent                  = "codex-tui/0.125.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.125.0)"
	codexOriginator                 = "codex-tui"
	newAPIDownstreamTransportHeader = "X-NewAPI-Downstream-Transport"
)

var dataTag = []byte("data:")

const codexSlowTimingThreshold = 30 * time.Second

var codexRetryAfterPhrasePattern = regexp.MustCompile(`(?i)please try again in ([0-9]+(?:\.[0-9]+)?)(ms|milliseconds?|s|sec|secs|seconds?)`)

func isPaidCodexPlanType(plan string) bool {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case "plus", "pro", "team", "enterprise":
		return true
	}
	return false
}

func syncCodexProbeRoutingState(auth *cliproxyauth.Auth, realPlan, boundEntry string) {
	if auth == nil {
		return
	}
	lease := cliproxyauth.IPv6BindLease(auth)
	paidViaLease := lease.URL != "" && lease.IP != "" && boundEntry == "" && isPaidCodexPlanType(realPlan)
	if !paidViaLease && strings.TrimSpace(boundEntry) != "" && lease.URL != "" && lease.IP != "" {
		// Resolver precedence is IPv6 lease > bound proxy entry. If the paid
		// path was discovered on a pool entry or direct fallback, keeping a stale
		// lease would route the real request back onto the broken path.
		cliproxyauth.ClearIPv6BindLease(auth)
	}
	if strings.TrimSpace(boundEntry) != "" {
		cliproxyauth.SetBoundProxyEntry(auth, boundEntry)
		return
	}
	cliproxyauth.SetBoundProxyEntry(auth, "")
}

type codexUpstreamTiming struct {
	endpoint        string
	model           string
	authID          string
	proxySource     string
	proxyPool       string
	proxyName       string
	proxyURL        string
	proxyFallback   bool
	runtimeMode     string
	directBindIP    string
	failoverState   string
	failoverReason  string
	stickyAuthID    string
	stickyLease     string
	status          int
	bytesRead       int
	startedAt       time.Time
	prepare         time.Duration
	httpDo          time.Duration
	readBody        time.Duration
	translate       time.Duration
	traceConn       time.Duration
	traceTLS        time.Duration
	traceWroteReq   time.Duration
	traceFirstByte  time.Duration
	traceReusedConn bool
	traceWasIdle    bool
	traceIdleTime   time.Duration
	streamLines     int
	streamChunks    int
	streamCompleted bool
	streamErrText   string
}

func logSlowCodexUpstreamTiming(ctx context.Context, timing codexUpstreamTiming) {
	total := time.Since(timing.startedAt)
	errText := strings.TrimSpace(timing.streamErrText)
	if total < codexSlowTimingThreshold && errText == "" && timing.streamCompleted {
		return
	}
	if errText == "" {
		errText = "-"
	}
	runtimeMode := strings.TrimSpace(timing.runtimeMode)
	if runtimeMode == "" {
		runtimeMode = proxypool.CodexResolutionTelemetryFor(nil, proxypool.Resolution{
			ProxyURL: timing.proxyURL,
			Source:   timing.proxySource,
		}, time.Now()).RuntimeEgressMode
		if runtimeMode == "" {
			runtimeMode = "assisted"
		}
	}
	legacyBound := strings.TrimSpace(timing.proxySource) == "bound-assisted"
	helps.LogWithRequestID(ctx).Infof(
		"codex upstream timing endpoint=%s model=%s auth_id=%s proxy_source=%s proxy_pool=%s proxy_name=%s proxy_url=%s proxy_fallback_direct=%t runtime_egress_mode=%s direct_bind_ip=%s failover_state=%s failover_reason=%s sticky_auth_id=%s sticky_lease=%s legacy_bound_proxy_used=%t resolution_source=%s status=%d total=%s prepare=%s http_do=%s read_body=%s translate=%s http_conn=%s http_tls=%s http_wrote_req=%s http_first_byte=%s http_conn_reused=%t http_conn_was_idle=%t http_conn_idle=%s response_bytes=%d stream_lines=%d stream_chunks=%d stream_completed=%t stream_err=%s",
		timing.endpoint,
		timing.model,
		strings.TrimSpace(timing.authID),
		strings.TrimSpace(timing.proxySource),
		strings.TrimSpace(timing.proxyPool),
		strings.TrimSpace(timing.proxyName),
		strings.TrimSpace(timing.proxyURL),
		timing.proxyFallback,
		runtimeMode,
		strings.TrimSpace(timing.directBindIP),
		strings.TrimSpace(timing.failoverState),
		strings.TrimSpace(timing.failoverReason),
		strings.TrimSpace(timing.stickyAuthID),
		strings.TrimSpace(timing.stickyLease),
		legacyBound,
		strings.TrimSpace(timing.proxySource),
		timing.status,
		total.Round(time.Millisecond),
		timing.prepare.Round(time.Millisecond),
		timing.httpDo.Round(time.Millisecond),
		timing.readBody.Round(time.Millisecond),
		timing.translate.Round(time.Millisecond),
		timing.traceConn.Round(time.Millisecond),
		timing.traceTLS.Round(time.Millisecond),
		timing.traceWroteReq.Round(time.Millisecond),
		timing.traceFirstByte.Round(time.Millisecond),
		timing.traceReusedConn,
		timing.traceWasIdle,
		timing.traceIdleTime.Round(time.Millisecond),
		timing.bytesRead,
		timing.streamLines,
		timing.streamChunks,
		timing.streamCompleted,
		errText,
	)
}

func applyCodexResolutionTiming(timing *codexUpstreamTiming, auth *cliproxyauth.Auth, resolution proxypool.Resolution) {
	if timing == nil {
		return
	}
	timing.proxySource = resolution.Source
	timing.proxyPool = resolution.ProxyPool
	timing.proxyName = resolution.ProxyName
	timing.proxyURL = resolution.ProxyURL
	timing.proxyFallback = resolution.FallbackToDirect
	telemetry := proxypool.CodexResolutionTelemetryFor(auth, resolution, time.Now())
	timing.runtimeMode = telemetry.RuntimeEgressMode
	timing.directBindIP = telemetry.DirectBindIP
	timing.failoverState = telemetry.FailoverState
	timing.failoverReason = telemetry.FailoverReason
	timing.stickyAuthID = telemetry.StickyAuthID
	timing.stickyLease = telemetry.StickyLease
}

func recordCodexProxyPassiveOutcome(timing codexUpstreamTiming, manager *proxypool.HealthManager) {
	if manager == nil {
		return
	}
	if strings.TrimSpace(timing.proxyPool) == "" || strings.TrimSpace(timing.proxyName) == "" {
		return
	}
	total := time.Since(timing.startedAt)
	if total <= 0 {
		return
	}
	manager.ReportPassiveOutcome(timing.proxyPool, timing.proxyName, proxypool.PassiveOutcome{
		Model:         timing.model,
		Endpoint:      timing.endpoint,
		Total:         total,
		FirstByte:     passiveFirstByteForCodexTiming(timing),
		ReadBody:      timing.readBody,
		ResponseBytes: int64(timing.bytesRead),
		StatusCode:    timing.status,
		Successful:    timing.streamErrText == "" && timing.status >= http.StatusOK && timing.status < http.StatusMultipleChoices && timing.streamCompleted,
		Error:         timing.streamErrText,
		CheckedAt:     time.Now(),
	})
}

func recordCodexRoutePassiveOutcome(timing codexUpstreamTiming, registry *proxypool.CodexRouteRegistry) {
	if registry == nil {
		return
	}
	if strings.TrimSpace(timing.authID) == "" || strings.TrimSpace(timing.proxyPool) == "" || strings.TrimSpace(timing.proxyName) == "" {
		return
	}
	total := time.Since(timing.startedAt)
	if total <= 0 {
		return
	}
	registry.RecordPassiveOutcome(timing.authID, proxypool.RouteDescriptor{
		Pool:  timing.proxyPool,
		Entry: timing.proxyName,
	}, proxypool.RoutePassiveOutcome{
		CheckedAt:  time.Now(),
		FirstByte:  passiveFirstByteForCodexTiming(timing),
		Total:      total,
		ReadBody:   timing.readBody,
		StatusCode: timing.status,
		Successful: timing.streamErrText == "" && timing.status >= http.StatusOK && timing.status < http.StatusMultipleChoices && timing.streamCompleted,
		Error:      timing.streamErrText,
	})
}

func passiveFirstByteForCodexTiming(timing codexUpstreamTiming) time.Duration {
	if strings.EqualFold(strings.TrimSpace(timing.endpoint), "responses/compact") {
		return 0
	}
	return timing.traceFirstByte
}

func finishCodexUpstreamTiming(ctx context.Context, timing codexUpstreamTiming) {
	recordCodexProxyPassiveOutcome(timing, proxypool.DefaultHealthManager())
	recordCodexRoutePassiveOutcome(timing, proxypool.DefaultCodexRouteRegistry())
	logSlowCodexUpstreamTiming(ctx, timing)
}

type codexHTTPTraceState struct {
	startedAt    time.Time
	connectStart time.Time
	gotConnAt    time.Time
	tlsStart     time.Time
	wroteReqAt   time.Time
	firstByteAt  time.Time
	reusedConn   bool
}

func withCodexHTTPTrace(ctx context.Context, timing *codexUpstreamTiming) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if timing == nil {
		return ctx
	}
	state := &codexHTTPTraceState{startedAt: time.Now()}
	trace := &httptrace.ClientTrace{
		GetConn: func(string) {
			state.connectStart = time.Now()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			state.gotConnAt = time.Now()
			state.reusedConn = info.Reused
			timing.traceWasIdle = info.WasIdle
			timing.traceIdleTime = info.IdleTime
		},
		TLSHandshakeStart: func() {
			state.tlsStart = time.Now()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
			if !state.tlsStart.IsZero() {
				timing.traceTLS += time.Since(state.tlsStart)
			}
		},
		WroteRequest: func(httptrace.WroteRequestInfo) {
			state.wroteReqAt = time.Now()
			if !state.gotConnAt.IsZero() {
				timing.traceWroteReq += state.wroteReqAt.Sub(state.gotConnAt)
			} else if !state.startedAt.IsZero() {
				timing.traceWroteReq += state.wroteReqAt.Sub(state.startedAt)
			}
		},
		GotFirstResponseByte: func() {
			state.firstByteAt = time.Now()
			if !state.connectStart.IsZero() && !state.gotConnAt.IsZero() {
				timing.traceConn += state.gotConnAt.Sub(state.connectStart)
			} else if !state.startedAt.IsZero() {
				timing.traceConn += time.Since(state.startedAt)
			}
			if !state.wroteReqAt.IsZero() {
				timing.traceFirstByte += time.Since(state.wroteReqAt)
			}
			timing.traceReusedConn = state.reusedConn
		},
	}
	return httptrace.WithClientTrace(ctx, trace)
}

func shouldPreserveCodexPreviousResponseID(ctx context.Context, auth *cliproxyauth.Auth, from sdktranslator.Format, body []byte) bool {
	if cliproxyexecutor.DownstreamWebsocket(ctx) {
		return true
	}
	_ = auth
	_ = from
	_ = body
	return false
}

func stripCodexUnsupportedResponseFields(body []byte, preservePreviousResponseID bool) []byte {
	if !preservePreviousResponseID {
		body, _ = sjson.DeleteBytes(body, "previous_response_id")
	}
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	return body
}

// CodexExecutor is a stateless executor for Codex (OpenAI Responses API entrypoint).
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type CodexExecutor struct {
	cfg *config.Config
}

func NewCodexExecutor(cfg *config.Config) *CodexExecutor { return &CodexExecutor{cfg: cfg} }

func (e *CodexExecutor) Identifier() string { return "codex" }

// PrepareRequest injects Codex credentials into the outgoing HTTP request.
func (e *CodexExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, _ := codexCreds(auth)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Codex credentials into the request and executes it.
func (e *CodexExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("codex executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient, _ := helps.NewProxyAwareHTTPClientWithResolution(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *CodexExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return e.executeCompact(ctx, auth, req, opts)
	}
	timing := codexUpstreamTiming{endpoint: "responses", model: thinking.ParseSuffix(req.Model).ModelName, startedAt: time.Now()}
	if auth != nil {
		timing.authID = auth.ID
	}
	defer func() {
		finishCodexUpstreamTiming(ctx, timing)
	}()
	prepareStarted := time.Now()
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, e.cfg, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, false)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	body = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body, _ = sjson.SetBytes(body, "stream", true)
	body = stripCodexUnsupportedResponseFields(body, shouldPreserveCodexPreviousResponseID(ctx, auth, from, body))
	body, _ = sjson.DeleteBytes(body, "stream_options")
	body = normalizeCodexInstructions(body)
	body = maybeAttachImageGenerationTool(baseModel, body)

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, err := e.cacheHelper(ctx, from, url, req, body)
	if err != nil {
		return resp, err
	}
	applyCodexHeaders(httpReq, auth, apiKey, true, e.cfg)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
	httpClient, resolution := helps.NewProxyAwareHTTPClientWithResolution(ctx, e.cfg, auth, 0)
	applyCodexResolutionTiming(&timing, auth, resolution)
	timing.prepare = time.Since(prepareStarted)
	traceCtx := withCodexHTTPTrace(httpReq.Context(), &timing)
	httpStarted := time.Now()
	httpResp, err := httpClient.Do(httpReq.WithContext(traceCtx))
	timing.httpDo = time.Since(httpStarted)
	if err != nil {
		if retryResp, retryResolution, retryErr, retried := e.retryCodexHTTPRequestWithFailover(ctx, auth, httpReq, &timing, resolution, err, 0); retried {
			httpResp = retryResp
			resolution = retryResolution
			err = retryErr
		}
	}
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	timing.status = httpResp.StatusCode
	if codexShouldFailoverForStatus(httpResp.StatusCode) {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
		retryResp, retryResolution, retryErr, retried := e.retryCodexHTTPRequestWithFailover(ctx, auth, httpReq, &timing, resolution, nil, httpResp.StatusCode)
		if retried {
			httpResp = retryResp
			resolution = retryResolution
			err = retryErr
			if err != nil {
				helps.RecordAPIResponseError(ctx, e.cfg, err)
				return resp, err
			}
			timing.status = httpResp.StatusCode
		}
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		readStarted := time.Now()
		b, _ := io.ReadAll(httpResp.Body)
		timing.readBody += time.Since(readStarted)
		timing.bytesRead += len(b)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = newCodexStatusErr(httpResp.StatusCode, b)
		handleImageUnsupportedRegion(ctx, opts, auth, req.Model, resolution, err)
		return resp, err
	}
	readStarted := time.Now()
	data, err := io.ReadAll(httpResp.Body)
	timing.readBody = time.Since(readStarted)
	timing.bytesRead = len(data)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)

	lines := bytes.Split(data, []byte("\n"))
	outputItemsByIndex := make(map[int64][]byte)
	var outputItemsFallback [][]byte
	for _, line := range lines {
		if !bytes.HasPrefix(line, dataTag) {
			continue
		}

		eventData := bytes.TrimSpace(line[5:])
		eventType := gjson.GetBytes(eventData, "type").String()
		if body, status, ok := codexResponsesEventErrorBody(eventData); ok {
			err = newCodexStatusErr(status, body)
			handleImageUnsupportedRegion(ctx, opts, auth, req.Model, resolution, err)
			return resp, err
		}

		if eventType == "response.output_item.done" {
			itemResult := gjson.GetBytes(eventData, "item")
			if !itemResult.Exists() || itemResult.Type != gjson.JSON {
				continue
			}
			outputIndexResult := gjson.GetBytes(eventData, "output_index")
			if outputIndexResult.Exists() {
				outputItemsByIndex[outputIndexResult.Int()] = []byte(itemResult.Raw)
			} else {
				outputItemsFallback = append(outputItemsFallback, []byte(itemResult.Raw))
			}
			continue
		}

		if eventType != "response.completed" {
			continue
		}
		timing.streamCompleted = true

		if detail, ok := helps.ParseCodexUsage(eventData); ok {
			reporter.Publish(ctx, detail)
		}

		completedData := eventData
		outputResult := gjson.GetBytes(completedData, "response.output")
		shouldPatchOutput := (!outputResult.Exists() || !outputResult.IsArray() || len(outputResult.Array()) == 0) && (len(outputItemsByIndex) > 0 || len(outputItemsFallback) > 0)
		if shouldPatchOutput {
			completedDataPatched := completedData
			completedDataPatched, _ = sjson.SetRawBytes(completedDataPatched, "response.output", []byte(`[]`))

			indexes := make([]int64, 0, len(outputItemsByIndex))
			for idx := range outputItemsByIndex {
				indexes = append(indexes, idx)
			}
			sort.Slice(indexes, func(i, j int) bool {
				return indexes[i] < indexes[j]
			})
			for _, idx := range indexes {
				completedDataPatched, _ = sjson.SetRawBytes(completedDataPatched, "response.output.-1", outputItemsByIndex[idx])
			}
			for _, item := range outputItemsFallback {
				completedDataPatched, _ = sjson.SetRawBytes(completedDataPatched, "response.output.-1", item)
			}
			completedData = completedDataPatched
		}

		var param any
		translateStarted := time.Now()
		out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, originalPayload, body, completedData, &param)
		timing.translate += time.Since(translateStarted)
		proxypool.DefaultCodexFailoverManager().MarkSuccess(authID, codexResolutionMode(auth, resolution))
		resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
		return resp, nil
	}
	err = statusErr{code: 408, msg: "stream error: stream disconnected before completion: stream closed before response.completed"}
	return resp, err
}

func (e *CodexExecutor) executeCompact(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	timing := codexUpstreamTiming{endpoint: "responses/compact", model: thinking.ParseSuffix(req.Model).ModelName, startedAt: time.Now()}
	if auth != nil {
		timing.authID = auth.ID
	}
	defer func() {
		finishCodexUpstreamTiming(ctx, timing)
	}()
	prepareStarted := time.Now()
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, e.cfg, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai-response")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, false)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	body = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body, _ = sjson.DeleteBytes(body, "stream")
	if compactRequestHasInlineInput(body) {
		body, _ = sjson.DeleteBytes(body, "previous_response_id")
	}
	body = normalizeCodexInstructions(body)

	url := strings.TrimSuffix(baseURL, "/") + "/responses/compact"
	httpReq, err := e.cacheHelper(ctx, from, url, req, body)
	if err != nil {
		return resp, err
	}
	applyCodexHeaders(httpReq, auth, apiKey, false, e.cfg)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
	httpClient, resolution := helps.NewProxyAwareHTTPClientWithResolution(ctx, e.cfg, auth, 0)
	applyCodexResolutionTiming(&timing, auth, resolution)
	timing.prepare = time.Since(prepareStarted)
	traceCtx := withCodexHTTPTrace(httpReq.Context(), &timing)
	httpStarted := time.Now()
	httpResp, err := httpClient.Do(httpReq.WithContext(traceCtx))
	timing.httpDo = time.Since(httpStarted)
	if err != nil {
		if retryResp, retryResolution, retryErr, retried := e.retryCodexHTTPRequestWithFailover(ctx, auth, httpReq, &timing, resolution, err, 0); retried {
			httpResp = retryResp
			resolution = retryResolution
			err = retryErr
		}
	}
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	timing.status = httpResp.StatusCode
	if codexShouldFailoverForStatus(httpResp.StatusCode) {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
		retryResp, retryResolution, retryErr, retried := e.retryCodexHTTPRequestWithFailover(ctx, auth, httpReq, &timing, resolution, nil, httpResp.StatusCode)
		if retried {
			httpResp = retryResp
			resolution = retryResolution
			err = retryErr
			if err != nil {
				helps.RecordAPIResponseError(ctx, e.cfg, err)
				return resp, err
			}
			timing.status = httpResp.StatusCode
		}
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		readStarted := time.Now()
		b, _ := io.ReadAll(httpResp.Body)
		timing.readBody += time.Since(readStarted)
		timing.bytesRead += len(b)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = newCodexStatusErr(httpResp.StatusCode, b)
		return resp, err
	}
	readStarted := time.Now()
	data, err := io.ReadAll(httpResp.Body)
	timing.readBody = time.Since(readStarted)
	timing.bytesRead = len(data)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(data))
	reporter.EnsurePublished(ctx)
	timing.streamCompleted = true
	var param any
	translateStarted := time.Now()
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, originalPayload, body, data, &param)
	timing.translate = time.Since(translateStarted)
	proxypool.DefaultCodexFailoverManager().MarkSuccess(authID, codexResolutionMode(auth, resolution))
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

func compactRequestHasInlineInput(body []byte) bool {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return false
	}
	switch input.Type {
	case gjson.String:
		return strings.TrimSpace(input.String()) != ""
	case gjson.JSON:
		if input.IsArray() {
			return len(input.Array()) > 0
		}
		return strings.TrimSpace(input.Raw) != "" && input.Raw != "null"
	default:
		return strings.TrimSpace(input.Raw) != "" && input.Raw != "null"
	}
}

func (e *CodexExecutor) retryCodexHTTPRequestWithFailover(ctx context.Context, auth *cliproxyauth.Auth, httpReq *http.Request, timing *codexUpstreamTiming, resolution proxypool.Resolution, requestErr error, statusCode int) (*http.Response, proxypool.Resolution, error, bool) {
	if auth == nil || timing == nil {
		return nil, resolution, requestErr, false
	}
	if requestErr != nil && !codexShouldFailoverForError(requestErr) {
		return nil, resolution, requestErr, false
	}
	if requestErr == nil && !codexShouldFailoverForStatus(statusCode) {
		return nil, resolution, requestErr, false
	}
	if !advanceCodexFailoverState(e.cfg, auth, resolution, codexFailoverReason(requestErr, statusCode)) {
		return nil, resolution, requestErr, false
	}

	retryReq, errClone := cloneHTTPRequestForRetry(httpReq)
	if errClone != nil {
		return nil, resolution, requestErr, false
	}
	httpClient, retryResolution := helps.NewProxyAwareHTTPClientWithResolution(ctx, e.cfg, auth, 0)
	applyCodexResolutionTiming(timing, auth, retryResolution)
	traceCtx := withCodexHTTPTrace(retryReq.Context(), timing)
	httpStarted := time.Now()
	retryResp, retryErr := httpClient.Do(retryReq.WithContext(traceCtx))
	timing.httpDo += time.Since(httpStarted)
	return retryResp, retryResolution, retryErr, true
}

func cloneHTTPRequestForRetry(req *http.Request) (*http.Request, error) {
	if req == nil {
		return nil, nil
	}
	cloned := req.Clone(req.Context())
	if req.Body == nil {
		return cloned, nil
	}
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		cloned.Body = body
		return cloned, nil
	}
	data, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(data))
	req.ContentLength = int64(len(data))
	cloned.Body = io.NopCloser(bytes.NewReader(data))
	cloned.ContentLength = int64(len(data))
	return cloned, nil
}

func advanceCodexFailoverState(cfg *config.Config, auth *cliproxyauth.Auth, resolution proxypool.Resolution, reason string) bool {
	if auth == nil {
		return false
	}
	now := time.Now()
	reason = strings.TrimSpace(reason)
	switch codexResolutionMode(auth, resolution) {
	case proxypool.CodexFailoverModeDirectV4:
		if lease := proxypool.DefaultCodexFailoverManager().CurrentIPv6Lease(auth); strings.TrimSpace(lease.URL) != "" {
			proxypool.DefaultCodexFailoverManager().PreferDirectV6WithReason(cfg, auth.ID, reason, now)
			return true
		}
	case proxypool.CodexFailoverModeDirectV6:
		if shouldEscalateDirectV6FailureToProxy(reason) {
			proxypool.DefaultCodexFailoverManager().PreferProxyPoolWithReason(cfg, auth.ID, reason, now)
			return true
		}
		if proxypool.DefaultCodexFailoverManager().RotateToNextIPv6Lease(cfg, auth, now) {
			proxypool.DefaultCodexFailoverManager().NoteReason(auth.ID, reason)
			return true
		}
		proxypool.DefaultCodexFailoverManager().PreferProxyPoolWithReason(cfg, auth.ID, reason, now)
		return true
	}
	return false
}

func shouldEscalateDirectV6FailureToProxy(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "network-unreachable", "no-route-to-host", "dns-no-such-host", "dns-server-misbehaving":
		return true
	default:
		return false
	}
}

func codexFailoverReason(requestErr error, statusCode int) string {
	if requestErr != nil {
		text := strings.ToLower(strings.TrimSpace(requestErr.Error()))
		switch {
		case strings.Contains(text, "server misbehaving"):
			return "dns-server-misbehaving"
		case strings.Contains(text, "no such host"):
			return "dns-no-such-host"
		case strings.Contains(text, "network is unreachable"):
			return "network-unreachable"
		case strings.Contains(text, "no route to host"):
			return "no-route-to-host"
		case strings.Contains(text, "connection reset"):
			return "connection-reset"
		case strings.Contains(text, "broken pipe"):
			return "broken-pipe"
		case strings.Contains(text, "tls handshake"):
			return "tls-handshake"
		case strings.Contains(text, "eof"):
			return "eof"
		case strings.Contains(text, "timeout"):
			return "connect-timeout"
		default:
			return "transport-error"
		}
	}
	switch statusCode {
	case http.StatusBadGateway:
		return "http-502"
	case http.StatusServiceUnavailable:
		return "http-503"
	default:
		return ""
	}
}

func codexResolutionMode(auth *cliproxyauth.Auth, resolution proxypool.Resolution) string {
	switch strings.TrimSpace(resolution.Source) {
	case "direct-v6-sticky":
		return proxypool.CodexFailoverModeDirectV6
	case "proxy-pool-fallback":
		return proxypool.CodexFailoverModeProxy
	}
	if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return proxypool.CodexFailoverModeDirectV4
	}
	return ""
}

func codexShouldFailoverForStatus(statusCode int) bool {
	return statusCode == http.StatusBadGateway || statusCode == http.StatusServiceUnavailable
}

func codexShouldFailoverForError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	text := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(text, "timeout") ||
		strings.Contains(text, "no such host") ||
		strings.Contains(text, "server misbehaving") ||
		strings.Contains(text, "network is unreachable") ||
		strings.Contains(text, "no route to host") ||
		strings.Contains(text, "connection reset") ||
		strings.Contains(text, "broken pipe") ||
		strings.Contains(text, "tls handshake") ||
		strings.Contains(text, "eof")
}

func (e *CodexExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}
	timing := codexUpstreamTiming{endpoint: "responses_stream", model: thinking.ParseSuffix(req.Model).ModelName, startedAt: time.Now()}
	if auth != nil {
		timing.authID = auth.ID
	}
	prepareStarted := time.Now()
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, e.cfg, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	body = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body = stripCodexUnsupportedResponseFields(body, shouldPreserveCodexPreviousResponseID(ctx, auth, from, body))
	body, _ = sjson.DeleteBytes(body, "stream_options")
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body = normalizeCodexInstructions(body)
	body = maybeAttachImageGenerationTool(baseModel, body)

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, err := e.cacheHelper(ctx, from, url, req, body)
	if err != nil {
		return nil, err
	}
	applyCodexHeaders(httpReq, auth, apiKey, true, e.cfg)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient, resolution := helps.NewProxyAwareHTTPClientWithResolution(ctx, e.cfg, auth, 0)
	timing.proxySource = resolution.Source
	timing.proxyPool = resolution.ProxyPool
	timing.proxyName = resolution.ProxyName
	timing.proxyURL = resolution.ProxyURL
	timing.proxyFallback = resolution.FallbackToDirect
	timing.prepare = time.Since(prepareStarted)
	traceCtx := withCodexHTTPTrace(httpReq.Context(), &timing)
	httpStarted := time.Now()
	httpResp, err := httpClient.Do(httpReq.WithContext(traceCtx))
	timing.httpDo = time.Since(httpStarted)
	if err != nil {
		if retryResp, retryResolution, retryErr, retried := e.retryCodexHTTPRequestWithFailover(ctx, auth, httpReq, &timing, resolution, err, 0); retried {
			httpResp = retryResp
			resolution = retryResolution
			err = retryErr
		}
	}
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		timing.streamErrText = err.Error()
		finishCodexUpstreamTiming(ctx, timing)
		return nil, err
	}
	timing.status = httpResp.StatusCode
	if codexShouldFailoverForStatus(httpResp.StatusCode) {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
		retryResp, retryResolution, retryErr, retried := e.retryCodexHTTPRequestWithFailover(ctx, auth, httpReq, &timing, resolution, nil, httpResp.StatusCode)
		if retried {
			httpResp = retryResp
			resolution = retryResolution
			err = retryErr
			if err != nil {
				helps.RecordAPIResponseError(ctx, e.cfg, err)
				timing.streamErrText = err.Error()
				finishCodexUpstreamTiming(ctx, timing)
				return nil, err
			}
			timing.status = httpResp.StatusCode
		}
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		readStarted := time.Now()
		data, readErr := io.ReadAll(httpResp.Body)
		timing.readBody += time.Since(readStarted)
		timing.bytesRead += len(data)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
		if readErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, readErr)
			timing.streamErrText = readErr.Error()
			finishCodexUpstreamTiming(ctx, timing)
			return nil, readErr
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		err = newCodexStatusErr(httpResp.StatusCode, data)
		handleImageUnsupportedRegion(ctx, opts, auth, req.Model, resolution, err)
		timing.streamErrText = err.Error()
		finishCodexUpstreamTiming(ctx, timing)
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor: close response body error: %v", errClose)
			}
			finishCodexUpstreamTiming(ctx, timing)
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800) // 50MB
		var param any
		sendChunk := func(chunk cliproxyexecutor.StreamChunk) bool {
			if ctx == nil {
				out <- chunk
				return true
			}
			select {
			case out <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for {
			scanStarted := time.Now()
			ok := scanner.Scan()
			timing.readBody += time.Since(scanStarted)
			if !ok {
				break
			}
			timing.streamLines++
			line := scanner.Bytes()
			timing.bytesRead += len(line)
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)

			if bytes.HasPrefix(line, dataTag) {
				data := bytes.TrimSpace(line[5:])
				if body, status, ok := codexResponsesEventErrorBody(data); ok {
					eventErr := newCodexStatusErr(status, body)
					if bodyCyber, okCyber := codexResponsesEventCyberPolicyErrorBody(data); okCyber {
						eventErr = newCodexCyberPolicyStatusErr(bodyCyber)
					}
					handleImageUnsupportedRegion(ctx, opts, auth, req.Model, resolution, eventErr)
					helps.RecordAPIResponseError(ctx, e.cfg, eventErr)
					reporter.PublishFailureWithError(ctx, eventErr)
					timing.streamErrText = eventErr.Error()
					_ = sendChunk(cliproxyexecutor.StreamChunk{Err: eventErr})
					return
				}
				eventType := gjson.GetBytes(data, "type").String()
				if eventType == "response.completed" {
					timing.streamCompleted = true
					if detail, ok := helps.ParseCodexUsage(data); ok {
						reporter.Publish(ctx, detail)
					}
					proxypool.DefaultCodexFailoverManager().MarkSuccess(authID, codexResolutionMode(auth, resolution))
				}
			}

			translateStarted := time.Now()
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, originalPayload, body, bytes.Clone(line), &param)
			timing.translate += time.Since(translateStarted)
			timing.streamChunks += len(chunks)
			for i := range chunks {
				if !sendChunk(cliproxyexecutor.StreamChunk{Payload: chunks[i]}) {
					return
				}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailureWithError(ctx, errScan)
			timing.streamErrText = errScan.Error()
			_ = sendChunk(cliproxyexecutor.StreamChunk{Err: errScan})
			return
		}
		if !timing.streamCompleted {
			errIncomplete := statusErr{code: http.StatusRequestTimeout, msg: "stream error: stream disconnected before completion: stream closed before response.completed"}
			helps.RecordAPIResponseError(ctx, e.cfg, errIncomplete)
			reporter.PublishFailureWithError(ctx, errIncomplete)
			timing.streamErrText = errIncomplete.Error()
			_ = sendChunk(cliproxyexecutor.StreamChunk{Err: errIncomplete})
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func codexHTTPStreamEventHasUserContent(eventType string, data []byte) bool {
	switch {
	case strings.HasPrefix(eventType, "response.output_text"):
		return strings.TrimSpace(gjson.GetBytes(data, "delta").String()) != ""
	case strings.HasPrefix(eventType, "response.function_call_arguments"):
		return strings.TrimSpace(gjson.GetBytes(data, "delta").String()) != ""
	case eventType == "response.output_item.done":
		return codexHTTPResponseOutputHasUserContent(gjson.GetBytes(data, "item"))
	case eventType == "response.completed":
		for _, item := range gjson.GetBytes(data, "response.output").Array() {
			if codexHTTPResponseOutputHasUserContent(item) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func codexHTTPResponseOutputHasUserContent(item gjson.Result) bool {
	if !item.Exists() {
		return false
	}
	switch item.Get("type").String() {
	case "message":
		for _, content := range item.Get("content").Array() {
			if strings.TrimSpace(content.Get("text").String()) != "" {
				return true
			}
		}
		return false
	case "function_call":
		return strings.TrimSpace(item.Get("call_id").String()) != "" ||
			strings.TrimSpace(item.Get("name").String()) != "" ||
			strings.TrimSpace(item.Get("arguments").String()) != ""
	default:
		return strings.TrimSpace(item.Get("type").String()) != ""
	}
}

func (e *CodexExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	body, err := thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	body, _ = sjson.SetBytes(body, "model", baseModel)
	body = stripCodexUnsupportedResponseFields(body, shouldPreserveCodexPreviousResponseID(ctx, auth, from, body))
	body, _ = sjson.DeleteBytes(body, "stream_options")
	body, _ = sjson.SetBytes(body, "stream", false)
	body = normalizeCodexInstructions(body)

	enc, err := tokenizerForCodexModel(baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex executor: tokenizer init failed: %w", err)
	}

	count, err := countCodexInputTokens(enc, body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex executor: token counting failed: %w", err)
	}

	usageJSON := fmt.Sprintf(`{"response":{"usage":{"input_tokens":%d,"output_tokens":0,"total_tokens":%d}}}`, count, count)
	translated := sdktranslator.TranslateTokenCount(ctx, to, from, count, []byte(usageJSON))
	return cliproxyexecutor.Response{Payload: translated}, nil
}

func tokenizerForCodexModel(model string) (tokenizer.Codec, error) {
	sanitized := strings.ToLower(strings.TrimSpace(model))
	switch {
	case sanitized == "":
		return tokenizer.Get(tokenizer.Cl100kBase)
	case strings.HasPrefix(sanitized, "gpt-5"):
		return tokenizer.ForModel(tokenizer.GPT5)
	case strings.HasPrefix(sanitized, "gpt-4.1"):
		return tokenizer.ForModel(tokenizer.GPT41)
	case strings.HasPrefix(sanitized, "gpt-4o"):
		return tokenizer.ForModel(tokenizer.GPT4o)
	case strings.HasPrefix(sanitized, "gpt-4"):
		return tokenizer.ForModel(tokenizer.GPT4)
	case strings.HasPrefix(sanitized, "gpt-3.5"), strings.HasPrefix(sanitized, "gpt-3"):
		return tokenizer.ForModel(tokenizer.GPT35Turbo)
	default:
		return tokenizer.Get(tokenizer.Cl100kBase)
	}
}

func countCodexInputTokens(enc tokenizer.Codec, body []byte) (int64, error) {
	if enc == nil {
		return 0, fmt.Errorf("encoder is nil")
	}
	if len(body) == 0 {
		return 0, nil
	}

	root := gjson.ParseBytes(body)
	var segments []string

	if inst := strings.TrimSpace(root.Get("instructions").String()); inst != "" {
		segments = append(segments, inst)
	}

	inputItems := root.Get("input")
	if inputItems.IsArray() {
		arr := inputItems.Array()
		for i := range arr {
			item := arr[i]
			switch item.Get("type").String() {
			case "message":
				content := item.Get("content")
				if content.IsArray() {
					parts := content.Array()
					for j := range parts {
						part := parts[j]
						if text := strings.TrimSpace(part.Get("text").String()); text != "" {
							segments = append(segments, text)
						}
					}
				}
			case "function_call":
				if name := strings.TrimSpace(item.Get("name").String()); name != "" {
					segments = append(segments, name)
				}
				if args := strings.TrimSpace(item.Get("arguments").String()); args != "" {
					segments = append(segments, args)
				}
			case "function_call_output":
				if out := strings.TrimSpace(item.Get("output").String()); out != "" {
					segments = append(segments, out)
				}
			default:
				if text := strings.TrimSpace(item.Get("text").String()); text != "" {
					segments = append(segments, text)
				}
			}
		}
	}

	tools := root.Get("tools")
	if tools.IsArray() {
		tarr := tools.Array()
		for i := range tarr {
			tool := tarr[i]
			if name := strings.TrimSpace(tool.Get("name").String()); name != "" {
				segments = append(segments, name)
			}
			if desc := strings.TrimSpace(tool.Get("description").String()); desc != "" {
				segments = append(segments, desc)
			}
			if params := tool.Get("parameters"); params.Exists() {
				val := params.Raw
				if params.Type == gjson.String {
					val = params.String()
				}
				if trimmed := strings.TrimSpace(val); trimmed != "" {
					segments = append(segments, trimmed)
				}
			}
		}
	}

	textFormat := root.Get("text.format")
	if textFormat.Exists() {
		if name := strings.TrimSpace(textFormat.Get("name").String()); name != "" {
			segments = append(segments, name)
		}
		if schema := textFormat.Get("schema"); schema.Exists() {
			val := schema.Raw
			if schema.Type == gjson.String {
				val = schema.String()
			}
			if trimmed := strings.TrimSpace(val); trimmed != "" {
				segments = append(segments, trimmed)
			}
		}
	}

	text := strings.Join(segments, "\n")
	if text == "" {
		return 0, nil
	}

	count, err := enc.Count(text)
	if err != nil {
		return 0, err
	}
	return int64(count), nil
}

func (e *CodexExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("codex executor: refresh called")
	if auth == nil {
		return nil, statusErr{code: 500, msg: "codex executor: auth is nil"}
	}
	var refreshToken string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && v != "" {
			refreshToken = v
		}
	}
	if refreshToken == "" {
		return auth, nil
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	now := time.Now()
	accessToken, jwtPlan, refreshed, err := e.codexAccessTokenForProbe(ctx, auth, refreshToken, now)
	if err != nil {
		return nil, err
	}

	// Resolve the authoritative plan_type by probing /wham/usage with the
	// current access token. The JWT's chatgpt_plan_type is a cached
	// snapshot (can lag by hours); /wham/usage is live. On probe failure we
	// leave existing plan_type untouched; ApplyPlanTypeRefreshDecision will not
	// mutate Disabled or Attributes without an authoritative reading.
	if refreshed {
		log.Debugf("codex executor: refreshed access token for auth %s before usage probe", auth.ID)
	}
	// Multi-path probe: walk every pool entry (shuffled) and stop as soon
	// as one reports a paid plan. Falls back to direct egress last. This
	// masks OpenAI's per-edge plan_type cache disagreement (different
	// proxy IPs hit different regions, and a newly-upgraded account's
	// plus status propagates unevenly). We pin the auth to the first
	// entry that observed a paid plan, so the real dispatch path later
	// sees the same node and OpenAI's cache decision is self-consistent.
	// Header note: Chatgpt-Account-Id has no observable effect on the
	// response (verified 2026-04-20 across 5 egress paths); we omit it.
	realPlan, boundEntry, supportedModels, fiveHourQuota, weeklyQuota, probeOK, probeErr := helps.ProbeCodexPlanAcrossPool(ctx, e.cfg, auth, accessToken)
	if probeErr != nil {
		return nil, probeErr
	}
	if !probeOK {
		log.Warnf("codex executor: /wham/usage multi-path probe for auth %s failed on every candidate", auth.ID)
	}
	cliproxyauth.ApplyPlanTypeRefreshDecision(auth, jwtPlan, realPlan, probeOK, now)
	if probeOK {
		applyCodexSupportedModels(auth, supportedModels, now)
		applyCodexFiveHourQuotaMetadata(auth, fiveHourQuota, now)
		applyCodexWeeklyQuotaMetadata(auth, weeklyQuota, now)
		syncCodexProbeRoutingState(auth, realPlan, boundEntry)
	}
	if auth.Disabled && strings.HasPrefix(auth.StatusMessage, "codex_downgrade_detected: ") {
		log.Warnf("codex executor: auth %s disabled by downgrade detection: %s", auth.ID, auth.StatusMessage)
	}
	return auth, nil
}

func (e *CodexExecutor) codexAccessTokenForProbe(ctx context.Context, auth *cliproxyauth.Auth, refreshToken string, now time.Time) (accessToken string, jwtPlan string, refreshed bool, err error) {
	if auth == nil {
		return "", "", false, statusErr{code: 500, msg: "codex executor: auth is nil"}
	}
	accessToken = strings.TrimSpace(stringFromAny(auth.Metadata["access_token"]))
	if idToken := strings.TrimSpace(stringFromAny(auth.Metadata["id_token"])); idToken != "" {
		if claims, jwtErr := codexauth.ParseJWTToken(idToken); jwtErr == nil {
			jwtPlan = strings.ToLower(strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType))
		}
	}
	forceTokenRefresh := boolFromAny(auth.Metadata[cliproxyauth.MetadataCodexForceTokenRefreshKey])
	delete(auth.Metadata, cliproxyauth.MetadataCodexForceTokenRefreshKey)
	if accessToken != "" && !forceTokenRefresh {
		if expiry, ok := auth.ExpirationTime(); ok && expiry.After(now.Add(codexUsageProbeRefreshLead)) {
			return accessToken, jwtPlan, false, nil
		}
	}

	svc := codexauth.NewCodexAuth(e.cfg)
	td, refreshErr := svc.RefreshTokensWithRetry(ctx, refreshToken, 3)
	if refreshErr != nil {
		return "", jwtPlan, false, refreshErr
	}
	storage, _ := auth.Storage.(*codexauth.CodexTokenStorage)
	applyRefreshedCodexTokenState(auth, storage, td, now)
	if claims, jwtErr := codexauth.ParseJWTToken(td.IDToken); jwtErr == nil {
		jwtPlan = strings.ToLower(strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType))
	} else {
		log.Warnf("codex executor: parse id_token JWT for auth %s failed: %v", auth.ID, jwtErr)
	}
	return td.AccessToken, jwtPlan, true, nil
}

func applyCodexSupportedModels(auth *cliproxyauth.Auth, models []string, now time.Time) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	clearSupportedModels := func() {
		delete(auth.Attributes, "supported_models")
		auth.Attributes["supported_models_source"] = "codex_entitlements"
		auth.Attributes["supported_models_updated"] = now.Format(time.RFC3339)
	}
	if len(models) == 0 {
		clearSupportedModels()
		return
	}
	seen := make(map[string]struct{}, len(models))
	clean := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		clean = append(clean, model)
	}
	if len(clean) == 0 {
		clearSupportedModels()
		return
	}
	sort.Strings(clean)
	auth.Attributes["supported_models"] = strings.Join(clean, ",")
	auth.Attributes["supported_models_source"] = "codex_entitlements"
	auth.Attributes["supported_models_updated"] = now.Format(time.RFC3339)
}

const codexUsageProbeRefreshLead = 2 * time.Minute

func applyCodexFiveHourQuotaMetadata(auth *cliproxyauth.Auth, quota *codexauth.WhamQuotaWindow, now time.Time) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	if quota == nil {
		delete(auth.Metadata, cliproxyauth.MetadataCodexFiveHourQuotaRemainingRatioKey)
		delete(auth.Metadata, cliproxyauth.MetadataCodexFiveHourQuotaResetAtKey)
		delete(auth.Metadata, cliproxyauth.MetadataCodexFiveHourQuotaLimitKey)
		delete(auth.Metadata, cliproxyauth.MetadataCodexFiveHourQuotaRemainingKey)
		auth.Metadata[cliproxyauth.MetadataCodexFiveHourQuotaUpdatedAtKey] = now.UTC().Format(time.RFC3339)
		return
	}
	auth.Metadata[cliproxyauth.MetadataCodexFiveHourQuotaRemainingRatioKey] = quota.RemainingRatio
	auth.Metadata[cliproxyauth.MetadataCodexFiveHourQuotaUpdatedAtKey] = now.UTC().Format(time.RFC3339)
	if quota.Limit > 0 {
		auth.Metadata[cliproxyauth.MetadataCodexFiveHourQuotaLimitKey] = quota.Limit
	} else {
		delete(auth.Metadata, cliproxyauth.MetadataCodexFiveHourQuotaLimitKey)
	}
	if quota.Remaining > 0 {
		auth.Metadata[cliproxyauth.MetadataCodexFiveHourQuotaRemainingKey] = quota.Remaining
	} else {
		delete(auth.Metadata, cliproxyauth.MetadataCodexFiveHourQuotaRemainingKey)
	}
	if !quota.ResetAt.IsZero() {
		auth.Metadata[cliproxyauth.MetadataCodexFiveHourQuotaResetAtKey] = quota.ResetAt.UTC().Format(time.RFC3339)
	} else {
		delete(auth.Metadata, cliproxyauth.MetadataCodexFiveHourQuotaResetAtKey)
	}
}

func applyCodexWeeklyQuotaMetadata(auth *cliproxyauth.Auth, quota *codexauth.WhamQuotaWindow, now time.Time) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	if quota == nil {
		delete(auth.Metadata, cliproxyauth.MetadataCodexWeeklyQuotaRemainingRatioKey)
		delete(auth.Metadata, cliproxyauth.MetadataCodexWeeklyQuotaResetAtKey)
		delete(auth.Metadata, cliproxyauth.MetadataCodexWeeklyQuotaLimitKey)
		delete(auth.Metadata, cliproxyauth.MetadataCodexWeeklyQuotaRemainingKey)
		auth.Metadata[cliproxyauth.MetadataCodexWeeklyQuotaUpdatedAtKey] = now.UTC().Format(time.RFC3339)
		return
	}
	auth.Metadata[cliproxyauth.MetadataCodexWeeklyQuotaRemainingRatioKey] = quota.RemainingRatio
	auth.Metadata[cliproxyauth.MetadataCodexWeeklyQuotaUpdatedAtKey] = now.UTC().Format(time.RFC3339)
	if quota.Limit > 0 {
		auth.Metadata[cliproxyauth.MetadataCodexWeeklyQuotaLimitKey] = quota.Limit
	} else {
		delete(auth.Metadata, cliproxyauth.MetadataCodexWeeklyQuotaLimitKey)
	}
	if quota.Remaining > 0 {
		auth.Metadata[cliproxyauth.MetadataCodexWeeklyQuotaRemainingKey] = quota.Remaining
	} else {
		delete(auth.Metadata, cliproxyauth.MetadataCodexWeeklyQuotaRemainingKey)
	}
	if !quota.ResetAt.IsZero() {
		auth.Metadata[cliproxyauth.MetadataCodexWeeklyQuotaResetAtKey] = quota.ResetAt.UTC().Format(time.RFC3339)
	} else {
		delete(auth.Metadata, cliproxyauth.MetadataCodexWeeklyQuotaResetAtKey)
	}
}

func stringFromAny(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func boolFromAny(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	default:
		return false
	}
}
func applyRefreshedCodexTokenState(auth *cliproxyauth.Auth, storage *codexauth.CodexTokenStorage, td *codexauth.CodexTokenData, now time.Time) {
	if auth == nil || td == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["id_token"] = td.IDToken
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.AccountID != "" {
		auth.Metadata["account_id"] = td.AccountID
	}
	auth.Metadata["email"] = td.Email
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "codex"
	auth.Metadata["last_refresh"] = now.Format(time.RFC3339)
	if storage != nil {
		storage.IDToken = td.IDToken
		storage.AccessToken = td.AccessToken
		if td.RefreshToken != "" {
			storage.RefreshToken = td.RefreshToken
		}
		if td.AccountID != "" {
			storage.AccountID = td.AccountID
		}
		storage.LastRefresh = now.Format(time.RFC3339)
		storage.Email = td.Email
		storage.Expire = td.Expire
		storage.Type = "codex"
	}
}

func (e *CodexExecutor) cacheHelper(ctx context.Context, from sdktranslator.Format, url string, req cliproxyexecutor.Request, rawJSON []byte) (*http.Request, error) {
	var cache helps.CodexCache
	if from == "claude" {
		userIDResult := gjson.GetBytes(req.Payload, "metadata.user_id")
		if userIDResult.Exists() {
			key := fmt.Sprintf("%s-%s", req.Model, userIDResult.String())
			var ok bool
			if cache, ok = helps.GetCodexCache(key); !ok {
				cache = helps.CodexCache{
					ID:     uuid.New().String(),
					Expire: time.Now().Add(1 * time.Hour),
				}
				helps.SetCodexCache(key, cache)
			}
		}
	} else if from == "openai-response" {
		promptCacheKey := gjson.GetBytes(req.Payload, "prompt_cache_key")
		if promptCacheKey.Exists() {
			cache.ID = promptCacheKey.String()
		}
	} else if from == "openai" {
		if apiKey := strings.TrimSpace(helps.APIKeyFromContext(ctx)); apiKey != "" {
			cache.ID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:codex:prompt-cache:"+apiKey)).String()
		}
	}

	if cache.ID != "" {
		rawJSON, _ = sjson.SetBytes(rawJSON, "prompt_cache_key", cache.ID)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(rawJSON))
	if err != nil {
		return nil, err
	}
	if cache.ID != "" {
		httpReq.Header.Set("Session_id", cache.ID)
	}
	return httpReq, nil
}

func applyCodexHeaders(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, cfg *config.Config) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)

	var ginHeaders http.Header
	if ginCtx, ok := r.Context().Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header
	}

	if ginHeaders.Get("X-Codex-Beta-Features") != "" {
		r.Header.Set("X-Codex-Beta-Features", ginHeaders.Get("X-Codex-Beta-Features"))
	}
	misc.EnsureHeader(r.Header, ginHeaders, "Version", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Turn-Metadata", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Client-Request-Id", "")
	cfgUserAgent, _ := codexHeaderDefaults(cfg, auth)
	ensureHeaderWithConfigPrecedence(r.Header, ginHeaders, "User-Agent", cfgUserAgent, codexUserAgent)

	if strings.Contains(r.Header.Get("User-Agent"), "Mac OS") {
		misc.EnsureHeader(r.Header, ginHeaders, "Session_id", uuid.NewString())
	}

	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}
	r.Header.Set("Connection", "Keep-Alive")

	isAPIKey := false
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			isAPIKey = true
		}
	}
	if originator := strings.TrimSpace(ginHeaders.Get("Originator")); originator != "" {
		r.Header.Set("Originator", originator)
	} else if !isAPIKey {
		r.Header.Set("Originator", codexOriginator)
	}
	if !isAPIKey {
		if auth != nil && auth.Metadata != nil {
			if accountID, ok := auth.Metadata["account_id"].(string); ok {
				r.Header.Set("Chatgpt-Account-Id", accountID)
			}
		}
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs)
}

func newCodexStatusErr(statusCode int, body []byte) statusErr {
	errCode := statusCode
	if isCodexModelCapacityError(body) {
		errCode = http.StatusTooManyRequests
	} else if isCodexImageInputRateLimitError(body) {
		errCode = http.StatusTooManyRequests
	} else if codexErrorBodyIsUnsupportedRegion(body) {
		errCode = http.StatusInternalServerError
	}
	err := statusErr{code: errCode, msg: string(body)}
	if retryAfter := parseCodexRetryAfter(errCode, body, time.Now()); retryAfter != nil {
		err.retryAfter = retryAfter
	}
	if err.code == http.StatusTooManyRequests && err.retryAfter == nil && isCodexModelCapacityError(body) {
		fallback := 5 * time.Minute
		err.retryAfter = &fallback
	}
	return err
}

func newCodexCyberPolicyStatusErr(body []byte) statusErr {
	sanitized := []byte(`{"error":{"code":"cyber_policy","message":"request rejected by safety system","type":"invalid_request_error","metadata":{"cpa_reason":"cyber_policy"}}}`)
	return statusErr{code: http.StatusBadRequest, msg: string(sanitized)}
}

func codexResponsesEventErrorBody(eventData []byte) ([]byte, int, bool) {
	if len(eventData) == 0 {
		return nil, 0, false
	}
	eventType := strings.TrimSpace(gjson.GetBytes(eventData, "type").String())
	if eventType != "response.failed" && eventType != "response.error" {
		return nil, 0, false
	}
	errNode := gjson.GetBytes(eventData, "response.error")
	if !errNode.Exists() {
		errNode = gjson.GetBytes(eventData, "error")
	}
	if !errNode.Exists() || errNode.Type != gjson.JSON {
		return nil, 0, false
	}
	body := []byte(`{}`)
	body, _ = sjson.SetRawBytes(body, "error", []byte(errNode.Raw))
	return body, codexResponsesEventErrorStatus(errNode), true
}

func codexResponsesEventErrorStatus(errNode gjson.Result) int {
	code := strings.TrimSpace(errNode.Get("code").String())
	errType := strings.TrimSpace(errNode.Get("type").String())
	switch {
	case strings.EqualFold(code, "unsupported_country_region_territory"):
		return http.StatusInternalServerError
	case strings.EqualFold(code, "cyber_policy"):
		return http.StatusBadRequest
	case strings.EqualFold(errType, "request_forbidden"):
		return http.StatusInternalServerError
	case strings.EqualFold(errType, "invalid_request_error"):
		return http.StatusBadRequest
	case strings.EqualFold(errType, "usage_limit_reached"):
		return http.StatusTooManyRequests
	default:
		return http.StatusBadRequest
	}
}

func codexErrorBodyIsUnsupportedRegion(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	code := strings.TrimSpace(gjson.GetBytes(body, "error.code").String())
	errType := strings.TrimSpace(gjson.GetBytes(body, "error.type").String())
	return strings.EqualFold(code, "unsupported_country_region_territory") || strings.EqualFold(errType, "request_forbidden")
}

func handleImageUnsupportedRegion(ctx context.Context, opts cliproxyexecutor.Options, auth *cliproxyauth.Auth, model string, resolution proxypool.Resolution, err error) {
	if !imageGenerationRequestFromOptions(opts) || !codexErrorIsUnsupportedRegion(err) {
		return
	}
	authID := ""
	if auth != nil {
		authID = strings.TrimSpace(auth.ID)
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	errorCode := strings.TrimSpace(gjson.Get(errText, "error.code").String())
	errorType := strings.TrimSpace(gjson.Get(errText, "error.type").String())
	helps.LogWithRequestID(ctx).WithFields(log.Fields{
		"auth_id":    authID,
		"proxy_pool": strings.TrimSpace(resolution.ProxyPool),
		"proxy_name": strings.TrimSpace(resolution.ProxyName),
		"proxy_src":  strings.TrimSpace(resolution.Source),
		"status":     http.StatusInternalServerError,
		"error_code": errorCode,
		"error_type": errorType,
		"model":      strings.TrimSpace(model),
	}).Warn("codex image generation unsupported country/region; marking proxy for rebind")

	if strings.TrimSpace(resolution.ProxyPool) != "" && strings.TrimSpace(resolution.ProxyName) != "" {
		proxypool.DefaultHealthManager().ReportPassiveUnsupportedRegion(resolution.ProxyPool, resolution.ProxyName, proxypool.PassiveOutcome{
			StatusCode: http.StatusForbidden,
			Error:      strings.TrimSpace(errorCode),
			CheckedAt:  time.Now(),
		})
	}
	if auth != nil && strings.TrimSpace(cliproxyauth.BoundProxyEntry(auth)) == strings.TrimSpace(resolution.ProxyName) && strings.TrimSpace(resolution.ProxyName) != "" {
		cliproxyauth.SetBoundProxyEntry(auth, "")
	}
}

func imageGenerationRequestFromOptions(opts cliproxyexecutor.Options) bool {
	if len(opts.Metadata) == 0 {
		return false
	}
	value, ok := opts.Metadata[cliproxyexecutor.ImageGenerationRequestMetadataKey].(bool)
	return ok && value
}

func codexErrorIsUnsupportedRegion(err error) bool {
	if err == nil {
		return false
	}
	errText := err.Error()
	code := strings.TrimSpace(gjson.Get(errText, "error.code").String())
	errType := strings.TrimSpace(gjson.Get(errText, "error.type").String())
	return strings.EqualFold(code, "unsupported_country_region_territory") || strings.EqualFold(errType, "request_forbidden")
}

func codexResponsesEventCyberPolicyErrorBody(eventData []byte) ([]byte, bool) {
	body, _, ok := codexResponsesEventErrorBody(eventData)
	if !ok {
		return nil, false
	}
	code := strings.TrimSpace(gjson.GetBytes(body, "error.code").String())
	if strings.EqualFold(code, "cyber_policy") {
		return body, true
	}
	message := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.message").String()))
	if strings.Contains(message, "cyber_policy") {
		return body, true
	}
	return nil, false
}

func normalizeCodexInstructions(body []byte) []byte {
	instructions := gjson.GetBytes(body, "instructions")
	if !instructions.Exists() || instructions.Type == gjson.Null {
		body, _ = sjson.SetBytes(body, "instructions", "")
	}
	return body
}

func isCodexModelCapacityError(errorBody []byte) bool {
	if len(errorBody) == 0 {
		return false
	}
	candidates := []string{
		gjson.GetBytes(errorBody, "error.message").String(),
		gjson.GetBytes(errorBody, "message").String(),
		string(errorBody),
	}
	for _, candidate := range candidates {
		lower := strings.ToLower(strings.TrimSpace(candidate))
		if lower == "" {
			continue
		}
		if strings.Contains(lower, "selected model is at capacity") ||
			strings.Contains(lower, "model is at capacity. please try a different model") ||
			strings.Contains(lower, "requested model is currently unavailable") ||
			strings.Contains(lower, "current model is unavailable") ||
			(strings.Contains(lower, "model unavailable") && strings.Contains(lower, "switch model")) {
			return true
		}
	}
	return false
}

func parseCodexRetryAfter(statusCode int, errorBody []byte, now time.Time) *time.Duration {
	if statusCode != http.StatusTooManyRequests || len(errorBody) == 0 {
		return nil
	}
	if strings.TrimSpace(gjson.GetBytes(errorBody, "error.type").String()) != "usage_limit_reached" {
		if parsed := parseCodexRetryAfterPhrase(errorBody); parsed != nil {
			return parsed
		}
		if isCodexPlainRateLimitError(errorBody) {
			retryAfter := 60 * time.Second
			return &retryAfter
		}
		return nil
	}
	if resetsAt := gjson.GetBytes(errorBody, "error.resets_at").Int(); resetsAt > 0 {
		resetAtTime := time.Unix(resetsAt, 0)
		if resetAtTime.After(now) {
			retryAfter := resetAtTime.Sub(now)
			return &retryAfter
		}
	}
	if resetsInSeconds := gjson.GetBytes(errorBody, "error.resets_in_seconds").Int(); resetsInSeconds > 0 {
		retryAfter := time.Duration(resetsInSeconds) * time.Second
		return &retryAfter
	}
	return nil
}

func parseCodexRetryAfterPhrase(errorBody []byte) *time.Duration {
	for _, candidate := range codexErrorTextCandidates(errorBody) {
		matches := codexRetryAfterPhrasePattern.FindStringSubmatch(candidate)
		if len(matches) != 3 {
			continue
		}
		value, err := strconv.ParseFloat(matches[1], 64)
		if err != nil || value <= 0 {
			continue
		}
		unit := strings.ToLower(matches[2])
		switch {
		case strings.HasPrefix(unit, "ms") || strings.HasPrefix(unit, "millisecond"):
			retryAfter := time.Duration(value * float64(time.Millisecond))
			return &retryAfter
		default:
			retryAfter := time.Duration(value * float64(time.Second))
			return &retryAfter
		}
	}
	return nil
}

func isCodexPlainRateLimitError(errorBody []byte) bool {
	if len(errorBody) == 0 {
		return false
	}
	for _, candidate := range codexErrorTextCandidates(errorBody) {
		lower := strings.ToLower(strings.TrimSpace(candidate))
		if lower == "" {
			continue
		}
		if strings.Contains(lower, "rate limit exceeded") ||
			strings.Contains(lower, "too many requests") ||
			strings.Contains(lower, "requests rate limit") ||
			strings.Contains(lower, "rate limited") {
			return true
		}
	}
	return false
}

func isCodexImageInputRateLimitError(errorBody []byte) bool {
	if len(errorBody) == 0 {
		return false
	}
	code := strings.TrimSpace(gjson.GetBytes(errorBody, "error.code").String())
	for _, candidate := range codexErrorTextCandidates(errorBody) {
		lower := strings.ToLower(strings.TrimSpace(candidate))
		if lower == "" {
			continue
		}
		if strings.EqualFold(code, "rate_limit_exceeded") &&
			strings.Contains(lower, "gpt-image") &&
			strings.Contains(lower, "input-images per min") {
			return true
		}
		if strings.Contains(lower, "rate limit reached for gpt-image") &&
			strings.Contains(lower, "for limit gpt-image") &&
			strings.Contains(lower, "input-images per min") {
			return true
		}
	}
	return false
}

func codexErrorTextCandidates(errorBody []byte) []string {
	return []string{
		gjson.GetBytes(errorBody, "detail").String(),
		gjson.GetBytes(errorBody, "error.message").String(),
		gjson.GetBytes(errorBody, "message").String(),
		string(errorBody),
	}
}

func codexCreds(a *cliproxyauth.Auth) (apiKey, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		apiKey = a.Attributes["api_key"]
		baseURL = a.Attributes["base_url"]
	}
	if apiKey == "" && a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok {
			apiKey = v
		}
	}
	return
}

func (e *CodexExecutor) resolveCodexConfig(auth *cliproxyauth.Auth) *config.CodexKey {
	if auth == nil || e.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range e.cfg.CodexKey {
		entry := &e.cfg.CodexKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range e.cfg.CodexKey {
			entry := &e.cfg.CodexKey[i]
			if strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
				return entry
			}
		}
	}
	return nil
}
