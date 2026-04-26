package proxypool

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestHealthManagerStoresSnapshot(t *testing.T) {
	t.Parallel()

	manager := NewHealthManager()
	checkedAt := time.Unix(1_744_419_200, 0)
	manager.StoreResult("shared-egress", "proxy-a", ProbeResult{
		Healthy:    true,
		StatusCode: 204,
		Latency:    150 * time.Millisecond,
		CheckedAt:  checkedAt,
	})

	snapshot := manager.Snapshot()
	poolSnapshot, ok := snapshot["shared-egress"]
	if !ok {
		t.Fatal("expected shared-egress snapshot to exist")
	}
	entry, ok := poolSnapshot["proxy-a"]
	if !ok {
		t.Fatal("expected proxy-a snapshot to exist")
	}
	if !entry.Healthy {
		t.Fatal("expected proxy-a to be healthy")
	}
	if entry.StatusCode != 204 {
		t.Fatalf("expected status 204, got %d", entry.StatusCode)
	}
	if entry.Latency != 150*time.Millisecond {
		t.Fatalf("expected latency 150ms, got %s", entry.Latency)
	}
	if !entry.CheckedAt.Equal(checkedAt) {
		t.Fatalf("expected checked-at %s, got %s", checkedAt, entry.CheckedAt)
	}
}

func TestHealthManagerMarksUnprobedEntryUsable(t *testing.T) {
	t.Parallel()

	manager := NewHealthManager()
	result, ok := manager.Result("shared-egress", "proxy-a")
	if ok {
		t.Fatalf("expected no cached result, got %+v", result)
	}
	if !manager.IsUsable("shared-egress", "proxy-a") {
		t.Fatal("expected entry without probe result to remain usable")
	}
}

func TestHealthManagerPassiveSlowOutcomeTemporarilyMarksUnusable(t *testing.T) {
	t.Parallel()

	manager := NewHealthManager()
	now := time.Unix(1_744_419_200, 0)

	manager.ReportPassiveOutcome("shared-egress", "proxy-a", PassiveOutcome{
		Total:         2 * time.Minute,
		ReadBody:      95 * time.Second,
		ResponseBytes: 128 * 1024,
		StatusCode:    200,
		CheckedAt:     now,
	})
	if !manager.IsUsableAt("shared-egress", "proxy-a", now) {
		t.Fatal("expected a single slow passive sample to keep entry usable")
	}

	manager.ReportPassiveOutcome("shared-egress", "proxy-a", PassiveOutcome{
		Total:         130 * time.Second,
		ReadBody:      110 * time.Second,
		ResponseBytes: 64 * 1024,
		StatusCode:    200,
		CheckedAt:     now.Add(time.Second),
	})
	if manager.IsUsableAt("shared-egress", "proxy-a", now.Add(time.Second)) {
		t.Fatal("expected repeated slow passive samples to mark entry unusable")
	}
	result, ok := manager.Result("shared-egress", "proxy-a")
	if !ok {
		t.Fatal("expected passive result to be stored")
	}
	if !result.Passive {
		t.Fatalf("Passive = false, want true")
	}
	if result.UnhealthyUntil.IsZero() {
		t.Fatal("expected passive result to have UnhealthyUntil")
	}
	if !manager.IsUsableAt("shared-egress", "proxy-a", result.UnhealthyUntil.Add(time.Second)) {
		t.Fatal("expected passive cooldown expiry to restore usability")
	}
}

func TestHealthManagerPassiveHealthyOutcomeClearsPassiveCooldown(t *testing.T) {
	t.Parallel()

	manager := NewHealthManager()
	now := time.Unix(1_744_419_200, 0)
	for i := 0; i < DefaultPassiveSlowStrikes; i++ {
		manager.ReportPassiveOutcome("shared-egress", "proxy-a", PassiveOutcome{
			Total:         2 * time.Minute,
			ReadBody:      100 * time.Second,
			ResponseBytes: 64 * 1024,
			StatusCode:    200,
			CheckedAt:     now.Add(time.Duration(i) * time.Second),
		})
	}
	if manager.IsUsableAt("shared-egress", "proxy-a", now.Add(2*time.Second)) {
		t.Fatal("expected passive slow samples to mark entry unusable")
	}

	manager.ReportPassiveOutcome("shared-egress", "proxy-a", PassiveOutcome{
		Total:         8 * time.Second,
		ReadBody:      7 * time.Second,
		ResponseBytes: 512 * 1024,
		StatusCode:    200,
		CheckedAt:     now.Add(3 * time.Second),
	})
	if !manager.IsUsableAt("shared-egress", "proxy-a", now.Add(3*time.Second)) {
		t.Fatal("expected fast passive success to restore usability")
	}
}

func TestHealthManagerLastCheckedAtIgnoresPassiveOutcomes(t *testing.T) {
	t.Parallel()

	manager := NewHealthManager()
	now := time.Unix(1_744_419_200, 0)
	manager.ReportPassiveOutcome("shared-egress", "proxy-a", PassiveOutcome{
		Total:         5 * time.Second,
		ReadBody:      4 * time.Second,
		ResponseBytes: 512 * 1024,
		StatusCode:    200,
		CheckedAt:     now,
	})

	if got := manager.lastCheckedAt("shared-egress"); !got.IsZero() {
		t.Fatalf("lastCheckedAt = %s, want zero for passive-only result", got)
	}
}

func TestHealthManagerPassiveHighThroughputLongRequestStaysUsable(t *testing.T) {
	t.Parallel()

	manager := NewHealthManager()
	now := time.Unix(1_744_419_200, 0)
	for i := 0; i < DefaultPassiveSlowStrikes+1; i++ {
		manager.ReportPassiveOutcome("shared-egress", "proxy-a", PassiveOutcome{
			Total:         2 * time.Minute,
			ReadBody:      100 * time.Second,
			ResponseBytes: int64(DefaultPassiveSlowBytesPerSecond) * 100 * 2,
			StatusCode:    200,
			CheckedAt:     now.Add(time.Duration(i) * time.Second),
		})
	}
	if !manager.IsUsableAt("shared-egress", "proxy-a", now.Add(time.Minute)) {
		t.Fatal("expected high-throughput long request to keep entry usable")
	}
}

func TestHealthManagerPassiveUpstreamErrorStatusStaysUsable(t *testing.T) {
	t.Parallel()

	manager := NewHealthManager()
	now := time.Unix(1_744_419_200, 0)
	for i := 0; i < DefaultPassiveSlowStrikes+1; i++ {
		manager.ReportPassiveOutcome("shared-egress", "proxy-a", PassiveOutcome{
			Total:         2 * time.Minute,
			ReadBody:      100 * time.Second,
			ResponseBytes: 64 * 1024,
			StatusCode:    503,
			CheckedAt:     now.Add(time.Duration(i) * time.Second),
		})
	}
	if !manager.IsUsableAt("shared-egress", "proxy-a", now.Add(time.Minute)) {
		t.Fatal("expected upstream status errors to keep entry usable")
	}
}

func TestHealthManagerUnsupportedRegionTemporarilyMarksUnusable(t *testing.T) {
	t.Parallel()

	manager := NewHealthManager()
	now := time.Unix(1_744_419_200, 0)
	manager.ReportPassiveUnsupportedRegion("shared-egress", "proxy-a", PassiveOutcome{
		StatusCode: 403,
		Error:      "unsupported_country_region_territory",
		CheckedAt:  now,
	})

	if manager.IsUsableAt("shared-egress", "proxy-a", now) {
		t.Fatal("expected unsupported-region passive result to mark entry unusable")
	}
	result, ok := manager.Result("shared-egress", "proxy-a")
	if !ok {
		t.Fatal("expected unsupported-region result to be stored")
	}
	if result.Healthy {
		t.Fatal("expected result Healthy=false")
	}
	if result.StatusCode != 403 {
		t.Fatalf("status = %d, want 403", result.StatusCode)
	}
	if result.Error != "unsupported_country_region_territory" {
		t.Fatalf("error = %q, want unsupported_country_region_territory", result.Error)
	}
	if result.UnhealthyUntil.IsZero() {
		t.Fatal("expected unsupported-region result to have UnhealthyUntil")
	}
	if !manager.IsUsableAt("shared-egress", "proxy-a", result.UnhealthyUntil.Add(time.Second)) {
		t.Fatal("expected unsupported-region cooldown expiry to restore usability")
	}
}

func TestHealthManagerReusesProbeClientForEntry(t *testing.T) {
	t.Parallel()

	manager := NewHealthManager()
	pool := config.ProxyPool{
		Name:                      "shared-egress",
		HealthCheckTimeoutSeconds: 8,
	}
	entry := config.ProxyPoolEntry{
		Name: "proxy-a",
		URL:  "socks5://proxyuser:secret@127.0.0.1:1080",
	}

	first, err := manager.probeClientForEntry(pool, entry)
	if err != nil {
		t.Fatalf("first probe client failed: %v", err)
	}
	second, err := manager.probeClientForEntry(pool, entry)
	if err != nil {
		t.Fatalf("second probe client failed: %v", err)
	}
	if first != second {
		t.Fatal("expected probe client to be reused for same proxy and timeout")
	}
}

func TestHealthManagerCheckPoolProbesEntriesConcurrently(t *testing.T) {
	t.Parallel()

	manager := NewHealthManager()
	var active int32
	var maxActive int32
	manager.probeEntryFunc = func(_ context.Context, _ config.ProxyPool, _ config.ProxyPoolEntry) ProbeResult {
		current := atomic.AddInt32(&active, 1)
		for {
			recorded := atomic.LoadInt32(&maxActive)
			if current <= recorded {
				break
			}
			if atomic.CompareAndSwapInt32(&maxActive, recorded, current) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return ProbeResult{
			Healthy:    true,
			StatusCode: 204,
			CheckedAt:  time.Now(),
		}
	}

	pool := config.ProxyPool{
		Name: "shared-egress",
		Entries: []config.ProxyPoolEntry{
			{Name: "proxy-a", URL: "socks5://127.0.0.1:1080"},
			{Name: "proxy-b", URL: "socks5://127.0.0.2:1080"},
			{Name: "proxy-c", URL: "socks5://127.0.0.3:1080"},
		},
	}

	status := manager.CheckPool(context.Background(), pool)
	if len(status.Entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(status.Entries))
	}
	if atomic.LoadInt32(&maxActive) < 2 {
		t.Fatalf("expected probes to overlap, max concurrent probes = %d", maxActive)
	}
}
