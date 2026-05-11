package auth

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestManagerExecute_SessionAffinityPinsAuthAcrossRequests(t *testing.T) {
	t.Parallel()

	const (
		model   = "gpt-5.4"
		authAID = "session-affinity-auth-a"
		authBID = "session-affinity-auth-b"
	)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &responseStickyExecutor{id: "codex"}
	manager.RegisterExecutor(executor)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authAID, "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(authBID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(authAID)
		reg.UnregisterClient(authBID)
	})

	for _, auth := range []*Auth{
		{ID: authAID, Provider: "codex"},
		{ID: authBID, Provider: "codex"},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, err)
		}
	}

	req := cliproxyexecutor.Request{
		Model:   model,
		Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`),
	}
	newOpts := func() cliproxyexecutor.Options {
		return cliproxyexecutor.Options{
			Metadata: map[string]any{
				"auth_session_id": "session-1",
			},
		}
	}

	if _, err := manager.Execute(context.Background(), []string{"codex"}, req, newOpts()); err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	if _, err := manager.Execute(context.Background(), []string{"codex"}, req, newOpts()); err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}

	calls := executor.ExecuteCalls()
	if len(calls) != 2 {
		t.Fatalf("Execute calls = %v, want 2 calls", calls)
	}
	if calls[0] != calls[1] {
		t.Fatalf("Execute auth sequence = %v, want same auth reused for sticky session", calls)
	}
}

func TestManagerExecute_SessionRetryAttemptExcludesBoundAuth(t *testing.T) {
	t.Parallel()

	const (
		model   = "gpt-5.4"
		authAID = "session-retry-auth-a"
		authBID = "session-retry-auth-b"
	)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &responseStickyExecutor{id: "codex"}
	manager.RegisterExecutor(executor)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authAID, "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(authBID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(authAID)
		reg.UnregisterClient(authBID)
	})

	for _, auth := range []*Auth{
		{ID: authAID, Provider: "codex"},
		{ID: authBID, Provider: "codex"},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, err)
		}
	}

	req := cliproxyexecutor.Request{
		Model:   model,
		Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`),
	}
	baseMeta := map[string]any{
		cliproxyexecutor.AuthSessionMetadataKey: "session-1",
	}

	if _, err := manager.Execute(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{Metadata: baseMeta}); err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}

	var secondSelectedAuth string
	if _, err := manager.Execute(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.AuthSessionMetadataKey:          "session-1",
			cliproxyexecutor.ExternalRetryAttemptMetadataKey: 1,
			cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
				secondSelectedAuth = authID
			},
		},
	}); err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}

	calls := executor.ExecuteCalls()
	if len(calls) != 2 {
		t.Fatalf("Execute calls = %v, want 2 calls", calls)
	}
	if calls[0] != authAID {
		t.Fatalf("first Execute auth = %q, want %q", calls[0], authAID)
	}
	if calls[1] != authBID {
		t.Fatalf("retry Execute auth = %q, selected = %q, want %q", calls[1], secondSelectedAuth, authBID)
	}
}
