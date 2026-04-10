// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements a Codex executor that uses the Responses API WebSocket transport.
package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/proxyrouting"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	apihandlers "github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/net/proxy"
)

const (
	codexResponsesWebsocketBetaHeaderValue = "responses_websockets=2026-02-06"
	codexResponsesWebsocketIdleTimeout     = 5 * time.Minute
	codexResponsesWebsocketHandshakeTO     = 30 * time.Second
)

// CodexWebsocketsExecutor executes Codex Responses requests using a WebSocket transport.
//
// It preserves the existing CodexExecutor HTTP implementation as a fallback for endpoints
// not available over WebSocket (e.g. /responses/compact) and for websocket upgrade failures.
type CodexWebsocketsExecutor struct {
	*CodexExecutor

	sessMu   sync.Mutex
	sessions map[string]*codexWebsocketSession
}

type codexWebsocketSession struct {
	sessionID string

	reqMu sync.Mutex

	lastRequest []byte

	connMu sync.Mutex
	conn   *websocket.Conn
	wsURL  string
	authID string

	writeMu sync.Mutex

	activeMu     sync.Mutex
	activeCh     chan codexWebsocketRead
	activeDone   <-chan struct{}
	activeCancel context.CancelFunc

	readerConn *websocket.Conn
}

func NewCodexWebsocketsExecutor(cfg *config.Config) *CodexWebsocketsExecutor {
	return &CodexWebsocketsExecutor{
		CodexExecutor: NewCodexExecutor(cfg),
		sessions:      make(map[string]*codexWebsocketSession),
	}
}

type codexWebsocketRead struct {
	conn    *websocket.Conn
	msgType int
	payload []byte
	err     error
}

type codexNonStreamAggregator struct {
	messageText   strings.Builder
	reasoningText strings.Builder
	itemsByID     map[string]map[string]any
	order         []string
	itemIDByCall  map[string]string
}

func (s *codexWebsocketSession) setActive(ch chan codexWebsocketRead) {
	if s == nil {
		return
	}
	s.activeMu.Lock()
	if s.activeCancel != nil {
		s.activeCancel()
		s.activeCancel = nil
		s.activeDone = nil
	}
	s.activeCh = ch
	if ch != nil {
		activeCtx, activeCancel := context.WithCancel(context.Background())
		s.activeDone = activeCtx.Done()
		s.activeCancel = activeCancel
	}
	s.activeMu.Unlock()
}

func (s *codexWebsocketSession) clearActive(ch chan codexWebsocketRead) {
	if s == nil {
		return
	}
	s.activeMu.Lock()
	if s.activeCh == ch {
		s.activeCh = nil
		if s.activeCancel != nil {
			s.activeCancel()
		}
		s.activeCancel = nil
		s.activeDone = nil
	}
	s.activeMu.Unlock()
}

func (s *codexWebsocketSession) writeMessage(conn *websocket.Conn, msgType int, payload []byte) error {
	if s == nil {
		return fmt.Errorf("codex websockets executor: session is nil")
	}
	if conn == nil {
		return fmt.Errorf("codex websockets executor: websocket conn is nil")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return conn.WriteMessage(msgType, payload)
}

func (s *codexWebsocketSession) configureConn(conn *websocket.Conn) {
	if s == nil || conn == nil {
		return
	}
	conn.SetPingHandler(func(appData string) error {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		// Reply pongs from the same write lock to avoid concurrent writes.
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(10*time.Second))
	})
}

func newCodexNonStreamAggregator() *codexNonStreamAggregator {
	return &codexNonStreamAggregator{
		itemsByID:    make(map[string]map[string]any),
		order:        make([]string, 0, 4),
		itemIDByCall: make(map[string]string),
	}
}

func (a *codexNonStreamAggregator) ensureItem(id string, itemType string) map[string]any {
	if a == nil {
		return nil
	}
	id = strings.TrimSpace(id)
	if id == "" {
		id = fmt.Sprintf("__idx_%d", len(a.order))
	}
	if item, ok := a.itemsByID[id]; ok {
		if itemType != "" && strings.TrimSpace(asString(item["type"])) == "" {
			item["type"] = itemType
		}
		if rawID := strings.TrimSpace(asString(item["id"])); rawID == "" && !strings.HasPrefix(id, "__idx_") {
			item["id"] = id
		}
		return item
	}

	item := map[string]any{}
	if itemType != "" {
		item["type"] = itemType
	}
	if !strings.HasPrefix(id, "__idx_") {
		item["id"] = id
	}
	a.itemsByID[id] = item
	a.order = append(a.order, id)
	return item
}

func (a *codexNonStreamAggregator) ingest(payload []byte) {
	if a == nil || len(bytes.TrimSpace(payload)) == 0 {
		return
	}
	root := gjson.ParseBytes(payload)
	switch root.Get("type").String() {
	case "response.output_text.delta":
		a.messageText.WriteString(root.Get("delta").String())
	case "response.reasoning_summary_text.delta":
		a.reasoningText.WriteString(root.Get("delta").String())
	case "response.output_item.added", "response.output_item.done":
		item := root.Get("item")
		if !item.Exists() || !item.IsObject() {
			return
		}
		itemID := strings.TrimSpace(item.Get("id").String())
		if itemID == "" {
			itemID = strings.TrimSpace(root.Get("item_id").String())
		}
		itemType := strings.TrimSpace(item.Get("type").String())
		dst := a.ensureItem(itemID, itemType)
		if dst == nil {
			return
		}
		mergeJSONObject(dst, item)
		if callID := strings.TrimSpace(item.Get("call_id").String()); callID != "" {
			a.itemIDByCall[callID] = itemID
		}
	case "response.function_call_arguments.delta":
		itemID := strings.TrimSpace(root.Get("item_id").String())
		if itemID == "" {
			if callID := strings.TrimSpace(root.Get("call_id").String()); callID != "" {
				itemID = a.itemIDByCall[callID]
			}
		}
		if itemID == "" {
			return
		}
		dst := a.ensureItem(itemID, "function_call")
		if dst == nil {
			return
		}
		existingArgs := asString(dst["arguments"])
		dst["arguments"] = existingArgs + root.Get("delta").String()
	case "response.function_call_arguments.done":
		itemID := strings.TrimSpace(root.Get("item_id").String())
		if itemID == "" {
			if callID := strings.TrimSpace(root.Get("call_id").String()); callID != "" {
				itemID = a.itemIDByCall[callID]
			}
		}
		if itemID == "" {
			return
		}
		dst := a.ensureItem(itemID, "function_call")
		if dst == nil {
			return
		}
		if args := root.Get("arguments"); args.Exists() {
			dst["arguments"] = args.String()
		}
	}
}

func (a *codexNonStreamAggregator) applyCompleted(payload []byte) []byte {
	if a == nil || len(bytes.TrimSpace(payload)) == 0 {
		return payload
	}

	responseNode := gjson.GetBytes(payload, "response")
	if !responseNode.Exists() || !responseNode.IsObject() {
		return payload
	}

	outputNode := responseNode.Get("output")
	if outputNode.Exists() && outputNode.IsArray() && len(outputNode.Array()) > 0 {
		return payload
	}

	output := a.materializeOutput()
	if len(output) == 0 {
		return payload
	}

	raw, err := json.Marshal(output)
	if err != nil {
		return payload
	}
	updated, err := sjson.SetRawBytes(payload, "response.output", raw)
	if err != nil {
		return payload
	}
	return updated
}

func (a *codexNonStreamAggregator) materializeOutput() []map[string]any {
	if a == nil {
		return nil
	}

	output := make([]map[string]any, 0, len(a.order)+2)

	message := strings.TrimSpace(a.messageText.String())
	reasoning := strings.TrimSpace(a.reasoningText.String())

	for _, id := range a.order {
		item := a.itemsByID[id]
		if item == nil {
			continue
		}
		cloned := cloneJSONObject(item)
		if strings.TrimSpace(asString(cloned["status"])) == "" {
			cloned["status"] = "completed"
		}
		switch strings.TrimSpace(asString(cloned["type"])) {
		case "message":
			if message != "" && !messageItemHasContent(cloned) {
				cloned["role"] = "assistant"
				cloned["content"] = []map[string]any{
					{
						"type": "output_text",
						"text": message,
					},
				}
			}
			if message != "" {
				message = ""
			}
		case "reasoning":
			if reasoning != "" && !reasoningItemHasSummary(cloned) {
				cloned["summary"] = []map[string]any{
					{
						"type": "summary_text",
						"text": reasoning,
					},
				}
			}
			if reasoning != "" {
				reasoning = ""
			}
		}
		output = append(output, cloned)
	}

	if message != "" {
		output = append(output, map[string]any{
			"type":   "message",
			"role":   "assistant",
			"status": "completed",
			"content": []map[string]any{
				{
					"type": "output_text",
					"text": message,
				},
			},
		})
	}

	if reasoning != "" {
		output = append(output, map[string]any{
			"type": "reasoning",
			"summary": []map[string]any{
				{
					"type": "summary_text",
					"text": reasoning,
				},
			},
		})
	}

	return output
}

func messageItemHasContent(item map[string]any) bool {
	content, ok := item["content"]
	if !ok || content == nil {
		return false
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return false
	}
	contentNode := gjson.ParseBytes(raw)
	if !contentNode.IsArray() {
		return false
	}
	for _, child := range contentNode.Array() {
		if child.Get("type").String() == "output_text" && strings.TrimSpace(child.Get("text").String()) != "" {
			return true
		}
	}
	return false
}

func reasoningItemHasSummary(item map[string]any) bool {
	summary, ok := item["summary"]
	if !ok || summary == nil {
		return false
	}
	raw, err := json.Marshal(summary)
	if err != nil {
		return false
	}
	summaryNode := gjson.ParseBytes(raw)
	if !summaryNode.IsArray() {
		return false
	}
	for _, child := range summaryNode.Array() {
		if child.Get("type").String() == "summary_text" && strings.TrimSpace(child.Get("text").String()) != "" {
			return true
		}
	}
	return false
}

func mergeJSONObject(dst map[string]any, src gjson.Result) {
	if dst == nil || !src.Exists() || !src.IsObject() {
		return
	}
	src.ForEach(func(key, value gjson.Result) bool {
		k := strings.TrimSpace(key.String())
		if k == "" {
			return true
		}
		switch {
		case value.IsObject():
			child, _ := dst[k].(map[string]any)
			if child == nil {
				child = map[string]any{}
			}
			mergeJSONObject(child, value)
			dst[k] = child
		case value.IsArray():
			var arr any
			if err := json.Unmarshal([]byte(value.Raw), &arr); err == nil {
				dst[k] = arr
			}
		default:
			dst[k] = scalarValue(value)
		}
		return true
	})
}

func cloneJSONObject(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	out := make(map[string]any, len(src))
	for key, value := range src {
		out[key] = cloneJSONValue(value)
	}
	return out
}

func cloneJSONValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return cloneJSONObject(v)
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = cloneJSONValue(v[i])
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(v))
		for i := range v {
			out[i] = cloneJSONObject(v[i])
		}
		return out
	default:
		return v
	}
}

func scalarValue(value gjson.Result) any {
	switch value.Type {
	case gjson.String:
		return value.String()
	case gjson.Number:
		return value.Value()
	case gjson.True:
		return true
	case gjson.False:
		return false
	case gjson.Null:
		return nil
	default:
		return value.Value()
	}
}

func asString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		if v == nil {
			return ""
		}
		return fmt.Sprint(v)
	}
}

func (e *CodexWebsocketsExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Alt == "responses/compact" {
		return e.CodexExecutor.executeCompact(ctx, auth, req, opts)
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, e.cfg, auth)
	defer reporter.trackFailure(ctx, &err)

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

	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body, _ = sjson.SetBytes(body, "stream", true)
	body = stripCodexUnsupportedResponseFields(body, shouldPreserveCodexPreviousResponseID(ctx, auth, from, body))
	if !gjson.GetBytes(body, "instructions").Exists() {
		body, _ = sjson.SetBytes(body, "instructions", "")
	}

	httpURL := strings.TrimSuffix(baseURL, "/") + "/responses"
	wsURL, err := buildCodexResponsesWebsocketURL(httpURL)
	if err != nil {
		return resp, err
	}

	body, wsHeaders := applyCodexPromptCacheHeaders(from, req, body)
	wsHeaders = applyCodexWebsocketHeaders(ctx, wsHeaders, auth, apiKey, e.cfg)

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}

	executionSessionID := executionSessionIDFromOptions(opts)
	var sess *codexWebsocketSession
	if executionSessionID != "" {
		sess = e.getOrCreateSession(executionSessionID)
		sess.reqMu.Lock()
		defer sess.reqMu.Unlock()
	}

	if sess != nil && shouldEnrichCodexWebsocketBridgeFollowupRequest(ctx, executionSessionID) {
		body = enrichCodexWebsocketBridgeFollowupRequest(body, sess.lastRequest)
	}

	wsReqBody := buildCodexWebsocketRequestBody(body)
	if sess != nil {
		sess.lastRequest = bytes.Clone(body)
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       wsURL,
		Method:    "WEBSOCKET",
		Headers:   wsHeaders.Clone(),
		Body:      wsReqBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	conn, respHS, errDial := e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
	if respHS != nil {
		recordAPIResponseMetadata(ctx, e.cfg, respHS.StatusCode, respHS.Header.Clone())
	}
	if errDial != nil {
		bodyErr := websocketHandshakeBody(respHS)
		if len(bodyErr) > 0 {
			appendAPIResponseChunk(ctx, e.cfg, bodyErr)
		}
		if respHS != nil && respHS.StatusCode == http.StatusUpgradeRequired {
			return e.CodexExecutor.Execute(ctx, auth, req, opts)
		}
		if respHS != nil && respHS.StatusCode > 0 {
			return resp, newCodexStatusErr(respHS.StatusCode, bodyErr)
		}
		recordAPIResponseError(ctx, e.cfg, errDial)
		return resp, errDial
	}
	closeHTTPResponseBody(respHS, "codex websockets executor: close handshake response body error")
	if sess == nil {
		logCodexWebsocketConnected(executionSessionID, authID, wsURL)
		defer func() {
			reason := "completed"
			if err != nil {
				reason = "error"
			}
			logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, reason, err)
			if errClose := conn.Close(); errClose != nil {
				log.Errorf("codex websockets executor: close websocket error: %v", errClose)
			}
		}()
	}

	var readCh chan codexWebsocketRead
	if sess != nil {
		readCh = make(chan codexWebsocketRead, 4096)
		sess.setActive(readCh)
		defer sess.clearActive(readCh)
	}

	if errSend := writeCodexWebsocketMessage(sess, conn, wsReqBody); errSend != nil {
		if sess != nil {
			e.invalidateUpstreamConn(sess, conn, "send_error", errSend)

			// Retry once with a fresh websocket connection. This is mainly to handle
			// upstream closing the socket between sequential requests within the same
			// execution session.
			connRetry, _, errDialRetry := e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
			if errDialRetry == nil && connRetry != nil {
				wsReqBodyRetry := buildCodexWebsocketRequestBody(body)
				recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
					URL:       wsURL,
					Method:    "WEBSOCKET",
					Headers:   wsHeaders.Clone(),
					Body:      wsReqBodyRetry,
					Provider:  e.Identifier(),
					AuthID:    authID,
					AuthLabel: authLabel,
					AuthType:  authType,
					AuthValue: authValue,
				})
				if errSendRetry := writeCodexWebsocketMessage(sess, connRetry, wsReqBodyRetry); errSendRetry == nil {
					conn = connRetry
					wsReqBody = wsReqBodyRetry
				} else {
					e.invalidateUpstreamConn(sess, connRetry, "send_error", errSendRetry)
					recordAPIResponseError(ctx, e.cfg, errSendRetry)
					return resp, errSendRetry
				}
			} else {
				recordAPIResponseError(ctx, e.cfg, errDialRetry)
				return resp, errDialRetry
			}
		} else {
			recordAPIResponseError(ctx, e.cfg, errSend)
			return resp, errSend
		}
	}
	aggregator := newCodexNonStreamAggregator()
	for {
		if ctx != nil && ctx.Err() != nil {
			return resp, ctx.Err()
		}
		msgType, payload, errRead := readCodexWebsocketMessage(ctx, sess, conn, readCh)
		if errRead != nil {
			recordAPIResponseError(ctx, e.cfg, errRead)
			return resp, errRead
		}
		if msgType != websocket.TextMessage {
			if msgType == websocket.BinaryMessage {
				err = fmt.Errorf("codex websockets executor: unexpected binary message")
				if sess != nil {
					e.invalidateUpstreamConn(sess, conn, "unexpected_binary", err)
				}
				recordAPIResponseError(ctx, e.cfg, err)
				return resp, err
			}
			continue
		}

		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 {
			continue
		}
		appendAPIResponseChunk(ctx, e.cfg, payload)

		if wsErr, ok := parseCodexWebsocketError(payload); ok {
			if sess != nil {
				e.invalidateUpstreamConn(sess, conn, "upstream_error", wsErr)
			}
			recordAPIResponseError(ctx, e.cfg, wsErr)
			return resp, wsErr
		}

		payload = normalizeCodexWebsocketCompletion(payload)
		aggregator.ingest(payload)
		eventType := gjson.GetBytes(payload, "type").String()
		if eventType == "response.completed" {
			payload = aggregator.applyCompleted(payload)
			if detail, ok := parseCodexUsage(payload); ok {
				reporter.publish(ctx, detail)
			}
			var param any
			out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, originalPayload, body, payload, &param)
			resp = cliproxyexecutor.Response{Payload: []byte(out)}
			return resp, nil
		}
	}
}

func (e *CodexWebsocketsExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	log.Debugf("Executing Codex Websockets stream request with auth ID: %s, model: %s", auth.ID, req.Model)
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, e.cfg, auth)
	defer reporter.trackFailure(ctx, &err)

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

	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body = stripCodexUnsupportedResponseFields(body, shouldPreserveCodexPreviousResponseID(ctx, auth, from, body))
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body, _ = sjson.SetBytes(body, "stream", true)
	if !gjson.GetBytes(body, "instructions").Exists() {
		body, _ = sjson.SetBytes(body, "instructions", "")
	}

	httpURL := strings.TrimSuffix(baseURL, "/") + "/responses"
	wsURL, err := buildCodexResponsesWebsocketURL(httpURL)
	if err != nil {
		return nil, err
	}

	body, wsHeaders := applyCodexPromptCacheHeaders(from, req, body)
	wsHeaders = applyCodexWebsocketHeaders(ctx, wsHeaders, auth, apiKey, e.cfg)

	var authID, authLabel, authType, authValue string
	authID = auth.ID
	authLabel = auth.Label
	authType, authValue = auth.AccountInfo()

	executionSessionID := executionSessionIDFromOptions(opts)
	var sess *codexWebsocketSession
	if executionSessionID != "" {
		sess = e.getOrCreateSession(executionSessionID)
		if sess != nil {
			sess.reqMu.Lock()
		}
	}

	if sess != nil && shouldEnrichCodexWebsocketBridgeFollowupRequest(ctx, executionSessionID) {
		body = enrichCodexWebsocketBridgeFollowupRequest(body, sess.lastRequest)
	}

	wsReqBody := buildCodexWebsocketRequestBody(body)
	if sess != nil {
		sess.lastRequest = bytes.Clone(body)
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       wsURL,
		Method:    "WEBSOCKET",
		Headers:   wsHeaders.Clone(),
		Body:      wsReqBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	conn, respHS, errDial := e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
	var upstreamHeaders http.Header
	if respHS != nil {
		upstreamHeaders = respHS.Header.Clone()
		recordAPIResponseMetadata(ctx, e.cfg, respHS.StatusCode, respHS.Header.Clone())
	}
	if errDial != nil {
		bodyErr := websocketHandshakeBody(respHS)
		if len(bodyErr) > 0 {
			appendAPIResponseChunk(ctx, e.cfg, bodyErr)
		}
		if respHS != nil && respHS.StatusCode == http.StatusUpgradeRequired {
			return e.CodexExecutor.ExecuteStream(ctx, auth, req, opts)
		}
		if respHS != nil && respHS.StatusCode > 0 {
			return nil, newCodexStatusErr(respHS.StatusCode, bodyErr)
		}
		recordAPIResponseError(ctx, e.cfg, errDial)
		if sess != nil {
			sess.reqMu.Unlock()
		}
		return nil, errDial
	}
	closeHTTPResponseBody(respHS, "codex websockets executor: close handshake response body error")

	if sess == nil {
		logCodexWebsocketConnected(executionSessionID, authID, wsURL)
	}

	var readCh chan codexWebsocketRead
	if sess != nil {
		readCh = make(chan codexWebsocketRead, 4096)
		sess.setActive(readCh)
	}

	if errSend := writeCodexWebsocketMessage(sess, conn, wsReqBody); errSend != nil {
		recordAPIResponseError(ctx, e.cfg, errSend)
		if sess != nil {
			e.invalidateUpstreamConn(sess, conn, "send_error", errSend)

			// Retry once with a new websocket connection for the same execution session.
			connRetry, _, errDialRetry := e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
			if errDialRetry != nil || connRetry == nil {
				recordAPIResponseError(ctx, e.cfg, errDialRetry)
				sess.clearActive(readCh)
				sess.reqMu.Unlock()
				return nil, errDialRetry
			}
			wsReqBodyRetry := buildCodexWebsocketRequestBody(body)
			recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
				URL:       wsURL,
				Method:    "WEBSOCKET",
				Headers:   wsHeaders.Clone(),
				Body:      wsReqBodyRetry,
				Provider:  e.Identifier(),
				AuthID:    authID,
				AuthLabel: authLabel,
				AuthType:  authType,
				AuthValue: authValue,
			})
			if errSendRetry := writeCodexWebsocketMessage(sess, connRetry, wsReqBodyRetry); errSendRetry != nil {
				recordAPIResponseError(ctx, e.cfg, errSendRetry)
				e.invalidateUpstreamConn(sess, connRetry, "send_error", errSendRetry)
				sess.clearActive(readCh)
				sess.reqMu.Unlock()
				return nil, errSendRetry
			}
			conn = connRetry
			wsReqBody = wsReqBodyRetry
		} else {
			logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, "send_error", errSend)
			if errClose := conn.Close(); errClose != nil {
				log.Errorf("codex websockets executor: close websocket error: %v", errClose)
			}
			return nil, errSend
		}
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		terminateReason := "completed"
		var terminateErr error

		defer close(out)
		defer func() {
			if sess != nil {
				sess.clearActive(readCh)
				sess.reqMu.Unlock()
				return
			}
			logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, terminateReason, terminateErr)
			if errClose := conn.Close(); errClose != nil {
				log.Errorf("codex websockets executor: close websocket error: %v", errClose)
			}
		}()

		send := func(chunk cliproxyexecutor.StreamChunk) bool {
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

		var param any
		for {
			if ctx != nil && ctx.Err() != nil {
				terminateReason = "context_done"
				terminateErr = ctx.Err()
				_ = send(cliproxyexecutor.StreamChunk{Err: ctx.Err()})
				return
			}
			msgType, payload, errRead := readCodexWebsocketMessage(ctx, sess, conn, readCh)
			if errRead != nil {
				if sess != nil && ctx != nil && ctx.Err() != nil {
					terminateReason = "context_done"
					terminateErr = ctx.Err()
					_ = send(cliproxyexecutor.StreamChunk{Err: ctx.Err()})
					return
				}
				terminateReason = "read_error"
				terminateErr = errRead
				recordAPIResponseError(ctx, e.cfg, errRead)
				reporter.publishFailure(ctx)
				_ = send(cliproxyexecutor.StreamChunk{Err: errRead})
				return
			}
			if msgType != websocket.TextMessage {
				if msgType == websocket.BinaryMessage {
					err = fmt.Errorf("codex websockets executor: unexpected binary message")
					terminateReason = "unexpected_binary"
					terminateErr = err
					recordAPIResponseError(ctx, e.cfg, err)
					reporter.publishFailure(ctx)
					if sess != nil {
						e.invalidateUpstreamConn(sess, conn, "unexpected_binary", err)
					}
					_ = send(cliproxyexecutor.StreamChunk{Err: err})
					return
				}
				continue
			}

			payload = bytes.TrimSpace(payload)
			if len(payload) == 0 {
				continue
			}
			appendAPIResponseChunk(ctx, e.cfg, payload)

			if wsErr, ok := parseCodexWebsocketError(payload); ok {
				terminateReason = "upstream_error"
				terminateErr = wsErr
				recordAPIResponseError(ctx, e.cfg, wsErr)
				reporter.publishFailure(ctx)
				if sess != nil {
					e.invalidateUpstreamConn(sess, conn, "upstream_error", wsErr)
				}
				_ = send(cliproxyexecutor.StreamChunk{Err: wsErr})
				return
			}

			payload = normalizeCodexWebsocketCompletion(payload)
			eventType := gjson.GetBytes(payload, "type").String()
			if eventType == "response.completed" || eventType == "response.done" {
				if detail, ok := parseCodexUsage(payload); ok {
					reporter.publish(ctx, detail)
				}
			}

			line := encodeCodexWebsocketAsSSE(payload)
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, originalPayload, body, line, &param)
			for i := range chunks {
				if !send(cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}) {
					terminateReason = "context_done"
					terminateErr = ctx.Err()
					return
				}
			}
			if eventType == "response.completed" || eventType == "response.done" {
				return
			}
		}
	}()

	return &cliproxyexecutor.StreamResult{Headers: upstreamHeaders, Chunks: out}, nil
}

func (e *CodexWebsocketsExecutor) dialCodexWebsocket(ctx context.Context, auth *cliproxyauth.Auth, wsURL string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	proxyURLs := resolveProxyPoolURLs(e.cfg, auth)
	if len(proxyURLs) == 0 {
		proxyURLs = []string{""}
	}

	var lastResp *http.Response
	var lastErr error
	for idx := range proxyURLs {
		dialer, ok := newProxyAwareWebsocketDialer(proxyURLs[idx])
		if !ok {
			// All proxies invalid would otherwise fall through with lastErr still nil.
			lastErr = fmt.Errorf("codex websockets executor: invalid proxy URL: %q", proxyURLs[idx])
			continue
		}
		conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
		if conn != nil {
			// Avoid gorilla/websocket flate tail validation issues on some upstreams/Go versions.
			// Negotiating permessage-deflate is fine; we just don't compress outbound messages.
			conn.EnableWriteCompression(false)
		}
		if err == nil {
			return conn, resp, nil
		}
		lastResp = resp
		lastErr = err
		if idx+1 >= len(proxyURLs) || !shouldRetryNextWebsocketProxy(err, resp) {
			return conn, resp, err
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		log.WithError(err).Debug("codex websockets executor: websocket proxy failed, retrying next proxy")
	}
	return nil, lastResp, lastErr
}

func writeCodexWebsocketMessage(sess *codexWebsocketSession, conn *websocket.Conn, payload []byte) error {
	if sess != nil {
		return sess.writeMessage(conn, websocket.TextMessage, payload)
	}
	if conn == nil {
		return fmt.Errorf("codex websockets executor: websocket conn is nil")
	}
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func buildCodexWebsocketRequestBody(body []byte) []byte {
	if len(body) == 0 {
		return nil
	}

	// Match codex-rs websocket v2 semantics: every request is `response.create`.
	// Incremental follow-up turns continue on the same websocket using
	// `previous_response_id` + incremental `input`, not `response.append`.
	wsReqBody, errSet := sjson.SetBytes(bytes.Clone(body), "type", "response.create")
	if errSet == nil && len(wsReqBody) > 0 {
		return wsReqBody
	}
	fallback := bytes.Clone(body)
	fallback, _ = sjson.SetBytes(fallback, "type", "response.create")
	return fallback
}

func shouldEnrichCodexWebsocketBridgeFollowupRequest(ctx context.Context, executionSessionID string) bool {
	if strings.TrimSpace(executionSessionID) == "" {
		return false
	}
	return apihandlers.DownstreamWebsocketBridge(ctx)
}

func enrichCodexWebsocketBridgeFollowupRequest(body []byte, lastRequest []byte) []byte {
	if len(body) == 0 || len(lastRequest) == 0 {
		return body
	}
	if strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) == "" {
		return body
	}

	enriched := bytes.Clone(body)
	instructionsResult := gjson.GetBytes(enriched, "instructions")
	if !instructionsResult.Exists() || strings.TrimSpace(instructionsResult.String()) == "" {
		instructions := gjson.GetBytes(lastRequest, "instructions")
		if instructions.Exists() && strings.TrimSpace(instructions.String()) != "" {
			enriched, _ = sjson.SetRawBytes(enriched, "instructions", []byte(instructions.Raw))
		}
	}
	if !gjson.GetBytes(enriched, "tools").Exists() {
		tools := gjson.GetBytes(lastRequest, "tools")
		if tools.Exists() {
			enriched, _ = sjson.SetRawBytes(enriched, "tools", []byte(tools.Raw))
		}
	}
	if !gjson.GetBytes(enriched, "tool_choice").Exists() {
		toolChoice := gjson.GetBytes(lastRequest, "tool_choice")
		if toolChoice.Exists() {
			enriched, _ = sjson.SetRawBytes(enriched, "tool_choice", []byte(toolChoice.Raw))
		}
	}
	return enriched
}

func readCodexWebsocketMessage(ctx context.Context, sess *codexWebsocketSession, conn *websocket.Conn, readCh chan codexWebsocketRead) (int, []byte, error) {
	if sess == nil {
		if conn == nil {
			return 0, nil, fmt.Errorf("codex websockets executor: websocket conn is nil")
		}
		_ = conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout))
		msgType, payload, errRead := conn.ReadMessage()
		return msgType, payload, errRead
	}
	if conn == nil {
		return 0, nil, fmt.Errorf("codex websockets executor: websocket conn is nil")
	}
	if readCh == nil {
		return 0, nil, fmt.Errorf("codex websockets executor: session read channel is nil")
	}
	for {
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case ev, ok := <-readCh:
			if !ok {
				return 0, nil, fmt.Errorf("codex websockets executor: session read channel closed")
			}
			if ev.conn != conn {
				continue
			}
			if ev.err != nil {
				return 0, nil, ev.err
			}
			return ev.msgType, ev.payload, nil
		}
	}
}

func newProxyAwareWebsocketDialer(proxyURL string) (*websocket.Dialer, bool) {
	dialer := &websocket.Dialer{
		Proxy:             nil,
		HandshakeTimeout:  codexResponsesWebsocketHandshakeTO,
		EnableCompression: true,
		NetDialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return dialer, true
	}
	if util.IsDirectProxyMode(proxyURL) {
		dialer.Proxy = nil
		return dialer, true
	}

	parsedURL, errParse := url.Parse(proxyURL)
	if errParse != nil {
		log.Errorf("codex websockets executor: parse proxy URL failed: %v", errParse)
		return nil, false
	}

	switch parsedURL.Scheme {
	case "socks5", "socks5h":
		var proxyAuth *proxy.Auth
		if parsedURL.User != nil {
			username := parsedURL.User.Username()
			password, _ := parsedURL.User.Password()
			proxyAuth = &proxy.Auth{User: username, Password: password}
		}
		socksDialer, errSOCKS5 := proxy.SOCKS5("tcp", parsedURL.Host, proxyAuth, proxy.Direct)
		if errSOCKS5 != nil {
			log.Errorf("codex websockets executor: create SOCKS5 dialer failed: %v", errSOCKS5)
			return nil, false
		}
		dialer.Proxy = nil
		dialer.NetDialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
			return socksDialer.Dial(network, addr)
		}
	case "http", "https":
		dialer.Proxy = http.ProxyURL(parsedURL)
	default:
		log.Errorf("codex websockets executor: unsupported proxy scheme: %s", parsedURL.Scheme)
		return nil, false
	}

	return dialer, true
}

func resolveProxyPoolURLs(cfg *config.Config, auth *cliproxyauth.Auth) []string {
	selection := proxyrouting.Resolve(cfg, auth)
	if proxyURL := strings.TrimSpace(selection.ProxyURL); proxyURL != "" {
		return util.OrderedProxyURLs(proxyURL)
	}
	return nil
}

func shouldRetryNextWebsocketProxy(err error, resp *http.Response) bool {
	if err == nil {
		return false
	}
	if resp != nil {
		return false
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func buildCodexResponsesWebsocketURL(httpURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(httpURL))
	if err != nil {
		return "", err
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	}
	return parsed.String(), nil
}

func applyCodexPromptCacheHeaders(from sdktranslator.Format, req cliproxyexecutor.Request, rawJSON []byte) ([]byte, http.Header) {
	headers := http.Header{}
	if len(rawJSON) == 0 {
		return rawJSON, headers
	}

	var cache codexCache
	if from == "claude" {
		userIDResult := gjson.GetBytes(req.Payload, "metadata.user_id")
		if userIDResult.Exists() {
			key := fmt.Sprintf("%s-%s", req.Model, userIDResult.String())
			if cached, ok := getCodexCache(key); ok {
				cache = cached
			} else {
				cache = codexCache{
					ID:     uuid.New().String(),
					Expire: time.Now().Add(1 * time.Hour),
				}
				setCodexCache(key, cache)
			}
		}
	} else if from == "openai-response" {
		if promptCacheKey := gjson.GetBytes(req.Payload, "prompt_cache_key"); promptCacheKey.Exists() {
			cache.ID = promptCacheKey.String()
		}
	}

	if cache.ID != "" {
		rawJSON, _ = sjson.SetBytes(rawJSON, "prompt_cache_key", cache.ID)
		headers.Set("Conversation_id", cache.ID)
		headers.Set("Session_id", cache.ID)
	}

	return rawJSON, headers
}

func applyCodexWebsocketHeaders(ctx context.Context, headers http.Header, auth *cliproxyauth.Auth, token string, cfg *config.Config) http.Header {
	if headers == nil {
		headers = http.Header{}
	}
	if strings.TrimSpace(token) != "" {
		headers.Set("Authorization", "Bearer "+token)
	}

	var ginHeaders http.Header
	if ginCtx := ginContextFrom(ctx); ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header
	}

	cfgUserAgent, cfgBetaFeatures := codexHeaderDefaults(cfg, auth)
	ensureHeaderWithPriority(headers, ginHeaders, "x-codex-beta-features", cfgBetaFeatures, "")
	misc.EnsureHeader(headers, ginHeaders, "x-codex-turn-state", "")
	misc.EnsureHeader(headers, ginHeaders, "x-codex-turn-metadata", "")
	misc.EnsureHeader(headers, ginHeaders, "x-responsesapi-include-timing-metrics", "")

	misc.EnsureHeader(headers, ginHeaders, "Version", "")
	betaHeader := strings.TrimSpace(headers.Get("OpenAI-Beta"))
	if betaHeader == "" && ginHeaders != nil {
		betaHeader = strings.TrimSpace(ginHeaders.Get("OpenAI-Beta"))
	}
	if betaHeader == "" || !strings.Contains(betaHeader, "responses_websockets=") {
		betaHeader = codexResponsesWebsocketBetaHeaderValue
	}
	headers.Set("OpenAI-Beta", betaHeader)
	misc.EnsureHeader(headers, ginHeaders, "Session_id", uuid.NewString())
	ensureHeaderWithConfigPrecedence(headers, ginHeaders, "User-Agent", cfgUserAgent, codexUserAgent)

	isAPIKey := false
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			isAPIKey = true
		}
	}
	if originator := strings.TrimSpace(ginHeaders.Get("Originator")); originator != "" {
		headers.Set("Originator", originator)
	} else if !isAPIKey {
		headers.Set("Originator", codexOriginator)
	}
	if !isAPIKey {
		if auth != nil && auth.Metadata != nil {
			if accountID, ok := auth.Metadata["account_id"].(string); ok {
				if trimmed := strings.TrimSpace(accountID); trimmed != "" {
					headers.Set("Chatgpt-Account-Id", trimmed)
				}
			}
		}
	}

	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(&http.Request{Header: headers}, attrs)

	return headers
}

func codexHeaderDefaults(cfg *config.Config, auth *cliproxyauth.Auth) (string, string) {
	if cfg == nil || auth == nil {
		return "", ""
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			return "", ""
		}
	}
	return strings.TrimSpace(cfg.CodexHeaderDefaults.UserAgent), strings.TrimSpace(cfg.CodexHeaderDefaults.BetaFeatures)
}

func ensureHeaderWithPriority(target http.Header, source http.Header, key, configValue, fallbackValue string) {
	if target == nil {
		return
	}
	if strings.TrimSpace(target.Get(key)) != "" {
		return
	}
	if source != nil {
		if val := strings.TrimSpace(source.Get(key)); val != "" {
			target.Set(key, val)
			return
		}
	}
	if val := strings.TrimSpace(configValue); val != "" {
		target.Set(key, val)
		return
	}
	if val := strings.TrimSpace(fallbackValue); val != "" {
		target.Set(key, val)
	}
}

func ensureHeaderWithConfigPrecedence(target http.Header, source http.Header, key, configValue, fallbackValue string) {
	if target == nil {
		return
	}
	if strings.TrimSpace(target.Get(key)) != "" {
		return
	}
	if val := strings.TrimSpace(configValue); val != "" {
		target.Set(key, val)
		return
	}
	if source != nil {
		if val := strings.TrimSpace(source.Get(key)); val != "" {
			target.Set(key, val)
			return
		}
	}
	if val := strings.TrimSpace(fallbackValue); val != "" {
		target.Set(key, val)
	}
}

type statusErrWithHeaders struct {
	statusErr
	headers http.Header
}

func (e statusErrWithHeaders) Headers() http.Header {
	if e.headers == nil {
		return nil
	}
	return e.headers.Clone()
}

func parseCodexWebsocketError(payload []byte) (error, bool) {
	if len(payload) == 0 {
		return nil, false
	}
	if strings.TrimSpace(gjson.GetBytes(payload, "type").String()) != "error" {
		return nil, false
	}
	status := int(gjson.GetBytes(payload, "status").Int())
	if status == 0 {
		status = int(gjson.GetBytes(payload, "status_code").Int())
	}
	out := []byte(`{}`)
	if errNode := gjson.GetBytes(payload, "error"); errNode.Exists() {
		raw := errNode.Raw
		if errNode.Type == gjson.String {
			raw = errNode.Raw
		}
		out, _ = sjson.SetRawBytes(out, "error", []byte(raw))
	} else {
		out, _ = sjson.SetBytes(out, "error.type", "server_error")
		message := "Unknown websocket error"
		if status > 0 {
			message = http.StatusText(status)
		}
		out, _ = sjson.SetBytes(out, "error.message", message)
	}
	if status <= 0 && isCodexUsageLimitError(out) {
		// Some websocket error events omit status fields; treat usage limit exhaustion
		// as a quota restriction so the auth manager can cool down the credential.
		status = http.StatusTooManyRequests
	}
	if status <= 0 {
		return nil, false
	}

	baseErr := newCodexStatusErr(status, out)
	headers := parseCodexWebsocketErrorHeaders(payload)
	if baseErr.retryAfter != nil {
		if headers == nil {
			headers = make(http.Header)
		}
		// Expose cooldown hints to downstream clients when available.
		if headers.Get("Retry-After") == "" {
			secs := int(math.Ceil(baseErr.retryAfter.Seconds()))
			if secs < 0 {
				secs = 0
			}
			headers.Set("Retry-After", strconv.Itoa(secs))
		}
	}
	return statusErrWithHeaders{
		statusErr: baseErr,
		headers:   headers,
	}, true
}

func parseCodexWebsocketErrorHeaders(payload []byte) http.Header {
	headersNode := gjson.GetBytes(payload, "headers")
	if !headersNode.Exists() || !headersNode.IsObject() {
		return nil
	}
	mapped := make(http.Header)
	headersNode.ForEach(func(key, value gjson.Result) bool {
		name := strings.TrimSpace(key.String())
		if name == "" {
			return true
		}
		switch value.Type {
		case gjson.String:
			if v := strings.TrimSpace(value.String()); v != "" {
				mapped.Set(name, v)
			}
		case gjson.Number, gjson.True, gjson.False:
			if v := strings.TrimSpace(value.Raw); v != "" {
				mapped.Set(name, v)
			}
		default:
		}
		return true
	})
	if len(mapped) == 0 {
		return nil
	}
	return mapped
}

func normalizeCodexWebsocketCompletion(payload []byte) []byte {
	if strings.TrimSpace(gjson.GetBytes(payload, "type").String()) == "response.done" {
		updated, err := sjson.SetBytes(payload, "type", "response.completed")
		if err == nil && len(updated) > 0 {
			return updated
		}
	}
	return payload
}

func encodeCodexWebsocketAsSSE(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}
	line := make([]byte, 0, len("data: ")+len(payload))
	line = append(line, []byte("data: ")...)
	line = append(line, payload...)
	return line
}

func websocketHandshakeBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	closeHTTPResponseBody(resp, "codex websockets executor: close handshake response body error")
	if len(body) == 0 {
		return nil
	}
	return body
}

func closeHTTPResponseBody(resp *http.Response, logPrefix string) {
	if resp == nil || resp.Body == nil {
		return
	}
	if errClose := resp.Body.Close(); errClose != nil {
		log.Errorf("%s: %v", logPrefix, errClose)
	}
}

func executionSessionIDFromOptions(opts cliproxyexecutor.Options) string {
	if len(opts.Metadata) == 0 {
		return ""
	}
	raw, ok := opts.Metadata[cliproxyexecutor.ExecutionSessionMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func (e *CodexWebsocketsExecutor) getOrCreateSession(sessionID string) *codexWebsocketSession {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	e.sessMu.Lock()
	defer e.sessMu.Unlock()
	if e.sessions == nil {
		e.sessions = make(map[string]*codexWebsocketSession)
	}
	if sess, ok := e.sessions[sessionID]; ok && sess != nil {
		return sess
	}
	sess := &codexWebsocketSession{sessionID: sessionID}
	e.sessions[sessionID] = sess
	return sess
}

func (e *CodexWebsocketsExecutor) ensureUpstreamConn(ctx context.Context, auth *cliproxyauth.Auth, sess *codexWebsocketSession, authID string, wsURL string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	if sess == nil {
		return e.dialCodexWebsocket(ctx, auth, wsURL, headers)
	}

	sess.connMu.Lock()
	conn := sess.conn
	readerConn := sess.readerConn
	sess.connMu.Unlock()
	if conn != nil {
		if readerConn != conn {
			sess.connMu.Lock()
			sess.readerConn = conn
			sess.connMu.Unlock()
			sess.configureConn(conn)
			go e.readUpstreamLoop(sess, conn)
		}
		return conn, nil, nil
	}

	conn, resp, errDial := e.dialCodexWebsocket(ctx, auth, wsURL, headers)
	if errDial != nil {
		return nil, resp, errDial
	}

	sess.connMu.Lock()
	if sess.conn != nil {
		previous := sess.conn
		sess.connMu.Unlock()
		if errClose := conn.Close(); errClose != nil {
			log.Errorf("codex websockets executor: close websocket error: %v", errClose)
		}
		return previous, nil, nil
	}
	sess.conn = conn
	sess.wsURL = wsURL
	sess.authID = authID
	sess.readerConn = conn
	sess.connMu.Unlock()

	sess.configureConn(conn)
	go e.readUpstreamLoop(sess, conn)
	logCodexWebsocketConnected(sess.sessionID, authID, wsURL)
	return conn, resp, nil
}

func (e *CodexWebsocketsExecutor) readUpstreamLoop(sess *codexWebsocketSession, conn *websocket.Conn) {
	if e == nil || sess == nil || conn == nil {
		return
	}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout))
		msgType, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			sess.activeMu.Lock()
			ch := sess.activeCh
			done := sess.activeDone
			sess.activeMu.Unlock()
			if ch != nil {
				select {
				case ch <- codexWebsocketRead{conn: conn, err: errRead}:
				case <-done:
				default:
				}
				sess.clearActive(ch)
				close(ch)
			}
			e.invalidateUpstreamConn(sess, conn, "upstream_disconnected", errRead)
			return
		}

		if msgType != websocket.TextMessage {
			if msgType == websocket.BinaryMessage {
				errBinary := fmt.Errorf("codex websockets executor: unexpected binary message")
				sess.activeMu.Lock()
				ch := sess.activeCh
				done := sess.activeDone
				sess.activeMu.Unlock()
				if ch != nil {
					select {
					case ch <- codexWebsocketRead{conn: conn, err: errBinary}:
					case <-done:
					default:
					}
					sess.clearActive(ch)
					close(ch)
				}
				e.invalidateUpstreamConn(sess, conn, "unexpected_binary", errBinary)
				return
			}
			continue
		}

		sess.activeMu.Lock()
		ch := sess.activeCh
		done := sess.activeDone
		sess.activeMu.Unlock()
		if ch == nil {
			continue
		}
		select {
		case ch <- codexWebsocketRead{conn: conn, msgType: msgType, payload: payload}:
		case <-done:
		}
	}
}

func (e *CodexWebsocketsExecutor) invalidateUpstreamConn(sess *codexWebsocketSession, conn *websocket.Conn, reason string, err error) {
	if sess == nil || conn == nil {
		return
	}

	sess.connMu.Lock()
	current := sess.conn
	authID := sess.authID
	wsURL := sess.wsURL
	sessionID := sess.sessionID
	if current == nil || current != conn {
		sess.connMu.Unlock()
		return
	}
	sess.conn = nil
	if sess.readerConn == conn {
		sess.readerConn = nil
	}
	sess.connMu.Unlock()

	logCodexWebsocketDisconnected(sessionID, authID, wsURL, reason, err)
	if errClose := conn.Close(); errClose != nil {
		log.Errorf("codex websockets executor: close websocket error: %v", errClose)
	}
}

func (e *CodexWebsocketsExecutor) CloseExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if e == nil {
		return
	}
	if sessionID == "" {
		return
	}
	if sessionID == cliproxyauth.CloseAllExecutionSessionsID {
		e.closeAllExecutionSessions("executor_replaced")
		return
	}

	e.sessMu.Lock()
	sess := e.sessions[sessionID]
	delete(e.sessions, sessionID)
	e.sessMu.Unlock()

	e.closeExecutionSession(sess, "session_closed")
}

func (e *CodexWebsocketsExecutor) closeAllExecutionSessions(reason string) {
	if e == nil {
		return
	}

	e.sessMu.Lock()
	sessions := make([]*codexWebsocketSession, 0, len(e.sessions))
	for sessionID, sess := range e.sessions {
		delete(e.sessions, sessionID)
		if sess != nil {
			sessions = append(sessions, sess)
		}
	}
	e.sessMu.Unlock()

	for i := range sessions {
		e.closeExecutionSession(sessions[i], reason)
	}
}

func (e *CodexWebsocketsExecutor) closeExecutionSession(sess *codexWebsocketSession, reason string) {
	if sess == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "session_closed"
	}

	sess.connMu.Lock()
	conn := sess.conn
	authID := sess.authID
	wsURL := sess.wsURL
	sess.conn = nil
	if sess.readerConn == conn {
		sess.readerConn = nil
	}
	sessionID := sess.sessionID
	sess.connMu.Unlock()

	if conn == nil {
		return
	}
	logCodexWebsocketDisconnected(sessionID, authID, wsURL, reason, nil)
	if errClose := conn.Close(); errClose != nil {
		log.Errorf("codex websockets executor: close websocket error: %v", errClose)
	}
}

func logCodexWebsocketConnected(sessionID string, authID string, wsURL string) {
	log.Infof("codex websockets: upstream connected session=%s auth=%s url=%s", strings.TrimSpace(sessionID), strings.TrimSpace(authID), strings.TrimSpace(wsURL))
}

func logCodexWebsocketDisconnected(sessionID string, authID string, wsURL string, reason string, err error) {
	if err != nil {
		log.Infof("codex websockets: upstream disconnected session=%s auth=%s url=%s reason=%s err=%v", strings.TrimSpace(sessionID), strings.TrimSpace(authID), strings.TrimSpace(wsURL), strings.TrimSpace(reason), err)
		return
	}
	log.Infof("codex websockets: upstream disconnected session=%s auth=%s url=%s reason=%s", strings.TrimSpace(sessionID), strings.TrimSpace(authID), strings.TrimSpace(wsURL), strings.TrimSpace(reason))
}

// CodexAutoExecutor routes Codex requests to the websocket transport only when:
//  1. The downstream transport is websocket, and
//  2. The selected auth enables websockets.
//
// For non-websocket downstream requests, it always uses the legacy HTTP implementation.
type CodexAutoExecutor struct {
	httpExec *CodexExecutor
	wsExec   *CodexWebsocketsExecutor
}

func shouldUseCodexWebsocketExecutor(ctx context.Context, auth *cliproxyauth.Auth) bool {
	return apihandlers.DownstreamWebsocketBridge(ctx) && codexWebsocketsEnabled(auth)
}

func NewCodexAutoExecutor(cfg *config.Config) *CodexAutoExecutor {
	return &CodexAutoExecutor{
		httpExec: NewCodexExecutor(cfg),
		wsExec:   NewCodexWebsocketsExecutor(cfg),
	}
}

func (e *CodexAutoExecutor) Identifier() string { return "codex" }

func (e *CodexAutoExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if e == nil || e.httpExec == nil {
		return nil
	}
	return e.httpExec.PrepareRequest(req, auth)
}

func (e *CodexAutoExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.HttpRequest(ctx, auth, req)
}

func (e *CodexAutoExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex auto executor: executor is nil")
	}
	if shouldUseCodexWebsocketExecutor(ctx, auth) {
		return e.wsExec.Execute(ctx, auth, req, opts)
	}
	return e.httpExec.Execute(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return nil, fmt.Errorf("codex auto executor: executor is nil")
	}
	if shouldUseCodexWebsocketExecutor(ctx, auth) {
		return e.wsExec.ExecuteStream(ctx, auth, req, opts)
	}
	return e.httpExec.ExecuteStream(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.Refresh(ctx, auth)
}

func (e *CodexAutoExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.CountTokens(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) CloseExecutionSession(sessionID string) {
	if e == nil || e.wsExec == nil {
		return
	}
	e.wsExec.CloseExecutionSession(sessionID)
}

func codexWebsocketsEnabled(auth *cliproxyauth.Auth) bool {
	return auth != nil && auth.WebsocketsEnabled()
}
