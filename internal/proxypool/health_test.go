package proxypool

import (
	"testing"
	"time"
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
