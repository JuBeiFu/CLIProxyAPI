package synthesizer

import (
	"strings"
	"testing"
	"time"
)

func TestSynthesizeAuthFile_CodexSessionUsesTopLevelPlanAndAccountID(t *testing.T) {
	t.Parallel()

	ctx := &SynthesisContext{
		AuthDir: t.TempDir(),
		Now:     time.Now().UTC(),
	}
	fullPath := ctx.AuthDir + `\codex-session-user@example.com-plus.json`
	data := []byte(`{"type":"codex_session","email":"codex-session-user@example.com","plan_type":"plus","account_id":"acct_session_123"}`)

	auths := SynthesizeAuthFile(ctx, fullPath, data)
	if len(auths) != 1 {
		t.Fatalf("len(auths) = %d, want 1", len(auths))
	}
	auth := auths[0]
	if got := strings.TrimSpace(auth.Provider); got != "codex" {
		t.Fatalf("provider = %q, want %q", got, "codex")
	}
	if got := strings.TrimSpace(auth.Attributes["plan_type"]); got != "plus" {
		t.Fatalf("attributes.plan_type = %q, want %q", got, "plus")
	}
	if got, _ := auth.Metadata["account_id"].(string); got != "acct_session_123" {
		t.Fatalf("metadata.account_id = %q, want %q", got, "acct_session_123")
	}
	if got, _ := auth.Metadata["chatgpt_account_id"].(string); got != "acct_session_123" {
		t.Fatalf("metadata.chatgpt_account_id = %q, want %q", got, "acct_session_123")
	}
}
