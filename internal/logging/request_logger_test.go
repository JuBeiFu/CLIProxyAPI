package logging

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestFileRequestLoggerMaybeCleanupOldErrorLogsThrottlesBurst(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	var runs atomic.Int32
	logger := &FileRequestLogger{
		errorLogsMaxFiles:        10,
		errorLogsCleanupInterval: time.Hour,
		errorLogsCleanupNow: func() time.Time {
			return now
		},
		errorLogsCleanup: func() error {
			runs.Add(1)
			return nil
		},
	}

	if err := logger.maybeCleanupOldErrorLogs(); err != nil {
		t.Fatalf("first cleanup error = %v", err)
	}
	if err := logger.maybeCleanupOldErrorLogs(); err != nil {
		t.Fatalf("second cleanup error = %v", err)
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("cleanup runs after burst = %d, want 1", got)
	}

	now = now.Add(2 * time.Hour)
	if err := logger.maybeCleanupOldErrorLogs(); err != nil {
		t.Fatalf("third cleanup error = %v", err)
	}
	if got := runs.Load(); got != 2 {
		t.Fatalf("cleanup runs after interval = %d, want 2", got)
	}
}
