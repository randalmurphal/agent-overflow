# Observability

Two opt-in streams: OpenTelemetry traces/metrics (`internal/observability/otel`)
and a per-thread NDJSON event log (`internal/observability/replay`). Both
default off. Disabled construction costs no goroutines and no file
descriptors.

## Settings

Three fields on `settings.Settings` control the stack, all wired in
`app.go` at `ServiceStartup`:

| Field | Effect |
|---|---|
| `ObservabilityTracingEnabled` | Flips the OTel `Provider` from no-op to real OTLP exporters. |
| `ObservabilityOtlpEndpoint` | gRPC `host:port`. Empty string falls back to the OTel env default (`localhost:4317`). |
| `ObservabilityEventLogEnabled` | Toggles the replay `Manager` at runtime via `SetEnabled`. |

Tracing changes require an app restart. We do not hot-swap
`TracerProvider`. The replay toggle is hot-swapped by
`App.ReconfigureObservability` (in `app_observability.go`). Neither
field is treated as a secret. The "SECURITY NOTE" block at the top of
`internal/settings/settings.go` spells out why and warns against
adding real secrets to this file.

## OTel (traces + metrics)

`otel.NewProvider` builds one `TracerProvider` + one `MeterProvider`,
both OTLP-gRPC exporters using `WithInsecure()` against the configured
endpoint. The `Provider` exposes a pre-built `Metrics` struct so callers
avoid otel's global lookups (see `otel.NewProvider` in
`internal/observability/otel/provider.go`).

- **Spans**: triage opens a `turn.lifecycle` span on `EventTurnStart` and
  closes it on `EventTurnComplete`, tagged with `thread.id`, `provider`,
  `model`, `turn.index` (see `Router.openTurnSpan` in
  `internal/triage/turn_telemetry.go`).
- **Counters**: `turns.started`, `turns.completed`, `turns.errored`,
  `items.persisted`, `payloads.persisted` (recorded by the triage
  router), plus `replay.events.queued` / `replay.events.dropped`
  (recorded by the event-log hook wired from `app.go` via
  `App.emitWithReplay`). A terminal turn span increments exactly one of
  `turns.completed` or `turns.errored`; provider errors, interruptions,
  truncation, persistence failures, and cleanup all take the errored path.
  A re-sent turn start replaces the live span without claiming a terminal
  outcome because it is a lifecycle transition for the same turn.
- **Histogram**: `provider.stream.frames` for byte sizes of individual
  provider stream frames.
- Metric export uses a `PeriodicReader` at 15s (see
  `otel.NewProvider`). Fine for a desktop app, not tuned for
  high-volume tracing backends.

## Replay (per-thread NDJSON)

`replay.Manager` owns one `Writer` per threadID, writing to
`<dbDir>/replay/<threadID>.jsonl`. Every event flowing through
`App.emitWithReplay` (in `app_emit.go`) is mirrored into the log when
enabled, including the typed routing channels (`provider:item_event`,
`provider:approval`, `provider:usage`, `provider:status`) and the
design and message-revert event families. Records are
`{ts, threadId, kind, data}` with `data` as opaque JSON.

- **Bounded queue**: `defaultQueueSize = 4096`. A full queue drops the
  event and bumps `replay.events.dropped`. Never blocks triage.
- **Rotation**: `defaultMaxBytes = 100 MB` per thread file; keeps three
  backups `.1 / .2 / .3` (see `Writer.rotate` in
  `internal/observability/replay/writer.go`).
- **Idle reaper**: `defaultIdleTimeout = 5 min`, scanned every 30s; an
  idle writer is closed so the manager doesn't hold dormant file
  descriptors (see `Manager.reapIdleWriters` in
  `internal/observability/replay/manager.go`).
- **Privacy**: replay captures user prompts, assistant output, tool
  inputs, command output. Opt-in only; nothing is redacted. Disabling
  the setting tears down the goroutines, closes every writer, and
  clears the queue; existing `.jsonl` files are left on disk.

## Related

- Raw provider stdio logging (`internal/logging`) is a separate,
  dev-only path gated by `AGENT_OVERFLOW_DEBUG=provider` (wired in
  `app_logging.go`). 10 MB rotation, not wired into settings.
- Replay files are deleted alongside their thread via
  `replay.Manager.RemoveThreadLog`.
