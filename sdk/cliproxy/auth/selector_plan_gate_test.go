package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func fakeCodexIDToken(planType string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := fmt.Sprintf(`{"https://api.openai.com/auth":{"chatgpt_plan_type":%q,"chatgpt_account_id":"acc_1"}}`, planType)
	body := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return header + "." + body + ".sig"
}

func TestHydrateCodexPlanType_ParsesIDToken(t *testing.T) {
	t.Parallel()

	auth := &Auth{
		ID:       "a",
		Provider: "codex",
		Metadata: map[string]any{
			metadataCodexIDToken: fakeCodexIDToken("plus"),
		},
	}

	hydrateCodexPlanType(auth)

	if got, _ := auth.Metadata[metadataPlanTypeKey].(string); got != "plus" {
		t.Fatalf("auth.Metadata[%q] = %q, want %q", metadataPlanTypeKey, got, "plus")
	}
}

func TestIsAuthBlockedForModel_CodexFreeBlockedForGPT54(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auth := &Auth{
		ID:       "free",
		Provider: "codex",
		Metadata: map[string]any{
			metadataPlanTypeKey: "free",
		},
	}

	blocked, reason, next := isAuthBlockedForModel(auth, "gpt-5.4(xhigh)", now)
	if !blocked {
		t.Fatalf("blocked = false, want true")
	}
	if reason != blockReasonPlanIneligible {
		t.Fatalf("reason = %v, want %v", reason, blockReasonPlanIneligible)
	}
	if !next.IsZero() {
		t.Fatalf("next = %v, want zero", next)
	}
}

func TestIsAuthBlockedForModel_CodexPaidAllowedForGPT54(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auth := &Auth{
		ID:       "paid",
		Provider: "codex",
		Metadata: map[string]any{
			metadataPlanTypeKey: "team",
		},
	}

	blocked, reason, next := isAuthBlockedForModel(auth, "gpt-5.4", now)
	if blocked {
		t.Fatalf("blocked = true, want false (reason=%v next=%v)", reason, next)
	}
}

func TestSelectorPick_AllPlanIneligibleReturnsAuthPlanRestricted(t *testing.T) {
	t.Parallel()

	selector := &FillFirstSelector{}
	auths := []*Auth{
		{ID: "a", Provider: "codex", Metadata: map[string]any{metadataPlanTypeKey: "free"}},
		{ID: "b", Provider: "codex", Metadata: map[string]any{metadataPlanTypeKey: "free"}},
	}

	_, err := selector.Pick(context.Background(), "codex", "gpt-5.4", cliproxyexecutor.Options{}, auths)
	if err == nil {
		t.Fatalf("Pick() error = nil")
	}

	var authErr *Error
	if !errors.As(err, &authErr) {
		t.Fatalf("Pick() error = %T, want *Error", err)
	}
	if authErr.Code != "auth_plan_restricted" {
		t.Fatalf("error.Code = %q, want %q", authErr.Code, "auth_plan_restricted")
	}
	if authErr.HTTPStatus != 403 {
		t.Fatalf("error.HTTPStatus = %d, want %d", authErr.HTTPStatus, 403)
	}
}

func TestSelectorPick_PlanIneligibleDoesNotBlockCooldownError(t *testing.T) {
	t.Parallel()

	now := time.Now()
	next := now.Add(30 * time.Second)

	selector := &FillFirstSelector{}
	auths := []*Auth{
		{ID: "free", Provider: "codex", Metadata: map[string]any{metadataPlanTypeKey: "free"}},
		{ID: "paid", Provider: "codex", Metadata: map[string]any{metadataPlanTypeKey: "team"}, Quota: QuotaState{Exceeded: true, NextRecoverAt: next}},
	}

	_, err := selector.Pick(context.Background(), "codex", "gpt-5.4", cliproxyexecutor.Options{}, auths)
	if err == nil {
		t.Fatalf("Pick() error = nil")
	}

	var cooldownErr *modelCooldownError
	if !errors.As(err, &cooldownErr) {
		t.Fatalf("Pick() error = %T, want *modelCooldownError", err)
	}
}

func TestSelectorPick_CodexPrefersFreeForNonRestrictedGPTModels(t *testing.T) {
	t.Parallel()

	selector := &FillFirstSelector{}
	auths := []*Auth{
		{ID: "paid", Provider: "codex", Attributes: map[string]string{"priority": "10"}, Metadata: map[string]any{metadataPlanTypeKey: "team"}},
		{ID: "free", Provider: "codex", Attributes: map[string]string{"priority": "0"}, Metadata: map[string]any{metadataPlanTypeKey: "free"}},
	}

	got, err := selector.Pick(context.Background(), "codex", "gpt-5.2", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil {
		t.Fatalf("Pick() auth = nil")
	}
	if got.ID != "free" {
		t.Fatalf("Pick() auth.ID = %q, want %q", got.ID, "free")
	}
}
