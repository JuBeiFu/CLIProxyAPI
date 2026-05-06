# Codex Auth Lab

`internal/runtime/executor/authlab` provides a repeatable Codex routing drill against the auth-aware upstream mock.

It currently covers two operator questions:

- `round-robin` vs `fill-first` auth skew under the same request mix
- standby hedge behavior when the primary profile is slow before first byte

## What it measures

- `hottest_auth_id`
- `hottest_auth_share`
- `load_summary.avg_first_byte`
- `load_summary.p95_first_byte`
- `route_summary.hedge_triggered`
- `route_summary.primary_wins`
- `route_summary.standby_wins`
- `mock_stats.auths`
- `mock_stats.profiles`

## How to use it

Run the package tests for the built-in scenarios:

```bash
go test ./internal/runtime/executor/authlab -count=1
```

The current lab is a direct mock routing drill. It is useful for comparing routing policy and hedge behavior without depending on the full embedded `CLIProxyAPI` startup path.

## Read the output

- Lower `hottest_auth_share` means auth distribution is healthier.
- Higher `standby_wins` means the hedge path is actually rescuing slow primary profiles.
- If `hedge_triggered` is high but `standby_wins` stays low, your hedge threshold is probably too aggressive or both routes are unhealthy.
