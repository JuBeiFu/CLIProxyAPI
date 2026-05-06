package authlab

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestGenerateCodexAuthLabFilesCreatesHeaderTaggedCodexAuths(t *testing.T) {
	dir := t.TempDir()
	files, err := generateCodexAuthLabFiles(dir, "http://127.0.0.1:18080", []string{"fast", "slow-first-byte", "intermittent-429"})
	if err != nil {
		t.Fatalf("generateCodexAuthLabFiles() error = %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("file count = %d, want 3", len(files))
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, `"base_url":"http://127.0.0.1:18080"`) {
		t.Fatalf("missing base_url in %s", text)
	}
	if !strings.Contains(text, `"X-Mock-Auth-ID":"auth-0001"`) {
		t.Fatalf("missing auth marker in %s", text)
	}
}

func TestRunCodexAuthLabContrastsRoundRobinAndFillFirst(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	roundRobin, err := RunCodexAuthLab(ctx, CodexAuthLabOptions{
		WorkDir:         t.TempDir(),
		AuthCount:       3,
		Requests:        12,
		Concurrency:     1,
		RoutingStrategy: "round-robin",
		Scenario:        "fast-complete",
		Profiles:        []string{"fast", "fast", "fast"},
		APIKey:          "lab-key",
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunCodexAuthLab(round-robin) error = %v", err)
	}

	fillFirst, err := RunCodexAuthLab(ctx, CodexAuthLabOptions{
		WorkDir:         t.TempDir(),
		AuthCount:       3,
		Requests:        12,
		Concurrency:     1,
		RoutingStrategy: "fill-first",
		Scenario:        "fast-complete",
		Profiles:        []string{"fast", "fast", "fast"},
		APIKey:          "lab-key",
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunCodexAuthLab(fill-first) error = %v", err)
	}

	if roundRobin.HottestAuthShare >= fillFirst.HottestAuthShare {
		t.Fatalf("round-robin hottest share = %.2f, fill-first hottest share = %.2f; want round-robin lower", roundRobin.HottestAuthShare, fillFirst.HottestAuthShare)
	}
	if fillFirst.HottestAuthID != "auth-0001" {
		t.Fatalf("fill-first hottest auth = %q, want auth-0001", fillFirst.HottestAuthID)
	}
}

func TestAuthLabReportsPrimaryStandbyHedgeWins(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := RunCodexAuthLab(ctx, CodexAuthLabOptions{
		WorkDir:          t.TempDir(),
		AuthCount:        2,
		Requests:         6,
		Concurrency:      1,
		RoutingStrategy:  "round-robin",
		Scenario:         "fast-complete",
		CodexRouteHedge:  true,
		HedgeAfter:       50 * time.Millisecond,
		MockProfileNames: []string{"slow-first-byte", "fast"},
		APIKey:           "lab-key",
		Timeout:          5 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunCodexAuthLab(hedge) error = %v", err)
	}
	if result.RouteSummary.HedgeTriggered == 0 {
		t.Fatal("HedgeTriggered = 0, want > 0")
	}
	if result.RouteSummary.StandbyWins == 0 {
		t.Fatal("StandbyWins = 0, want > 0")
	}
}
