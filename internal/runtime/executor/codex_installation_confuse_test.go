package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// TestApplyCodexInstallationConfuse verifies the per-account remap of
// client_metadata.x-codex-installation-id: off by default, deterministic and
// account-distinct when enabled, and a no-op when the field is absent.
func TestApplyCodexInstallationConfuse(t *testing.T) {
	const orig = "real-install-123"
	body := []byte(`{"model":"gpt-5.5","client_metadata":{"x-codex-installation-id":"` + orig + `"},"input":[]}`)
	authA := &cliproxyauth.Auth{ID: "acct-A"}
	authB := &cliproxyauth.Auth{ID: "acct-B"}
	on := &config.Config{CodexConfuseInstallationID: true}
	off := &config.Config{CodexConfuseInstallationID: false}

	get := func(b []byte) string {
		return gjson.GetBytes(b, "client_metadata.x-codex-installation-id").String()
	}

	if got := get(applyCodexInstallationConfuse(off, authA, body)); got != orig {
		t.Fatalf("flag off must not change installation id, got %q", got)
	}

	gotA := get(applyCodexInstallationConfuse(on, authA, body))
	if gotA == "" || gotA == orig {
		t.Fatalf("flag on must remap installation id to a different value, got %q", gotA)
	}
	if get(applyCodexInstallationConfuse(on, authA, body)) != gotA {
		t.Fatalf("remap must be deterministic for the same auth")
	}
	if get(applyCodexInstallationConfuse(on, authB, body)) == gotA {
		t.Fatalf("different auth must produce a different confused installation id")
	}

	plain := []byte(`{"model":"gpt-5.5","input":[]}`)
	if gjson.GetBytes(applyCodexInstallationConfuse(on, authA, plain), "client_metadata.x-codex-installation-id").Exists() {
		t.Fatalf("must not add an installation id when none was present")
	}

	if got := get(applyCodexInstallationConfuse(on, nil, body)); got != orig {
		t.Fatalf("nil auth must leave body unchanged, got %q", got)
	}
	if got := get(applyCodexInstallationConfuse(on, &cliproxyauth.Auth{ID: "  "}, body)); got != orig {
		t.Fatalf("blank auth id must leave body unchanged, got %q", got)
	}
}
