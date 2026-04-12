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
