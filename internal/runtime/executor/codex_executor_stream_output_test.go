package executor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorExecuteStreamForwardsInitialMetadataBeforeUserOutput(t *testing.T) {
	proceed := make(chan struct{})
	createdSent := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_early\",\"created_at\":1775555723,\"model\":\"gpt-5.5\"}}\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(createdSent)
		<-proceed
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_early\",\"object\":\"response\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello","stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	select {
	case <-createdSent:
	case <-time.After(time.Second):
		close(proceed)
		t.Fatal("upstream did not send response.created")
	}

	var first cliproxyexecutor.StreamChunk
	timedOut := false
	select {
	case first = <-result.Chunks:
	case <-time.After(200 * time.Millisecond):
		timedOut = true
	}
	close(proceed)
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
	}
	if timedOut {
		t.Fatal("response.created was buffered until user output")
	}
	if first.Err != nil {
		t.Fatalf("unexpected first chunk error: %v", first.Err)
	}
	if !bytes.Contains(first.Payload, []byte("response.created")) {
		t.Fatalf("first chunk = %s, want response.created", string(first.Payload))
	}
}

func TestCodexExecutorExecute_EmptyStreamCompletionOutputUsesOutputItemDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]},\"output_index\":0}\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1775555723,\"status\":\"completed\",\"model\":\"gpt-5.4-mini-2026-03-17\",\"output\":[],\"usage\":{\"input_tokens\":8,\"output_tokens\":28,\"total_tokens\":36}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"Say ok"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	gotContent := gjson.GetBytes(resp.Payload, "choices.0.message.content").String()
	if gotContent != "ok" {
		t.Fatalf("choices.0.message.content = %q, want %q; payload=%s", gotContent, "ok", string(resp.Payload))
	}
}

func TestCodexExecutorExecuteStreamZeroUsageCompletionForwardsCompleted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_empty\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"thinking\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_empty\",\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello","stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var got bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		got.Write(chunk.Payload)
	}
	if !bytes.Contains(got.Bytes(), []byte("response.completed")) {
		t.Fatalf("response.completed was not forwarded; got %s", got.String())
	}
}

func TestCodexExecutorExecuteStreamZeroUsageCompletionWithOutputPasses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_tool\",\"object\":\"response\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"tool_search\",\"arguments\":\"{\\\"query\\\":\\\"ws\\\"}\"}],\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello","stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var got bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		got.Write(chunk.Payload)
	}
	if !bytes.Contains(got.Bytes(), []byte("response.completed")) {
		t.Fatalf("response.completed was not forwarded; got %s", got.String())
	}
}

func TestCodexExecutorExecuteStreamSynthesizesCompletionAfterOutputEOF(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_partial\",\"created_at\":1775555723,\"model\":\"gpt-5.5\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial answer\"}\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello","stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var got bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		got.Write(chunk.Payload)
	}
	if !bytes.Contains(got.Bytes(), []byte("response.output_text.delta")) {
		t.Fatalf("expected output delta before synthesized completion; got %s", got.String())
	}
	if !bytes.Contains(got.Bytes(), []byte("response.completed")) {
		t.Fatalf("expected synthesized response.completed; got %s", got.String())
	}
	if !bytes.Contains(got.Bytes(), []byte("partial answer")) {
		t.Fatalf("expected synthesized completion to retain output text; got %s", got.String())
	}
	if !bytes.Contains(got.Bytes(), []byte(`"type":"message"`)) {
		t.Fatalf("expected synthesized completion output message; got %s", got.String())
	}
	if bytes.Contains(got.Bytes(), []byte(`"usage"`)) {
		t.Fatalf("synthetic completion must not invent usage; got %s", got.String())
	}
}

func TestCodexExecutorExecuteStreamSynthesizesCompletionAfterReasoningEOF(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_reasoning_partial\",\"created_at\":1775555723,\"model\":\"gpt-5.5\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"thinking only\"}\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello","stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var got bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		got.Write(chunk.Payload)
	}
	if !bytes.Contains(got.Bytes(), []byte("response.reasoning_summary_text.delta")) {
		t.Fatalf("expected reasoning delta before synthesized completion; got %s", got.String())
	}
	if !bytes.Contains(got.Bytes(), []byte("response.completed")) {
		t.Fatalf("expected synthesized response.completed; got %s", got.String())
	}
	if !bytes.Contains(got.Bytes(), []byte("thinking only")) {
		t.Fatalf("expected synthesized completion to retain reasoning text; got %s", got.String())
	}
	if !bytes.Contains(got.Bytes(), []byte(`"type":"reasoning"`)) {
		t.Fatalf("expected synthesized completion reasoning output; got %s", got.String())
	}
}

func TestCodexExecutorExecuteStreamErrorsWhenStreamClosesBeforeCompletedWithoutContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_incomplete\"}}\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello","stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var got bytes.Buffer
	var gotErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			gotErr = chunk.Err
			continue
		}
		got.Write(chunk.Payload)
	}
	if !bytes.Contains(got.Bytes(), []byte("response.created")) {
		t.Fatalf("expected response.created before failure; got %s", got.String())
	}
	if gotErr == nil {
		t.Fatal("expected terminal stream error when response.completed is missing")
	}
	errResult, ok := gotErr.(*cliproxyauth.Error)
	if !ok {
		t.Fatalf("expected *cliproxyauth.Error, got %T: %v", gotErr, gotErr)
	}
	if errResult.Code != "request_scoped_auth_unavailable" {
		t.Fatalf("Code = %q, want request_scoped_auth_unavailable", errResult.Code)
	}
	if got := errResult.StatusCode(); got != http.StatusRequestTimeout {
		t.Fatalf("StatusCode = %d, want %d", got, http.StatusRequestTimeout)
	}
	if !bytes.Contains([]byte(errResult.Message), []byte("stream disconnected before completion")) {
		t.Fatalf("unexpected error message: %v", errResult.Message)
	}
}

func TestCodexExecutorExecuteStreamCyberPolicyReturnsNonRetryableSanitizedError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"cyber_policy\",\"message\":\"This content was flagged for possible cybersecurity risk. To get authorized for security work, join the Trusted Access for Cyber program: https://chatgpt.com/cyber\"}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello","stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var gotErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			gotErr = chunk.Err
		}
	}
	if gotErr == nil {
		t.Fatal("expected cyber policy error chunk")
	}
	statusCoder, ok := gotErr.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("expected status coder error, got %T: %v", gotErr, gotErr)
	}
	if got := statusCoder.StatusCode(); got != http.StatusBadRequest {
		t.Fatalf("StatusCode = %d, want %d", got, http.StatusBadRequest)
	}
	if bytes.Contains([]byte(gotErr.Error()), []byte("Trusted Access")) || bytes.Contains([]byte(gotErr.Error()), []byte("chatgpt.com/cyber")) {
		t.Fatalf("cyber policy error leaked upstream message: %s", gotErr.Error())
	}
	if got := gjson.Get(gotErr.Error(), "error.message").String(); got != "request rejected by safety system" {
		t.Fatalf("error.message = %q", got)
	}
}
