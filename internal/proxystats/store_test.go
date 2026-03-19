package proxystats

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStoreRecordAggregatesAttempts(t *testing.T) {
	store := NewStore(8)
	startedAt := time.Date(2026, 3, 12, 10, 0, 0, 0, time.UTC)
	completedAt := startedAt.Add(150 * time.Millisecond)

	store.Record(Attempt{
		Timestamp:           completedAt,
		StartedAt:           startedAt,
		CompletedAt:         completedAt,
		ProxyURL:            "socks5://user:pass@proxy-a:1080",
		ProxyDisplay:        RedactProxyURL("socks5://user:pass@proxy-a:1080"),
		ProxyProfile:        "warp-free",
		SelectionSource:     "proxy-routing-rule",
		Provider:            "codex",
		PlanType:            "free",
		AuthKind:            "oauth",
		StatusCode:          200,
		Success:             true,
		ResponseReceived:    true,
		FirstByteDurationMs: 40,
		TotalDurationMs:     150,
	})

	store.Record(Attempt{
		Timestamp:        completedAt.Add(time.Second),
		StartedAt:        startedAt.Add(time.Second),
		CompletedAt:      completedAt.Add(time.Second),
		ProxyURL:         "socks5://proxy-a:1080",
		ProxyDisplay:     "socks5://proxy-a:1080",
		ProxyProfile:     "warp-free",
		SelectionSource:  "proxy-routing-rule",
		Provider:         "codex",
		PlanType:         "free",
		AuthKind:         "oauth",
		Success:          false,
		ResponseReceived: false,
		TotalDurationMs:  200,
		Error:            "dial tcp timeout",
	})

	snapshot := store.Snapshot()
	if snapshot.TotalAttempts != 2 {
		t.Fatalf("snapshot.TotalAttempts = %d, want 2", snapshot.TotalAttempts)
	}
	if len(snapshot.Proxies) != 1 {
		t.Fatalf("len(snapshot.Proxies) = %d, want 1", len(snapshot.Proxies))
	}
	proxy := snapshot.Proxies[0]
	if proxy.SuccessCount != 1 || proxy.FailureCount != 1 {
		t.Fatalf("proxy success/failure = %d/%d, want 1/1", proxy.SuccessCount, proxy.FailureCount)
	}
	if proxy.TransportErrorCount != 1 {
		t.Fatalf("proxy.TransportErrorCount = %d, want 1", proxy.TransportErrorCount)
	}
	if proxy.FirstByteAvgMs != 40 {
		t.Fatalf("proxy.FirstByteAvgMs = %v, want 40", proxy.FirstByteAvgMs)
	}
}

func TestWrapResponseRecordsResponseLifecycle(t *testing.T) {
	DefaultStore().Reset()
	t.Cleanup(func() { DefaultStore().Reset() })

	startedAt := time.Now().Add(-120 * time.Millisecond)
	ctx := WithRequestMetadata(nil, RequestMetadata{
		ProxyProfile:    "paid-egress",
		SelectionSource: "auth-proxy-profile",
		Provider:        "claude",
		PlanType:        "pro",
		AuthKind:        "api-key",
		AuthIndex:       "idx-1",
	})
	resp := WrapResponse(ctx, "http://user:pass@proxy-b:8080", startedAt, &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader("hello world")),
	})

	buf := make([]byte, 5)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	snapshot := DefaultStore().Snapshot()
	if snapshot.TotalAttempts != 1 {
		t.Fatalf("snapshot.TotalAttempts = %d, want 1", snapshot.TotalAttempts)
	}
	if len(snapshot.Proxies) != 1 {
		t.Fatalf("len(snapshot.Proxies) = %d, want 1", len(snapshot.Proxies))
	}
	proxy := snapshot.Proxies[0]
	if proxy.ProxyURL != "http://proxy-b:8080" {
		t.Fatalf("proxy.ProxyURL = %q, want redacted proxy url", proxy.ProxyURL)
	}
	if proxy.ProxyProfile != "paid-egress" {
		t.Fatalf("proxy.ProxyProfile = %q, want paid-egress", proxy.ProxyProfile)
	}
	if proxy.ResponseCount != 1 || proxy.SuccessCount != 1 {
		t.Fatalf("proxy response/success = %d/%d, want 1/1", proxy.ResponseCount, proxy.SuccessCount)
	}
	if len(snapshot.Recent) != 1 {
		t.Fatalf("len(snapshot.Recent) = %d, want 1", len(snapshot.Recent))
	}
	if snapshot.Recent[0].Provider != "claude" {
		t.Fatalf("recent provider = %q, want claude", snapshot.Recent[0].Provider)
	}
}

func TestStorePreferredProxyOrder_PenalizesRecentTransportFailures(t *testing.T) {
	store := NewStore(8)
	now := time.Date(2026, 3, 19, 10, 0, 0, 0, time.UTC)

	store.Record(Attempt{
		Timestamp:        now.Add(-5 * time.Second),
		StartedAt:        now.Add(-6 * time.Second),
		CompletedAt:      now.Add(-5 * time.Second),
		ProxyURL:         "http://proxy-a:8080",
		ProxyDisplay:     "http://proxy-a:8080",
		Success:          false,
		ResponseReceived: false,
		TotalDurationMs:  100,
		Error:            "dial tcp timeout",
	})
	store.Record(Attempt{
		Timestamp:           now.Add(-3 * time.Second),
		StartedAt:           now.Add(-4 * time.Second),
		CompletedAt:         now.Add(-3 * time.Second),
		ProxyURL:            "http://proxy-b:8080",
		ProxyDisplay:        "http://proxy-b:8080",
		Success:             true,
		ResponseReceived:    true,
		StatusCode:          200,
		FirstByteDurationMs: 20,
		TotalDurationMs:     80,
	})

	ordered := store.PreferredProxyOrder([]string{"http://proxy-a:8080", "http://proxy-b:8080"}, 0, now)
	if got := strings.Join(ordered, ","); got != "http://proxy-b:8080,http://proxy-a:8080" {
		t.Fatalf("ordered proxies = %q, want healthy proxy first", got)
	}
}
