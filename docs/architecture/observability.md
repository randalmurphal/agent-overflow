# Observability

Two opt-in streams: OpenTelemetry traces/metrics (`internal/observability/otel`)
and a per-thread NDJSON event log (`internal/observability/replay`). Both
default off — disabled construction costs no goroutines and no file
descriptors.

## Settings

Three fields on `settings.Settings` control the stack, all wired in
`app.go` at `ServiceStartup`:

| Field | Effect |
|---|---|
| `ObservabilityTracingEnabled` | Flips the OTel `Provider` from no-op to real OTLP exporters. |
| `ObservabilityOtlpEndpoint` | gRPC `host:port`. Empty string falls back to the OTel env default (`localhost:4317`). |
| `ObservabilityEventLogEnabled` | Toggles the replay `Manager` at runtime via `SetEnabled`. |

Tracing changes require an app restart — we do not hot-swap
`TracerProvider`. The replay toggle is hot-swapped by
`app.ReconfigureObservability` (`app_observability.go:33`). Neither field
is treated as a secret — the "SECURITY NOTE" block at
`internal/settings/settings.go:41` spells out why and warns against
adding real secrets to this file.

## OTel (traces + metrics)

`otel.NewProvider` builds one `TracerProvider` + one `MeterProvider`,
both OTLP-gRPC exporters using `WithInsecure()` against the configured
endpoint. The `Provider` exposes a pre-built `Metrics` struct so callers
avoid otel's global lookups (`internal/observability/otel/provider.go:63`).

- **Spans** — triage opens a `turn.lifecycle` span on `EventTurnStart` and
  closes it on `EventTurnComplete`, tagged with `thread.id`, `provider`,
  `model`, `turn.index` (`internal/triage/router.go:302`).
- **Counters** — `turns.started`, `turns.completed`, `turns.errored`,
  `items.persisted`, `payloads.persisted` (recorded by the triage
  router), plus `replay.events.queued` / `replay.events.dropped`
  (recorded by the event-log hook in `app.go:419`).
- **Histogram** — `provider.stream.frames` for byte sizes of individual
  provider stream frames.
- Metric export uses a `PeriodicReader` at 15s (provider.go:186) — fine
  for a desktop app, not tuned for high-volume tracing backends.

## Replay (per-thread NDJSON)

`replay.Manager` owns one `Writer` per threadID, writing to
`<dbDir>/replay/<threadID>.jsonl`. Every event flowing through
`a.emitWithReplay()` (`app.go:404`) is mirrored into the log when
enabled — including frontend-visible events like `provider:event`,
`provider:meta`, design and checkpoint events. Records are
`{ts, threadId, kind, data}` with `data` as opaque JSON.

- **Bounded queue** — `defaultQueueSize = 4096`. A full queue drops the
  event and bumps `replay.events.dropped`. Never blocks triage.
- **Rotation** — `defaultMaxBytes = 100 MB` per thread file; keeps three
  backups `.1 / .2 / .3` (`writer.go:16`).
- **Idle reaper** — `defaultIdleTimeout = 5 min`, scanned every 30s; an
  idle writer is closed so the manager doesn't hold dormant file
  descriptors (`manager.go:22`, `manager.go:371`).
- **Privacy** — replay captures user prompts, assistant output, tool
  inputs, command output. Opt-in only; nothing is redacted. Disabling
  the setting tears down the goroutines, closes every writer, and
  clears the queue; existing `.jsonl` files are left on disk.

## Related

- Raw provider stdio logging (`internal/logging`) is a separate,
  dev-only path gated by `AGENT_OVERFLOW_DEBUG=provider`
  (`app_logging.go:13`). 10 MB rotation, not wired into settings.
- Replay files are deleted alongside their thread via
  `replay.Manager.RemoveThreadLog` (`manager.go:414`).
