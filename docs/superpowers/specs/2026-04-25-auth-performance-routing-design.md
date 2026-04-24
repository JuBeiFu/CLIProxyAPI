# Auth Performance Routing Design

## Goal

Add per-auth, per-model output token throughput tracking to CLIProxyAPI and use that data as an optional routing signal for choosing faster credentials within the existing scheduler.

## Background

CLIProxyAPI already emits usage records with provider, model, auth ID, auth index, latency, request status, and token counts. The management usage snapshot aggregates request details by API key and model, and the scheduler already maintains provider/model shards, static priority buckets, cooldown state, disabled state, websocket preference, and inflight counts.

The missing piece is a runtime performance view keyed by credential and model. The new feature should turn existing request completion data into a stable speed signal, expose it through management APIs, and let the scheduler use it only when explicitly enabled.

## Metric Definition

The primary speed metric is output tokens per second:

```text
output_tps = output_tokens / duration_seconds
```

Duration selection:

- For non-streaming responses, use total request latency from `UsageReporter`.
- For streaming responses with first-byte timing available, use `total_latency - first_byte_latency` as generation duration.
- For streaming responses without first-byte timing, use total request latency.
- For missing or zero output tokens, record the sample for request count and latency, while contributing no speed sample.

The scoring path should use EWMA values and a minimum sample threshold so short bursts or single outliers do not dominate routing decisions.

## Architecture

The feature has three layers:

1. Collection: convert usage records and optional timing fields into performance samples.
2. Aggregation: maintain a thread-safe in-memory tracker keyed by `(provider, auth_id, model)`.
3. Routing: optionally score ready credentials in the scheduler using the tracker snapshot.

The tracker is intentionally separate from the existing usage snapshot. Usage statistics remain the audit/detail store. The performance tracker is the real-time routing signal with bounded memory and decay semantics.

## Components

### `internal/performance`

Create a new package for runtime auth performance state.

Responsibilities:

- Accept samples from completed requests.
- Keep per-key EWMA for output TPS, total latency, generation latency, first-byte latency, and failure rate.
- Track success count, failure count, request count, token count, last seen time, and recent-window counts.
- Provide immutable snapshots for management endpoints and scheduler scoring.
- Apply default configuration and clamp invalid config values.

Key structs:

```go
type Key struct {
    Provider string
    AuthID   string
    Model    string
}

type Sample struct {
    Provider          string
    AuthID            string
    AuthIndex         string
    Model             string
    RequestedAt       time.Time
    Latency           time.Duration
    FirstByteLatency  time.Duration
    OutputTokens      int64
    TotalTokens       int64
    Failed            bool
    StatusCode        int
}

type Snapshot struct {
    Provider              string    `json:"provider"`
    AuthID                string    `json:"auth_id"`
    AuthIndex             string    `json:"auth_index,omitempty"`
    Model                 string    `json:"model"`
    RequestCount          int64     `json:"request_count"`
    SuccessCount          int64     `json:"success_count"`
    FailureCount          int64     `json:"failure_count"`
    OutputTokens          int64     `json:"output_tokens"`
    OutputTPSEWMA         float64   `json:"output_tps_ewma"`
    LatencyMsEWMA         float64   `json:"latency_ms_ewma"`
    FirstByteMsEWMA       float64   `json:"first_byte_ms_ewma,omitempty"`
    FailureRateEWMA       float64   `json:"failure_rate_ewma"`
    WindowRequestCount    int64     `json:"window_request_count"`
    WindowOutputTokens    int64     `json:"window_output_tokens"`
    WindowOutputTPS       float64   `json:"window_output_tps"`
    SampleReady           bool      `json:"sample_ready"`
    LastSeen              time.Time `json:"last_seen"`
}
```

### Usage Collection

Extend `sdk/cliproxy/usage.Record` with optional timing fields:

```go
FirstByteLatency time.Duration
```

`UsageReporter` should keep its existing once-only publication semantics. When token usage is available, it builds a performance sample from the record and publishes it to the tracker. When only latency is available, it still records request and failure data.

Existing executor timing logs can continue to exist. The first implementation can route all providers using total latency. Codex HTTP/WebSocket first-byte timing can be wired into `UsageReporter` in a later task when the timing field is available at the response path.

### Management API

Add:

```text
GET /v0/management/auth-performance
```

Query parameters:

- `provider`: optional provider filter.
- `model`: optional model filter.
- `auth_id`: optional auth filter.
- `ready_only`: optional boolean to return sample-ready entries only.

Response:

```json
{
  "performance": [
    {
      "provider": "codex",
      "auth_id": "auth-1",
      "auth_index": "3",
      "model": "gpt-5.4",
      "request_count": 15,
      "success_count": 14,
      "failure_count": 1,
      "output_tokens": 9120,
      "output_tps_ewma": 38.7,
      "latency_ms_ewma": 4210,
      "failure_rate_ewma": 0.03,
      "window_request_count": 12,
      "window_output_tokens": 7400,
      "window_output_tps": 41.2,
      "sample_ready": true,
      "last_seen": "2026-04-25T12:34:56Z"
    }
  ],
  "config": {
    "enabled": false,
    "shadow_log": true,
    "window_seconds": 300,
    "min_samples": 5
  }
}
```

This endpoint must be protected by the existing management middleware and must never expose tokens, refresh tokens, API keys, or raw request/response payloads.

### Configuration

Extend `config.RoutingConfig` with performance-aware settings:

```yaml
routing:
  performance-aware: false
  performance-shadow-log: true
  performance-window-seconds: 300
  performance-min-samples: 5
  performance-ewma-alpha: 0.25
  performance-weight-tps: 1.0
  performance-weight-latency: 0.25
  performance-weight-failure: 2.0
  performance-weight-inflight: 0.5
```

Defaults:

- `performance-aware`: `false`
- `performance-shadow-log`: `true`
- `performance-window-seconds`: `300`
- `performance-min-samples`: `5`
- `performance-ewma-alpha`: `0.25`
- `performance-weight-tps`: `1.0`
- `performance-weight-latency`: `0.25`
- `performance-weight-failure`: `2.0`
- `performance-weight-inflight`: `0.5`

Config should hot-reload through the existing config watcher path. Invalid numeric values should fall back to defaults.

## Routing Behavior

Performance routing must preserve existing scheduler semantics:

- Disabled, blocked, cooldown, quota-exceeded, and unsupported-model auths remain ineligible.
- Websocket-compatible auth preference for websocket downstream requests stays active.
- Static auth `priority` remains the top-level bucket selector.
- Performance score only changes ordering inside the current provider/model/priority bucket.
- Existing response binding and pinned auth behavior stay authoritative.
- Existing retry and tried-auth exclusion behavior remains authoritative.

Score formula:

```text
score =
  normalized_output_tps * performance_weight_tps
  - normalized_latency * performance_weight_latency
  - failure_rate_ewma * performance_weight_failure
  - inflight * performance_weight_inflight
```

Normalization is computed within the current candidate set for a single provider/model/priority bucket. This keeps scores comparable only among credentials that are already equivalent under static routing constraints.

Cold-start behavior:

- When `sample_count < performance-min-samples`, the entry is sample-cold.
- Sample-cold entries use the median score of ready candidates when available.
- If all candidates are sample-cold, scheduler falls back to existing round-robin or fill-first behavior.
- Sample-cold entries remain eligible, so new credentials can collect data.

Shadow mode:

- When `performance-shadow-log` is true, the scheduler computes the performance-preferred auth and logs a structured debug line comparing it with the selected auth.
- Shadow logging works even when `performance-aware` is false.
- The log includes provider, model, selected auth ID, shadow auth ID, score values, candidate count, and sample-ready count.

Enabled mode:

- When `performance-aware` is true, the scheduler selects the highest score inside the selected priority bucket.
- Ties use the existing strategy ordering: round-robin cursor for round-robin selectors and deterministic order for fill-first selectors.

## Data Flow

```text
executor response
  -> UsageReporter.Publish / PublishFailure
  -> sdk/cliproxy/usage.PublishRecord
  -> internal/usage.LoggerPlugin for existing usage statistics
  -> internal/performance.AuthPerformanceTracker.Record
  -> management snapshot endpoint
  -> scheduler score provider
  -> ready bucket ordering
```

## Error Handling

- Missing provider, auth ID, or model causes the performance tracker to ignore the sample.
- Negative durations are clamped to zero.
- Zero latency samples update request counters and failure rate, while speed EWMA remains unchanged.
- Failed requests update failure EWMA and counters. They do not add output TPS even if token fields are present.
- Tracker operations must be panic-free and safe under concurrent request completion.
- Management snapshot returns an empty list when statistics are unavailable.

## Testing Strategy

Unit tests:

- Tracker records successful samples and computes output TPS EWMA.
- Tracker records failures and updates failure rate without speed inflation.
- Tracker enforces minimum sample readiness.
- Tracker expires window events outside `performance-window-seconds`.
- Tracker snapshots filter by provider, model, auth ID, and ready-only.
- Score normalization prefers higher output TPS within the same candidate set.
- Score penalizes high latency, failures, and inflight counts.
- Cold candidates use fallback score and remain eligible.

Scheduler tests:

- With `performance-aware=false`, existing round-robin/fill-first tests keep passing.
- With shadow mode, selected auth follows existing strategy and shadow choice is computed.
- With `performance-aware=true`, same-priority candidates prefer the higher score.
- Higher static priority beats lower static priority even when lower priority has better TPS.
- Websocket preference remains active for websocket downstream requests.
- Tried-auth exclusion and cooldown state remain active.

Management tests:

- `GET /v0/management/auth-performance` returns snapshots and config.
- Filters work independently and together.
- The response contains no secret material.

Integration tests:

- Simulated two-auth provider where one auth consistently returns more output tokens per second.
- After enough samples, enabled routing chooses the faster auth more often.
- Disabling performance-aware routing restores existing selection behavior.

## Rollout Plan

Phase 1: Observability

- Add tracker, sample collection, config, and management endpoint.
- Keep `performance-aware=false`.
- Enable `performance-shadow-log=true` by default.

Phase 2: Shadow Validation

- Compare selected auth vs performance-preferred auth in logs.
- Watch failure rate, latency, TPS stability, and candidate churn.
- Validate per-model separation, especially Codex HTTP vs websocket behavior.

Phase 3: Controlled Enablement

- Enable `performance-aware=true` for test or canary service.
- Keep minimum samples at 5 or higher.
- Monitor 502/429 rate, output TPS, first-byte latency, and inflight distribution.

Phase 4: Default Policy Decision

- Keep the feature opt-in by config for production.
- Consider enabling by default only after enough production observations show stable improvement.

## Security And Privacy

The feature stores only aggregate performance metrics. It must not store request bodies, response bodies, access tokens, refresh tokens, API keys, cookies, or prompts. Auth IDs and auth indexes already appear in management contexts; this endpoint should follow the same access controls.

## Acceptance Criteria

- Operators can view per-auth, per-model output TPS and latency through management API.
- Scheduler can compute performance scores without changing routing while `performance-aware=false`.
- When enabled, scheduler prefers faster same-priority credentials for the same model.
- Existing priority, cooldown, disabled, websocket, pinned-auth, and retry semantics remain intact.
- Tests cover tracker, management endpoint, and scheduler behavior.
- `go test ./internal/performance ./internal/api/handlers/management ./sdk/cliproxy/auth -count=1` passes.
- `go build -o test-output ./cmd/server` succeeds.
