package executor

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/proxypool"
	log "github.com/sirupsen/logrus"
)

func TestLogSlowCodexUpstreamTimingIncludesAuthIDAndHTTPTraceFields(t *testing.T) {
	var buf bytes.Buffer
	origOut := log.StandardLogger().Out
	origLevel := log.StandardLogger().Level
	log.SetOutput(&buf)
	log.SetLevel(log.InfoLevel)
	defer log.SetOutput(origOut)
	defer log.SetLevel(origLevel)

	ctx := logging.WithRequestID(context.Background(), "req-timing-1")
	logSlowCodexUpstreamTiming(ctx, codexUpstreamTiming{
		endpoint:        "responses/compact",
		model:           "gpt-5.4",
		authID:          "auth-slow-1",
		proxySource:     "proxy-pool",
		proxyPool:       "free-egress",
		proxyName:       "free-proxy-7",
		proxyFallback:   true,
		status:          200,
		startedAt:       time.Now().Add(-65 * time.Second),
		prepare:         12 * time.Millisecond,
		httpDo:          64 * time.Second,
		readBody:        45 * time.Millisecond,
		translate:       time.Millisecond,
		traceConn:       10 * time.Millisecond,
		traceTLS:        20 * time.Millisecond,
		traceWroteReq:   30 * time.Millisecond,
		traceFirstByte:  63 * time.Second,
		traceWasIdle:    true,
		traceIdleTime:   2 * time.Second,
		bytesRead:       321,
		streamLines:     0,
		streamChunks:    0,
		streamErrText:   "",
		traceReusedConn: true,
	})

	out := buf.String()
	for _, part := range []string{
		"codex upstream timing",
		"request_id=req-timing-1",
		"auth_id=auth-slow-1",
		"endpoint=responses/compact",
		"http_conn=",
		"http_tls=",
		"http_wrote_req=",
		"http_first_byte=",
		"http_conn_reused=true",
		"http_conn_was_idle=true",
		"http_conn_idle=",
		"proxy_source=proxy-pool",
		"proxy_pool=free-egress",
		"proxy_name=free-proxy-7",
		"proxy_fallback_direct=true",
	} {
		if !strings.Contains(out, part) {
			t.Fatalf("log output missing %q: %s", part, out)
		}
	}
}

func TestLogSlowCodexUpstreamTimingSkipsFastRequests(t *testing.T) {
	var buf bytes.Buffer
	origOut := log.StandardLogger().Out
	origLevel := log.StandardLogger().Level
	log.SetOutput(&buf)
	log.SetLevel(log.InfoLevel)
	defer log.SetOutput(origOut)
	defer log.SetLevel(origLevel)

	logSlowCodexUpstreamTiming(context.Background(), codexUpstreamTiming{
		endpoint:  "responses",
		model:     "gpt-5.4",
		authID:    "auth-fast-1",
		status:    200,
		startedAt: time.Now().Add(-5 * time.Second),
		httpDo:    4 * time.Second,
		readBody:  500 * time.Millisecond,
		streamCompleted: true,
	})

	if got := buf.String(); got != "" {
		t.Fatalf("expected no log output for fast request, got %s", got)
	}
}

func TestLogSlowCodexUpstreamTimingLogsFastIncompleteStreamFailures(t *testing.T) {
	var buf bytes.Buffer
	origOut := log.StandardLogger().Out
	origLevel := log.StandardLogger().Level
	log.SetOutput(&buf)
	log.SetLevel(log.InfoLevel)
	defer log.SetOutput(origOut)
	defer log.SetLevel(origLevel)

	ctx := logging.WithRequestID(context.Background(), "req-timing-fast-fail")
	logSlowCodexUpstreamTiming(ctx, codexUpstreamTiming{
		endpoint:        "responses_stream",
		model:           "gpt-5.5",
		authID:          "auth-fast-fail-1",
		proxySource:     "proxy-pool",
		proxyPool:       "free-egress",
		proxyName:       "free-proxy-3",
		status:          200,
		startedAt:       time.Now().Add(-6 * time.Second),
		httpDo:          5 * time.Second,
		readBody:        400 * time.Millisecond,
		traceFirstByte:  2 * time.Second,
		streamErrText:   "stream error: stream disconnected before completion: stream closed before response.completed",
		streamCompleted: false,
	})

	out := buf.String()
	for _, part := range []string{
		"codex upstream timing",
		"request_id=req-timing-fast-fail",
		"auth_id=auth-fast-fail-1",
		"stream_completed=false",
		"stream_err=stream error: stream disconnected before completion: stream closed before response.completed",
	} {
		if !strings.Contains(out, part) {
			t.Fatalf("log output missing %q: %s", part, out)
		}
	}
}

func TestRecordCodexProxyPassiveOutcomeMarksRepeatedSlowProxyUnusable(t *testing.T) {
	manager := proxypool.NewHealthManager()
	now := time.Now()
	timing := codexUpstreamTiming{
		proxyPool: "free-egress",
		proxyName: "free-proxy-11",
		status:    200,
		bytesRead: 64 * 1024,
		startedAt: now.Add(-2 * time.Minute),
		readBody:  100 * time.Second,
	}

	recordCodexProxyPassiveOutcome(timing, manager)
	if !manager.IsUsableAt("free-egress", "free-proxy-11", now) {
		t.Fatal("expected first slow passive outcome to keep proxy usable")
	}

	recordCodexProxyPassiveOutcome(timing, manager)
	if manager.IsUsableAt("free-egress", "free-proxy-11", now) {
		t.Fatal("expected repeated slow passive outcomes to mark proxy unusable")
	}
}

func TestRecordCodexProxyPassiveOutcomeMarksRepeatedSlowFirstByteUnusable(t *testing.T) {
	manager := proxypool.NewHealthManager()
	now := time.Now()
	timing := codexUpstreamTiming{
		endpoint:       "responses_stream",
		proxyPool:      "free-egress",
		proxyName:      "free-proxy-12",
		status:         200,
		bytesRead:      512 * 1024,
		startedAt:      now.Add(-20 * time.Second),
		readBody:       12 * time.Second,
		traceFirstByte: proxypool.DefaultPassiveSlowFirstByte + time.Second,
	}

	recordCodexProxyPassiveOutcome(timing, manager)
	if !manager.IsUsableAt("free-egress", "free-proxy-12", now) {
		t.Fatal("expected first slow first-byte passive outcome to keep proxy usable")
	}

	recordCodexProxyPassiveOutcome(timing, manager)
	if manager.IsUsableAt("free-egress", "free-proxy-12", now) {
		t.Fatal("expected repeated slow first-byte passive outcomes to mark proxy unusable")
	}
}

func TestRecordCodexProxyPassiveOutcomeIgnoresCompactFirstByte(t *testing.T) {
	manager := proxypool.NewHealthManager()
	now := time.Now()
	timing := codexUpstreamTiming{
		endpoint:       "responses/compact",
		proxyPool:      "free-egress",
		proxyName:      "free-proxy-10",
		status:         200,
		bytesRead:      512 * 1024,
		startedAt:      now.Add(-65 * time.Second),
		readBody:       100 * time.Millisecond,
		traceFirstByte: time.Minute,
	}

	recordCodexProxyPassiveOutcome(timing, manager)
	recordCodexProxyPassiveOutcome(timing, manager)
	if !manager.IsUsableAt("free-egress", "free-proxy-10", now) {
		t.Fatal("expected compact first-byte latency to stay out of passive proxy health")
	}
}

func TestRecordCodexProxyPassiveOutcomeAggressivelyPenalizesLongGPT54Cancel(t *testing.T) {
	manager := proxypool.NewHealthManager()
	now := time.Now()
	timing := codexUpstreamTiming{
		endpoint:      "responses_stream",
		model:         "gpt-5.4",
		proxyPool:     "free-egress",
		proxyName:     "free-proxy-14",
		status:        200,
		bytesRead:     1700000,
		startedAt:     now.Add(-5 * time.Minute),
		readBody:      4*time.Minute + 50*time.Second,
		traceFirstByte: 900 * time.Millisecond,
		streamErrText: "context canceled",
	}

	recordCodexProxyPassiveOutcome(timing, manager)
	if manager.IsUsableAt("free-egress", "free-proxy-14", now) {
		t.Fatal("expected long-lived gpt-5.4 cancellation to mark proxy unusable immediately")
	}
}

func TestRecordCodexProxyPassiveOutcomeAggressivelyPenalizesGPT54InternalError(t *testing.T) {
	manager := proxypool.NewHealthManager()
	now := time.Now()
	timing := codexUpstreamTiming{
		endpoint:       "responses_stream",
		model:          "gpt-5.4",
		proxyPool:      "free-egress",
		proxyName:      "free-proxy-13",
		status:         200,
		bytesRead:      1800000,
		startedAt:      now.Add(-4 * time.Minute),
		readBody:       3*time.Minute + 50*time.Second,
		traceFirstByte: 800 * time.Millisecond,
		streamErrText:  "stream error: stream ID 2889; INTERNAL_ERROR; received from peer",
	}

	recordCodexProxyPassiveOutcome(timing, manager)
	if manager.IsUsableAt("free-egress", "free-proxy-13", now) {
		t.Fatal("expected gpt-5.4 INTERNAL_ERROR to mark proxy unusable immediately")
	}
}

func TestRecordCodexProxyPassiveOutcomeKeepsVeryLongGPT54SuccessUsable(t *testing.T) {
	manager := proxypool.NewHealthManager()
	now := time.Now()
	timing := codexUpstreamTiming{
		endpoint:       "responses_stream",
		model:          "gpt-5.4",
		proxyPool:      "free-egress",
		proxyName:      "free-proxy-8",
		status:         200,
		bytesRead:      700000,
		startedAt:      now.Add(-4*time.Minute - 10*time.Second),
		readBody:       4 * time.Minute,
		traceFirstByte: 700 * time.Millisecond,
		streamCompleted: true,
	}

	recordCodexProxyPassiveOutcome(timing, manager)
	if !manager.IsUsableAt("free-egress", "free-proxy-8", now) {
		t.Fatal("expected very long completed gpt-5.4 stream to stay usable")
	}
}

func TestRecordCodexProxyPassiveOutcomeAggressivelyPenalizesVeryLongIncompleteGPT54Stream(t *testing.T) {
	manager := proxypool.NewHealthManager()
	now := time.Now()
	timing := codexUpstreamTiming{
		endpoint:       "responses_stream",
		model:          "gpt-5.4",
		proxyPool:      "free-egress",
		proxyName:      "free-proxy-8",
		status:         200,
		bytesRead:      700000,
		startedAt:      now.Add(-4*time.Minute - 10*time.Second),
		readBody:       4 * time.Minute,
		traceFirstByte: 700 * time.Millisecond,
		streamErrText:  `{"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`,
	}

	recordCodexProxyPassiveOutcome(timing, manager)
	if manager.IsUsableAt("free-egress", "free-proxy-8", now) {
		t.Fatal("expected very long incomplete gpt-5.4 stream to mark proxy unusable immediately")
	}
}
