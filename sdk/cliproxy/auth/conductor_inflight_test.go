package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestManager_Execute_PrefersPreviouslySelectedAuthWithinExecutionSession(t *testing.T) {
	manager := newInflightTestManager(t)
	selected := ""

	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "session-affinity",
			cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
				selected = authID
			},
		},
	}
	resp1, err := manager.Execute(context.Background(), []string{"test"}, cliproxyexecutor.Request{Model: "gpt-5"}, opts)
	if err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	if string(resp1.Payload) != `{"ok":true}` {
		t.Fatalf("first Execute() payload = %s", string(resp1.Payload))
	}
	if selected != "auth-a" {
		t.Fatalf("first selected auth = %v, want auth-a", selected)
	}

	selected = ""
	opts2 := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "session-affinity",
			cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
				selected = authID
			},
		},
	}
	resp2, err := manager.Execute(context.Background(), []string{"test"}, cliproxyexecutor.Request{Model: "gpt-5"}, opts2)
	if err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}
	if string(resp2.Payload) != `{"ok":true}` {
		t.Fatalf("second Execute() payload = %s", string(resp2.Payload))
	}
	if selected != "auth-a" {
		t.Fatalf("second selected auth = %v, want auth-a", selected)
	}
}

func TestManager_Execute_FallsBackWhenSessionAffinityAuthRemoved(t *testing.T) {
	store := &deletingStore{}
	manager := NewManager(store, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy:           "fill-first",
			MaxInflightPerAuth: 1,
		},
	})
	manager.RegisterExecutor(&inflightRecordingExecutor{id: "test"})

	for _, authID := range []string{"auth-a", "auth-b"} {
		registerSchedulerModels(t, "test", "gpt-5", authID)
		auth := &Auth{
			ID:       authID,
			FileName: "auths/" + authID + ".json",
			Provider: "test",
			Attributes: map[string]string{
				"path": "/tmp/" + authID,
			},
			Metadata: map[string]any{
				"type": "test",
			},
		}
		if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
			t.Fatalf("register %s: %v", authID, err)
		}
	}

	selected := ""
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "session-rebind",
			cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
				selected = authID
			},
		},
	}
	if _, err := manager.Execute(context.Background(), []string{"test"}, cliproxyexecutor.Request{Model: "gpt-5"}, opts); err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	if selected != "auth-a" {
		t.Fatalf("first selected auth = %v, want auth-a", selected)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   "auth-a",
		Provider: "test",
		Model:    "gpt-5",
		Success:  false,
		Error: &Error{
			HTTPStatus: 401,
			Message:    "token revoked",
		},
	})

	selected = ""
	opts2 := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "session-rebind",
			cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
				selected = authID
			},
		},
	}
	if _, err := manager.Execute(context.Background(), []string{"test"}, cliproxyexecutor.Request{Model: "gpt-5"}, opts2); err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}
	if selected != "auth-b" {
		t.Fatalf("second selected auth = %v, want auth-b", selected)
	}
}

func TestManager_Execute_FallsBackWhenSessionAffinityAuthDisabled(t *testing.T) {
	manager := newInflightTestManager(t)
	selected := ""

	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "session-disabled",
			cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
				selected = authID
			},
		},
	}
	if _, err := manager.Execute(context.Background(), []string{"test"}, cliproxyexecutor.Request{Model: "gpt-5"}, opts); err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	if selected != "auth-a" {
		t.Fatalf("first selected auth = %v, want auth-a", selected)
	}

	authA, ok := manager.GetByID("auth-a")
	if !ok || authA == nil {
		t.Fatal("expected auth-a to exist")
	}
	authA.Disabled = true
	if _, err := manager.Update(context.Background(), authA); err != nil {
		t.Fatalf("disable auth-a: %v", err)
	}

	selected = ""
	opts2 := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "session-disabled",
			cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
				selected = authID
			},
		},
	}
	if _, err := manager.Execute(context.Background(), []string{"test"}, cliproxyexecutor.Request{Model: "gpt-5"}, opts2); err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}
	if selected != "auth-b" {
		t.Fatalf("second selected auth = %v, want auth-b", selected)
	}
}

func TestManager_ExecutionSessionAffinity_ExpiresAfterOneHour(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy:           "round-robin",
			MaxInflightPerAuth: 1,
		},
	})
	manager.RegisterExecutor(&inflightRecordingExecutor{id: "test"})
	for _, authID := range []string{"auth-a", "auth-b"} {
		registerSchedulerModels(t, "test", "gpt-5", authID)
		if _, err := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: authID, Provider: "test"}); err != nil {
			t.Fatalf("register %s: %v", authID, err)
		}
	}

	selected := ""
	baseNow := time.Date(2026, 3, 25, 21, 0, 0, 0, time.UTC)
	prevNow := executionSessionAffinityNowFunc
	executionSessionAffinityNowFunc = func() time.Time { return baseNow }
	t.Cleanup(func() { executionSessionAffinityNowFunc = prevNow })

	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "session-ttl",
			cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
				selected = authID
			},
		},
	}
	if _, err := manager.Execute(context.Background(), []string{"test"}, cliproxyexecutor.Request{Model: "gpt-5"}, opts); err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	if selected != "auth-a" {
		t.Fatalf("first selected auth = %v, want auth-a", selected)
	}

	baseNow = baseNow.Add(30 * time.Minute)
	selected = ""
	midOpts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "session-ttl",
			cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
				selected = authID
			},
		},
	}
	if _, err := manager.Execute(context.Background(), []string{"test"}, cliproxyexecutor.Request{Model: "gpt-5"}, midOpts); err != nil {
		t.Fatalf("mid Execute() error = %v", err)
	}
	if selected != "auth-a" {
		t.Fatalf("mid selected auth = %v, want auth-a before ttl expiry", selected)
	}

	controlSelected := ""
	controlOpts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "control-session",
			cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
				controlSelected = authID
			},
		},
	}
	if _, err := manager.Execute(context.Background(), []string{"test"}, cliproxyexecutor.Request{Model: "gpt-5"}, controlOpts); err != nil {
		t.Fatalf("control Execute() error = %v", err)
	}
	if controlSelected != "auth-b" {
		t.Fatalf("control selected auth = %v, want auth-b", controlSelected)
	}

	baseNow = time.Date(2026, 3, 25, 22, 1, 0, 0, time.UTC)
	if got := manager.executionSessionPreferredAuthID("session-ttl"); got != "" {
		t.Fatalf("executionSessionPreferredAuthID() after ttl = %q, want empty", got)
	}
	selected = ""
	opts2 := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "session-ttl",
			cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
				selected = authID
			},
		},
	}
	if _, err := manager.Execute(context.Background(), []string{"test"}, cliproxyexecutor.Request{Model: "gpt-5"}, opts2); err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}
	if selected == "" {
		t.Fatal("second selected auth after ttl = empty, want normal selection to proceed after affinity expiry")
	}
}

type inflightRecordingExecutor struct {
	id string

	mu      sync.Mutex
	authIDs []string
}

func (e *inflightRecordingExecutor) Identifier() string { return e.id }

func (e *inflightRecordingExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *inflightRecordingExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *inflightRecordingExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *inflightRecordingExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *inflightRecordingExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func newInflightTestManager(t *testing.T) *Manager {
	t.Helper()

	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy:           "fill-first",
			MaxInflightPerAuth: 1,
		},
	})
	manager.RegisterExecutor(&inflightRecordingExecutor{id: "test"})

	for _, authID := range []string{"auth-a", "auth-b"} {
		registerSchedulerModels(t, "test", "gpt-5", authID)
		if _, err := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: authID, Provider: "test"}); err != nil {
			t.Fatalf("register %s: %v", authID, err)
		}
	}
	return manager
}

func TestManager_PickNextMixedReserved_RespectsMaxInflightPerAuth(t *testing.T) {
	manager := newInflightTestManager(t)

	auth1, _, _, release1, err := manager.pickNextMixedReserved(context.Background(), []string{"test"}, "gpt-5", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("first pickNextMixedReserved error = %v", err)
	}
	if auth1 == nil || auth1.ID != "auth-a" {
		t.Fatalf("first auth = %v, want auth-a", auth1)
	}
	if release1 == nil {
		t.Fatal("first release = nil")
	}

	auth2, _, _, release2, err := manager.pickNextMixedReserved(context.Background(), []string{"test"}, "gpt-5", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("second pickNextMixedReserved error = %v", err)
	}
	if auth2 == nil || auth2.ID != "auth-b" {
		t.Fatalf("second auth = %v, want auth-b", auth2)
	}
	if release2 == nil {
		t.Fatal("second release = nil")
	}

	if _, _, _, release3, err := manager.pickNextMixedReserved(context.Background(), []string{"test"}, "gpt-5", cliproxyexecutor.Options{}, nil); err == nil {
		if release3 != nil {
			release3()
		}
		t.Fatal("third pickNextMixedReserved error = nil, want auth_busy")
	} else {
		authErr, ok := err.(*Error)
		if !ok || authErr.Code != "auth_busy" {
			t.Fatalf("third pickNextMixedReserved error = %#v, want auth_busy", err)
		}
	}

	release2()

	auth4, _, _, release4, err := manager.pickNextMixedReserved(context.Background(), []string{"test"}, "gpt-5", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("fourth pickNextMixedReserved error = %v", err)
	}
	if auth4 == nil || auth4.ID != "auth-b" {
		t.Fatalf("fourth auth = %v, want auth-b while auth-a remains busy", auth4)
	}
	release4()
	release1()

	auth5, _, _, release5, err := manager.pickNextMixedReserved(context.Background(), []string{"test"}, "gpt-5", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("fifth pickNextMixedReserved error = %v", err)
	}
	if auth5 == nil || auth5.ID != "auth-a" {
		t.Fatalf("fifth auth = %v, want auth-a after release", auth5)
	}
	release5()
}

func TestManager_ExecuteMixedOnce_SkipsBusyAuthWithoutConsumingRetryCredentialBudget(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy:           "fill-first",
			MaxInflightPerAuth: 1,
		},
	})

	executor := &inflightRecordingExecutor{id: "test"}
	manager.RegisterExecutor(executor)
	for _, authID := range []string{"auth-a", "auth-b"} {
		registerSchedulerModels(t, "test", "gpt-5", authID)
		if _, err := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: authID, Provider: "test"}); err != nil {
			t.Fatalf("register %s: %v", authID, err)
		}
	}

	heldAuth, _, _, release, err := manager.pickNextMixedReserved(context.Background(), []string{"test"}, "gpt-5", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("reserve held auth: %v", err)
	}
	if heldAuth == nil || heldAuth.ID != "auth-a" {
		t.Fatalf("held auth = %v, want auth-a", heldAuth)
	}
	defer release()

	resp, err := manager.executeMixedOnce(context.Background(), []string{"test"}, cliproxyexecutor.Request{Model: "gpt-5"}, cliproxyexecutor.Options{}, 1)
	if err != nil {
		t.Fatalf("executeMixedOnce() error = %v", err)
	}
	if string(resp.Payload) != `{"ok":true}` {
		t.Fatalf("executeMixedOnce() payload = %s", string(resp.Payload))
	}
}

func TestManager_PickNextMixedReserved_AllBusyCanOverflowWhenConfigured(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy:                           "fill-first",
			MaxInflightPerAuth:                 1,
			AllowInflightOverflowWhenExhausted: true,
		},
	})
	manager.RegisterExecutor(&inflightRecordingExecutor{id: "test"})

	for _, authID := range []string{"auth-a", "auth-b"} {
		registerSchedulerModels(t, "test", "gpt-5", authID)
		if _, err := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: authID, Provider: "test"}); err != nil {
			t.Fatalf("register %s: %v", authID, err)
		}
	}

	auth1, _, _, release1, err := manager.pickNextMixedReserved(context.Background(), []string{"test"}, "gpt-5", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("first pickNextMixedReserved error = %v", err)
	}
	if auth1 == nil || auth1.ID != "auth-a" {
		t.Fatalf("first auth = %v, want auth-a", auth1)
	}
	defer release1()

	auth2, _, _, release2, err := manager.pickNextMixedReserved(context.Background(), []string{"test"}, "gpt-5", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("second pickNextMixedReserved error = %v", err)
	}
	if auth2 == nil || auth2.ID != "auth-b" {
		t.Fatalf("second auth = %v, want auth-b", auth2)
	}
	defer release2()

	auth3, _, _, release3, err := manager.pickNextMixedReserved(context.Background(), []string{"test"}, "gpt-5", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("third pickNextMixedReserved error = %v", err)
	}
	if auth3 == nil || auth3.ID != "auth-a" {
		t.Fatalf("third auth = %v, want auth-a after overflow fallback", auth3)
	}
	if release3 == nil {
		t.Fatal("third release = nil")
	}
	release3()
}

func TestManager_ExecuteMixedOnce_AllBusyCanOverflowWhenConfigured(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy:                           "fill-first",
			MaxInflightPerAuth:                 1,
			AllowInflightOverflowWhenExhausted: true,
		},
	})

	executor := &inflightRecordingExecutor{id: "test"}
	manager.RegisterExecutor(executor)
	registerSchedulerModels(t, "test", "gpt-5", "auth-a")
	if _, err := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: "auth-a", Provider: "test"}); err != nil {
		t.Fatalf("register auth-a: %v", err)
	}

	heldAuth, _, _, release, err := manager.pickNextMixedReserved(context.Background(), []string{"test"}, "gpt-5", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("reserve held auth: %v", err)
	}
	if heldAuth == nil || heldAuth.ID != "auth-a" {
		t.Fatalf("held auth = %v, want auth-a", heldAuth)
	}
	defer release()

	resp, err := manager.executeMixedOnce(context.Background(), []string{"test"}, cliproxyexecutor.Request{Model: "gpt-5"}, cliproxyexecutor.Options{}, 1)
	if err != nil {
		t.Fatalf("executeMixedOnce() error = %v", err)
	}
	if string(resp.Payload) != `{"ok":true}` {
		t.Fatalf("executeMixedOnce() payload = %s", string(resp.Payload))
	}
}

func TestAttachStreamResultRelease_ReleasesAfterStreamDrain(t *testing.T) {
	in := make(chan cliproxyexecutor.StreamChunk, 1)
	in <- cliproxyexecutor.StreamChunk{Payload: []byte("hello")}
	close(in)

	released := make(chan struct{})
	stream := attachStreamResultRelease(context.Background(), &cliproxyexecutor.StreamResult{
		Headers: http.Header{"X-Test": {"ok"}},
		Chunks:  in,
	}, func() {
		close(released)
	})

	if got := stream.Headers.Get("X-Test"); got != "ok" {
		t.Fatalf("stream header = %q, want ok", got)
	}

	count := 0
	for range stream.Chunks {
		count++
	}
	if count != 1 {
		t.Fatalf("chunk count = %d, want 1", count)
	}

	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("release callback was not invoked after stream drain")
	}
}
