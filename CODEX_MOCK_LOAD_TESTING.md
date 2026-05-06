# Codex Mock Load Testing

This mock suite lets you run the chain below with repeatable upstream behavior:

`new-api -> CLIProxyAPI -> mock upstream`

## Components

- `cmd/codex_mock_server`
  - Starts a standalone mock Codex upstream server.
  - Exposes:
    - `POST /responses`
    - `POST /responses/compact`
    - `GET /_mock/stats`
    - `POST /_mock/reset`
    - `GET /_mock/scenarios`
- `cmd/codex_load_runner`
  - Sends concurrent OpenAI Responses-style requests and prints a JSON summary.

## Built-in Scenarios

- `fast-complete`
- `reasoning-before-output`
- `reasoning-eof`
- `stall-before-output`
- `zero-usage-complete`
- `tool-call-complete`
- `rate-limit`
- `server-error`
- `compact-success`

Scenario selection supports two modes:

1. Static per-auth routing with a base URL path prefix.
2. Per-request routing with `metadata.cpa_mock_scenario`.

## Start the Mock

```bash
go run ./cmd/codex_mock_server -listen 127.0.0.1:18080
```

Check the scenario list:

```bash
curl http://127.0.0.1:18080/_mock/scenarios
```

Check accumulated stats:

```bash
curl http://127.0.0.1:18080/_mock/stats
```

Reset stats:

```bash
curl -X POST http://127.0.0.1:18080/_mock/reset
```

## Point CLIProxyAPI at the Mock

Use a Codex API key entry with `base-url` pointed at the mock server:

```yaml
api-keys:
  - "local-test-key"

codex-api-key:
  - api-key: "sk-mock"
    base-url: "http://127.0.0.1:18080"
```

For a fixed scenario on one auth, use a path prefix:

```yaml
codex-api-key:
  - api-key: "sk-mock"
    prefix: "reasoning"
    base-url: "http://127.0.0.1:18080/scenarios/reasoning-before-output"
```

Then start CLIProxyAPI:

```bash
go run ./cmd/server --config ./config.yaml
```

## Per-request Scenario Switching

Any OpenAI Responses request can choose a scenario without changing the auth:

```json
{
  "model": "gpt-5.4",
  "input": "hello",
  "stream": true,
  "metadata": {
    "cpa_mock_scenario": "reasoning-before-output"
  }
}
```

This works when the request goes directly to the mock, through CLIProxyAPI, or through `new-api -> CLIProxyAPI`, as long as the request body reaches the upstream unchanged.

## Run Load Against CLIProxyAPI

Example: stream requests with large reasoning before visible output.

```bash
go run ./cmd/codex_load_runner \
  -target http://127.0.0.1:8317/v1/responses \
  -api-key local-test-key \
  -model gpt-5.4 \
  -scenario reasoning-before-output \
  -requests 500 \
  -concurrency 50 \
  -stream=true \
  -timeout 2m
```

Example: force client-side cancellation during a stall.

```bash
go run ./cmd/codex_load_runner \
  -target http://127.0.0.1:8317/v1/responses \
  -api-key local-test-key \
  -scenario stall-before-output \
  -requests 100 \
  -concurrency 20 \
  -cancel-after 250ms
```

Example: compact endpoint behavior.

```bash
go run ./cmd/codex_load_runner \
  -target http://127.0.0.1:8317/v1/responses/compact \
  -api-key local-test-key \
  -scenario compact-success \
  -stream=false \
  -requests 100 \
  -concurrency 20
```

## Run Load Through new-api

If `new-api` is configured to forward Codex traffic to CLIProxyAPI, point the runner at the `new-api` endpoint instead:

```bash
go run ./cmd/codex_load_runner \
  -target http://127.0.0.1:3000/v1/responses \
  -api-key your-new-api-key \
  -scenario reasoning-before-output \
  -requests 500 \
  -concurrency 50
```

Compare the runner summary with:

- `CLIProxyAPI` request logs
- `/_mock/stats`
- `new-api` request logs

The useful first-pass splits are:

- `reasoning-before-output` vs `stall-before-output`
- `reasoning-eof`
- `zero-usage-complete`
- `rate-limit` and `server-error`
