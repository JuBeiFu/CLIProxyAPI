package auth

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type responseStickyExecutor struct {
	id string

	mu            sync.Mutex
	executeCalls  []string
	streamCalls   []string
	payloads      [][]byte
	executeErrors map[string]error
	streamErrors  map[string]error
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
	err := e.executeErrors[auth.ID]
	e.mu.Unlock()
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

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
	err := e.streamErrors[auth.ID]
	e.mu.Unlock()

	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	if err != nil {
		chunks <- cliproxyexecutor.StreamChunk{Err: err}
		close(chunks)
		return &cliproxyexecutor.StreamResult{
			Headers: http.Header{"X-Auth": {auth.ID}},
			Chunks:  chunks,
		}, nil
	}
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

func (e *responseStickyExecutor) ExecuteCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.executeCalls))
	copy(out, e.executeCalls)
	return out
}

func (e *responseStickyExecutor) StreamCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.streamCalls))
	copy(out, e.streamCalls)
	return out
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

func TestManagerExecute_OpenAIResponsesPreviousResponseIDRetriesOnAnotherAuthAfterTransientPinnedFailure(t *testing.T) {
	t.Parallel()

	const (
		authAID = "response-bind-exec-transient-auth-a"
		authBID = "response-bind-exec-transient-auth-b"
		model   = "gpt-5.4"
	)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(1, 100*time.Millisecond, 2)
	executor := &responseStickyExecutor{id: "codex"}
	manager.RegisterExecutor(executor)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authAID, "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(authBID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(authAID)
		reg.UnregisterClient(authBID)
	})

	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authAID,
		Provider: "codex",
	}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authBID,
		Provider: "codex",
	}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}

	firstPayload := []byte(`{"input":"hello"}`)
	firstResp, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model:   model,
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
	if firstResponseID == "" {
		t.Fatalf("first response id = empty, payload = %s", string(firstResp.Payload))
	}
	if boundAuthID := manager.lookupBoundAuthID(firstResponseID); boundAuthID != authAID {
		t.Fatalf("lookupBoundAuthID(%q) = %q, want %q", firstResponseID, boundAuthID, authAID)
	}

	executor.mu.Lock()
	executor.executeErrors = map[string]error{
		authAID: &retryAfterStatusError{status: http.StatusBadGateway, message: "upstream zero-usage completion without output events"},
	}
	executor.mu.Unlock()

	secondPayload := []byte(fmt.Sprintf(`{"input":"follow up","previous_response_id":"%s"}`, firstResponseID))
	secondResp, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model:   model,
		Payload: secondPayload,
	}, cliproxyexecutor.Options{
		OriginalRequest: secondPayload,
		SourceFormat:    sdktranslator.FromString("openai-response"),
	})
	if errExecute != nil {
		t.Fatalf("second Execute() error = %v", errExecute)
	}
	if secondAuthID := parseStickyResponseAuthID(secondResp.Payload); secondAuthID != authBID {
		t.Fatalf("second Execute() auth = %q, want %q", secondAuthID, authBID)
	}

	wantCalls := []string{authAID, authAID, authBID}
	if gotCalls := executor.ExecuteCalls(); !reflect.DeepEqual(gotCalls, wantCalls) {
		t.Fatalf("execute calls = %#v, want %#v", gotCalls, wantCalls)
	}
	if boundAuthID := manager.lookupBoundAuthID(firstResponseID); boundAuthID != "" {
		t.Fatalf("lookupBoundAuthID(%q) = %q, want empty after transient failure", firstResponseID, boundAuthID)
	}
}

func TestManagerExecuteStream_OpenAIResponsesPreviousResponseIDRetriesOnAnotherAuthAfterTransientPinnedFailure(t *testing.T) {
	t.Parallel()

	const (
		authAID = "response-bind-stream-transient-auth-a"
		authBID = "response-bind-stream-transient-auth-b"
		model   = "gpt-5.4"
	)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(1, 100*time.Millisecond, 2)
	executor := &responseStickyExecutor{id: "codex"}
	manager.RegisterExecutor(executor)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authAID, "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(authBID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(authAID)
		reg.UnregisterClient(authBID)
	})

	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authAID,
		Provider: "codex",
	}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authBID,
		Provider: "codex",
	}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}

	firstPayload := []byte(`{"input":"hello"}`)
	firstResult, errExecute := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model:   model,
		Payload: firstPayload,
	}, cliproxyexecutor.Options{
		Stream:          true,
		OriginalRequest: firstPayload,
		SourceFormat:    sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.PinnedAuthMetadataKey: authAID,
		},
	})
	if errExecute != nil {
		t.Fatalf("first ExecuteStream() error = %v", errExecute)
	}
	firstResponseID, firstAuthID := readStickyCompletedChunk(t, firstResult)
	if firstResponseID == "" {
		t.Fatal("first stream response id = empty")
	}
	if firstAuthID != authAID {
		t.Fatalf("first stream auth = %q, want %q", firstAuthID, authAID)
	}

	executor.mu.Lock()
	executor.streamErrors = map[string]error{
		authAID: &retryAfterStatusError{status: http.StatusBadGateway, message: "upstream zero-usage completion without output events"},
	}
	executor.mu.Unlock()

	secondPayload := []byte(fmt.Sprintf(`{"input":"follow up","previous_response_id":"%s"}`, firstResponseID))
	secondResult, errExecute := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model:   model,
		Payload: secondPayload,
	}, cliproxyexecutor.Options{
		Stream:          true,
		OriginalRequest: secondPayload,
		SourceFormat:    sdktranslator.FromString("openai-response"),
	})
	if errExecute != nil {
		t.Fatalf("second ExecuteStream() error = %v", errExecute)
	}
	_, secondAuthID := readStickyCompletedChunk(t, secondResult)
	if secondAuthID != authBID {
		t.Fatalf("second ExecuteStream() auth = %q, want %q", secondAuthID, authBID)
	}

	wantCalls := []string{authAID, authAID, authBID}
	if gotCalls := executor.StreamCalls(); !reflect.DeepEqual(gotCalls, wantCalls) {
		t.Fatalf("stream calls = %#v, want %#v", gotCalls, wantCalls)
	}
	if boundAuthID := manager.lookupBoundAuthID(firstResponseID); boundAuthID != "" {
		t.Fatalf("lookupBoundAuthID(%q) = %q, want empty after transient failure", firstResponseID, boundAuthID)
	}
}

func TestManagerExecute_OpenAIResponsesCompactIgnoresPreviousResponseIDBindingAndUsesRoundRobin(t *testing.T) {
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
	if secondAuthID != authAID {
		t.Fatalf("compact Execute() auth = %q, selected = %q, want %q", secondAuthID, secondSelectedAuth, authAID)
	}
}

func TestManagerExecute_OpenAIResponsesCompactHonorsExplicitPreferBestAuth(t *testing.T) {
	t.Parallel()

	const (
		authAID = "response-bind-compact-explicit-best-auth-a"
		authBID = "response-bind-compact-explicit-best-auth-b"
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
	if firstResponseID == "" {
		t.Fatalf("first response id = empty, payload = %s", string(firstResp.Payload))
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
			cliproxyexecutor.PreferBestAuthMetadataKey: true,
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

func TestManager_UnbindResponsesForAuthRemovesCompactTranscripts(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.bindResponseToAuth("resp-a", "auth-a")
	manager.bindCompactTranscript("resp-a", []byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]`))

	if got := manager.lookupCompactTranscript("resp-a"); len(got) == 0 {
		t.Fatal("compact transcript was not stored")
	}

	if removed := manager.unbindResponsesForAuth("auth-a"); removed != 1 {
		t.Fatalf("removed bindings = %d, want 1", removed)
	}
	if got := manager.lookupCompactTranscript("resp-a"); len(got) != 0 {
		t.Fatalf("compact transcript still present after unbind, len=%d", len(got))
	}
}

func TestManager_BindCompactTranscriptEnforcesTotalByteLimit(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.responseCompactMaxBytes = 170
	manager.responseCompactMaxEntries = 10

	first := []byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"first compact transcript"}]}]`)
	second := []byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"second compact transcript"}]}]`)
	manager.bindCompactTranscript("resp-a", first)
	manager.bindCompactTranscript("resp-b", second)

	if got := manager.lookupCompactTranscript("resp-a"); len(got) != 0 {
		t.Fatalf("old compact transcript was not evicted, len=%d", len(got))
	}
	if got := manager.lookupCompactTranscript("resp-b"); !bytes.Equal(got, second) {
		t.Fatalf("new compact transcript = %s, want %s", string(got), string(second))
	}
	if manager.responseCompactsTotalBytes > manager.responseCompactMaxBytes {
		t.Fatalf("compact transcript bytes = %d, limit = %d", manager.responseCompactsTotalBytes, manager.responseCompactMaxBytes)
	}
}

func TestManager_ExecuteStreamWithModelPoolDoesNotClonePayloadForActiveStream(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &responseStickyExecutor{id: "codex"}
	reqPayload := largeCompactRequestPayload(512, 16*1024)

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	streamResult, err := manager.executeStreamWithModelPool(context.Background(), executor, &Auth{
		ID:       "stream-active-payload-auth",
		Provider: "codex",
	}, "codex", cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: reqPayload,
	}, cliproxyexecutor.Options{}, "gpt-5.4", []string{"gpt-5.4"}, false, &inflightLease{})
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("executeStreamWithModelPool() error = %v", err)
	}
	for range streamResult.Chunks {
	}
	runtime.KeepAlive(reqPayload)

	allocated := after.TotalAlloc - before.TotalAlloc
	limit := uint64(len(reqPayload) / 8)
	if allocated > limit {
		t.Fatalf("allocated %d bytes while wrapping %d-byte streaming request, want <= %d", allocated, len(reqPayload), limit)
	}
}

func TestManager_BindResponseFromStreamResultDoesNotClonePayloadBeforeCompletion(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	reqPayload := largeCompactRequestPayload(512, 16*1024)
	chunks := make(chan cliproxyexecutor.StreamChunk)
	streamResult := &cliproxyexecutor.StreamResult{
		Headers: http.Header{"X-Test": {"stream"}},
		Chunks:  chunks,
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	wrapped := manager.bindResponseFromStreamResult("stream-bind-payload-auth", reqPayload, streamResult)
	runtime.ReadMemStats(&after)
	close(chunks)
	for range wrapped.Chunks {
	}
	runtime.KeepAlive(reqPayload)

	allocated := after.TotalAlloc - before.TotalAlloc
	limit := uint64(len(reqPayload) / 8)
	if allocated > limit {
		t.Fatalf("allocated %d bytes while binding %d-byte streaming request, want <= %d", allocated, len(reqPayload), limit)
	}
}

func TestCompactTranscriptFromPayloadLargeInputAllocationsStayBounded(t *testing.T) {
	t.Parallel()

	reqPayload := largeCompactRequestPayload(48, 16*1024)
	respPayload := []byte(`{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
	_ = compactTranscriptFromPayload(reqPayload, respPayload)

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	transcript := compactTranscriptFromPayload(reqPayload, respPayload)
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(transcript)

	if !gjson.ValidBytes(transcript) {
		t.Fatalf("transcript is invalid JSON: %s", string(transcript))
	}
	if got := len(gjson.ParseBytes(transcript).Array()); got != 49 {
		t.Fatalf("transcript items = %d, want 49", got)
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	limit := uint64(len(reqPayload) * 3)
	if allocated > limit {
		t.Fatalf("allocated %d bytes for %d-byte request, want <= %d", allocated, len(reqPayload), limit)
	}
}

func largeCompactRequestPayload(messageCount, messageSize int) []byte {
	text := strings.Repeat("x", messageSize)
	var b strings.Builder
	b.Grow(messageCount*messageSize + 256)
	b.WriteString(`{"input":[`)
	for i := 0; i < messageCount; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"type":"message","role":"user","content":[{"type":"input_text","text":`)
		b.WriteString(strconv.Quote(text))
		b.WriteString(`}]}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}
