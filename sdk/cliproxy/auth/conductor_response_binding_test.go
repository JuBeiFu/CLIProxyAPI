package auth

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type responseStickyExecutor struct {
	id string

	mu           sync.Mutex
	executeCalls []string
	streamCalls  []string
	payloads     [][]byte
}

func (e *responseStickyExecutor) Identifier() string {
	return e.id
}

func (e *responseStickyExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = ctx
	_ = opts

	e.mu.Lock()
	callIndex := len(e.executeCalls)
	e.executeCalls = append(e.executeCalls, auth.ID)
	e.payloads = append(e.payloads, bytes.Clone(req.Payload))
	e.mu.Unlock()

	payload := []byte(fmt.Sprintf(`{"id":"resp-exec-%d","auth_id":"%s","output":[{"id":"msg-exec-%d","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"assistant-%d"}]}]}`, callIndex+1, auth.ID, callIndex+1, callIndex+1))
	return cliproxyexecutor.Response{
		Payload: payload,
		Headers: http.Header{"X-Auth": {auth.ID}},
	}, nil
}

func (e *responseStickyExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	_ = ctx
	_ = req
	_ = opts

	e.mu.Lock()
	callIndex := len(e.streamCalls)
	e.streamCalls = append(e.streamCalls, auth.ID)
	e.mu.Unlock()

	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.output_text.delta","delta":"hi"}`)}
	chunks <- cliproxyexecutor.StreamChunk{
		Payload: []byte(fmt.Sprintf(`data: {"type":"response.completed","response":{"id":"resp-stream-%d","auth_id":"%s"}}`, callIndex+1, auth.ID)),
	}
	close(chunks)
	return &cliproxyexecutor.StreamResult{
		Headers: http.Header{"X-Auth": {auth.ID}},
		Chunks:  chunks,
	}, nil
}

func (e *responseStickyExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	_ = ctx
	return auth, nil
}

func (e *responseStickyExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = ctx
	_ = auth
	_ = req
	_ = opts
	return cliproxyexecutor.Response{}, nil
}

func (e *responseStickyExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	_ = ctx
	_ = auth
	_ = req
	return nil, nil
}

func parseStickyResponseAuthID(payload []byte) string {
	return gjson.GetBytes(payload, "auth_id").String()
}

func parseStickyResponseID(payload []byte) string {
	return gjson.GetBytes(payload, "id").String()
}

func readStickyCompletedChunk(t *testing.T, streamResult *cliproxyexecutor.StreamResult) (string, string) {
	t.Helper()
	if streamResult == nil {
		t.Fatal("streamResult = nil")
	}
	for chunk := range streamResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		payload := bytes.TrimSpace(chunk.Payload)
		if len(payload) == 0 {
			continue
		}
		if bytes.HasPrefix(payload, []byte("data:")) {
			payload = bytes.TrimSpace(payload[len("data:"):])
		}
		if !gjson.ValidBytes(payload) {
			continue
		}
		eventType := gjson.GetBytes(payload, "type").String()
		if eventType != "response.completed" && eventType != "response.done" {
			continue
		}
		return gjson.GetBytes(payload, "response.id").String(), gjson.GetBytes(payload, "response.auth_id").String()
	}
	t.Fatal("missing response.completed chunk")
	return "", ""
}

func TestManagerExecute_OpenAIResponsesPreviousResponseIDPinsAuth(t *testing.T) {
	t.Parallel()

	const (
		authAID = "response-bind-exec-auth-a"
		authBID = "response-bind-exec-auth-b"
	)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &responseStickyExecutor{id: "codex"}
	manager.RegisterExecutor(executor)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authAID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	reg.RegisterClient(authBID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() {
		reg.UnregisterClient(authAID)
		reg.UnregisterClient(authBID)
	})

	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authAID,
		Provider: "codex",
		Attributes: map[string]string{
			"plan_type": "free",
		},
	}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authBID,
		Provider: "codex",
		Attributes: map[string]string{
			"plan_type": "pro",
		},
	}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}

	firstPayload := []byte(`{"input":"hello"}`)
	firstResp, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: firstPayload,
	}, cliproxyexecutor.Options{
		OriginalRequest: firstPayload,
		SourceFormat:    sdktranslator.FromString("openai-response"),
	})
	if errExecute != nil {
		t.Fatalf("first Execute() error = %v", errExecute)
	}
	firstResponseID := parseStickyResponseID(firstResp.Payload)
	firstAuthID := parseStickyResponseAuthID(firstResp.Payload)
	if firstResponseID == "" {
		t.Fatalf("first response id = empty, payload = %s", string(firstResp.Payload))
	}
	if firstAuthID == "" {
		t.Fatalf("first auth id = empty, payload = %s", string(firstResp.Payload))
	}
	if boundAuthID := manager.lookupBoundAuthID(firstResponseID); boundAuthID != firstAuthID {
		t.Fatalf("lookupBoundAuthID(%q) = %q, want %q", firstResponseID, boundAuthID, firstAuthID)
	}

	secondPayload := []byte(fmt.Sprintf(`{"input":"follow up","previous_response_id":"%s"}`, firstResponseID))
	var secondSelectedAuth string
	secondResp, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: secondPayload,
	}, cliproxyexecutor.Options{
		OriginalRequest: secondPayload,
		SourceFormat:    sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
				secondSelectedAuth = authID
			},
		},
	})
	if errExecute != nil {
		t.Fatalf("second Execute() error = %v", errExecute)
	}
	secondAuthID := parseStickyResponseAuthID(secondResp.Payload)
	if secondAuthID != firstAuthID {
		t.Fatalf("second Execute() auth = %q, selected = %q, want %q", secondAuthID, secondSelectedAuth, firstAuthID)
	}
}

func TestManagerExecuteStream_OpenAIResponsesPreviousResponseIDPinsAuth(t *testing.T) {
	t.Parallel()

	const (
		authAID = "response-bind-stream-auth-a"
		authBID = "response-bind-stream-auth-b"
	)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &responseStickyExecutor{id: "codex"}
	manager.RegisterExecutor(executor)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authAID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	reg.RegisterClient(authBID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() {
		reg.UnregisterClient(authAID)
		reg.UnregisterClient(authBID)
	})

	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authAID,
		Provider: "codex",
		Attributes: map[string]string{
			"plan_type": "free",
		},
	}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authBID,
		Provider: "codex",
		Attributes: map[string]string{
			"plan_type": "pro",
		},
	}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}

	firstPayload := []byte(`{"input":"hello"}`)
	firstResult, errExecute := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: firstPayload,
	}, cliproxyexecutor.Options{
		Stream:          true,
		OriginalRequest: firstPayload,
		SourceFormat:    sdktranslator.FromString("openai-response"),
	})
	if errExecute != nil {
		t.Fatalf("first ExecuteStream() error = %v", errExecute)
	}
	firstResponseID, firstAuthID := readStickyCompletedChunk(t, firstResult)
	if firstResponseID == "" {
		t.Fatal("first stream response id = empty")
	}
	if firstAuthID == "" {
		t.Fatal("first stream auth id = empty")
	}
	if boundAuthID := manager.lookupBoundAuthID(firstResponseID); boundAuthID != firstAuthID {
		t.Fatalf("lookupBoundAuthID(%q) = %q, want %q", firstResponseID, boundAuthID, firstAuthID)
	}

	secondPayload := []byte(fmt.Sprintf(`{"input":"follow up","previous_response_id":"%s"}`, firstResponseID))
	var secondSelectedAuth string
	secondResult, errExecute := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: secondPayload,
	}, cliproxyexecutor.Options{
		Stream:          true,
		OriginalRequest: secondPayload,
		SourceFormat:    sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
				secondSelectedAuth = authID
			},
		},
	})
	if errExecute != nil {
		t.Fatalf("second ExecuteStream() error = %v, selected = %q", errExecute, secondSelectedAuth)
	}
	_, secondAuthID := readStickyCompletedChunk(t, secondResult)
	if secondAuthID != firstAuthID {
		t.Fatalf("second ExecuteStream() auth = %q, selected = %q, want %q", secondAuthID, secondSelectedAuth, firstAuthID)
	}
}

func TestManagerExecute_OpenAIResponsesCompactIgnoresPreviousResponseIDBindingAndPrefersBestAuth(t *testing.T) {
	t.Parallel()

	const (
		authAID = "response-bind-compact-auth-a"
		authBID = "response-bind-compact-auth-b"
	)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &responseStickyExecutor{id: "codex"}
	manager.RegisterExecutor(executor)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authAID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	reg.RegisterClient(authBID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() {
		reg.UnregisterClient(authAID)
		reg.UnregisterClient(authBID)
	})

	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authAID,
		Provider: "codex",
		Attributes: map[string]string{
			"plan_type": "free",
		},
	}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authBID,
		Provider: "codex",
		Attributes: map[string]string{
			"plan_type": "pro",
		},
	}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}

	firstPayload := []byte(`{"input":"hello"}`)
	firstResp, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: firstPayload,
	}, cliproxyexecutor.Options{
		OriginalRequest: firstPayload,
		SourceFormat:    sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.PinnedAuthMetadataKey: authAID,
		},
	})
	if errExecute != nil {
		t.Fatalf("first Execute() error = %v", errExecute)
	}
	firstResponseID := parseStickyResponseID(firstResp.Payload)
	firstAuthID := parseStickyResponseAuthID(firstResp.Payload)
	if firstResponseID == "" {
		t.Fatalf("first response id = empty, payload = %s", string(firstResp.Payload))
	}
	if firstAuthID != authAID {
		t.Fatalf("first Execute() auth = %q, want %q", firstAuthID, authAID)
	}
	if boundAuthID := manager.lookupBoundAuthID(firstResponseID); boundAuthID != firstAuthID {
		t.Fatalf("lookupBoundAuthID(%q) = %q, want %q", firstResponseID, boundAuthID, firstAuthID)
	}
	secondPayload := []byte(fmt.Sprintf(`{"input":"compact this","previous_response_id":"%s"}`, firstResponseID))
	var secondSelectedAuth string
	secondResp, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: secondPayload,
	}, cliproxyexecutor.Options{
		Alt:             "responses/compact",
		OriginalRequest: secondPayload,
		SourceFormat:    sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
				secondSelectedAuth = authID
			},
		},
	})
	if errExecute != nil {
		t.Fatalf("second Execute() error = %v, selected = %q", errExecute, secondSelectedAuth)
	}
	secondAuthID := parseStickyResponseAuthID(secondResp.Payload)
	if secondAuthID != authBID {
		t.Fatalf("compact Execute() auth = %q, selected = %q, want %q", secondAuthID, secondSelectedAuth, authBID)
	}
}

func TestManagerExecute_OpenAIResponsesCompactPreviousResponseOnlyPinsOriginalAuth(t *testing.T) {
	t.Parallel()

	const (
		authAID = "response-bind-compact-prevonly-auth-a"
		authBID = "response-bind-compact-prevonly-auth-b"
	)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &responseStickyExecutor{id: "codex"}
	manager.RegisterExecutor(executor)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authAID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	reg.RegisterClient(authBID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() {
		reg.UnregisterClient(authAID)
		reg.UnregisterClient(authBID)
	})

	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authAID,
		Provider: "codex",
		Attributes: map[string]string{
			"plan_type": "free",
		},
	}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authBID,
		Provider: "codex",
		Attributes: map[string]string{
			"plan_type": "pro",
		},
	}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}

	firstPayload := []byte(`{"input":"hello"}`)
	firstResp, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: firstPayload,
	}, cliproxyexecutor.Options{
		OriginalRequest: firstPayload,
		SourceFormat:    sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.PinnedAuthMetadataKey: authAID,
		},
	})
	if errExecute != nil {
		t.Fatalf("first Execute() error = %v", errExecute)
	}
	firstResponseID := parseStickyResponseID(firstResp.Payload)
	firstAuthID := parseStickyResponseAuthID(firstResp.Payload)
	if firstResponseID == "" {
		t.Fatalf("first response id = empty, payload = %s", string(firstResp.Payload))
	}
	if firstAuthID != authAID {
		t.Fatalf("first Execute() auth = %q, want %q", firstAuthID, authAID)
	}

	secondPayload := []byte(fmt.Sprintf(`{"previous_response_id":"%s"}`, firstResponseID))
	var secondSelectedAuth string
	secondResp, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: secondPayload,
	}, cliproxyexecutor.Options{
		Alt:             "responses/compact",
		OriginalRequest: secondPayload,
		SourceFormat:    sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
				secondSelectedAuth = authID
			},
		},
	})
	if errExecute != nil {
		t.Fatalf("second Execute() error = %v, selected = %q", errExecute, secondSelectedAuth)
	}
	secondAuthID := parseStickyResponseAuthID(secondResp.Payload)
	if secondAuthID != authAID {
		t.Fatalf("compact previous-response-only auth = %q, selected = %q, want %q", secondAuthID, secondSelectedAuth, authAID)
	}
}

func TestManagerExecute_OpenAIResponsesCompactPreviousResponseOnlyExpandsToTranscript(t *testing.T) {
	t.Parallel()

	const authID = "response-bind-compact-transcript-auth"

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &responseStickyExecutor{id: "codex"}
	manager.RegisterExecutor(executor)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() {
		reg.UnregisterClient(authID)
	})

	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authID,
		Provider: "codex",
	}); errRegister != nil {
		t.Fatalf("Register(auth) error = %v", errRegister)
	}

	firstPayload := []byte(`{"input":"say pong only"}`)
	firstResp, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: firstPayload,
	}, cliproxyexecutor.Options{
		OriginalRequest: firstPayload,
		SourceFormat:    sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.PinnedAuthMetadataKey: authID,
		},
	})
	if errExecute != nil {
		t.Fatalf("first Execute() error = %v", errExecute)
	}
	firstResponseID := parseStickyResponseID(firstResp.Payload)
	if firstResponseID == "" {
		t.Fatalf("first response id = empty, payload = %s", string(firstResp.Payload))
	}

	secondPayload := []byte(fmt.Sprintf(`{"previous_response_id":"%s"}`, firstResponseID))
	_, errExecute = manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: secondPayload,
	}, cliproxyexecutor.Options{
		Alt:             "responses/compact",
		OriginalRequest: secondPayload,
		SourceFormat:    sdktranslator.FromString("openai-response"),
	})
	if errExecute != nil {
		t.Fatalf("second Execute() error = %v", errExecute)
	}

	executor.mu.Lock()
	defer executor.mu.Unlock()

	if len(executor.payloads) != 2 {
		t.Fatalf("payload count = %d, want 2", len(executor.payloads))
	}

	compactPayload := executor.payloads[1]
	if prev := gjson.GetBytes(compactPayload, "previous_response_id").String(); prev != "" {
		t.Fatalf("compact payload previous_response_id = %q, want empty; payload=%s", prev, string(compactPayload))
	}

	input := gjson.GetBytes(compactPayload, "input").Array()
	if len(input) != 2 {
		t.Fatalf("compact payload input len = %d, want 2; payload=%s", len(input), string(compactPayload))
	}
	if got := input[0].Get("role").String(); got != "user" {
		t.Fatalf("input[0].role = %q, want %q; payload=%s", got, "user", string(compactPayload))
	}
	if got := input[0].Get("content.0.type").String(); got != "input_text" {
		t.Fatalf("input[0].content[0].type = %q, want %q; payload=%s", got, "input_text", string(compactPayload))
	}
	if got := input[0].Get("content.0.text").String(); got != "say pong only" {
		t.Fatalf("input[0].content[0].text = %q, want %q; payload=%s", got, "say pong only", string(compactPayload))
	}
	if got := input[1].Get("role").String(); got != "assistant" {
		t.Fatalf("input[1].role = %q, want %q; payload=%s", got, "assistant", string(compactPayload))
	}
	if got := input[1].Get("content.0.type").String(); got != "output_text" {
		t.Fatalf("input[1].content[0].type = %q, want %q; payload=%s", got, "output_text", string(compactPayload))
	}
	if got := input[1].Get("content.0.text").String(); got != "assistant-1" {
		t.Fatalf("input[1].content[0].text = %q, want %q; payload=%s", got, "assistant-1", string(compactPayload))
	}
}
