package usage

import (
	"context"
	"testing"
	"time"
)

func TestManagerStartStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	manager := NewManager(1)
	manager.Start(ctx)
	cancel()

	deadline := time.After(time.Second)
	for {
		manager.mu.Lock()
		closed := manager.closed
		manager.mu.Unlock()
		if closed {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("manager did not close after context cancellation")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}
