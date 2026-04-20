# internal/logging/

Structured NDJSON file logging with size-based rotation. One log file
per logger; up to three backups (`.1` / `.2` / `.3`).

## Layout

- `logger.go` — `Logger` type, `NewLogger`, `Log`, `Close`, and the
  internal `rotate` flow (delete `.3`, shift `.2→.3`, `.1→.2`,
  `current→.1`, create new current).
- `provider_events.go` — `ProviderEventEntry` shape and the
  `LogProviderEvent` helper for raw provider stdin/stdout bytes.

## Responsibility boundary

- What BELONGS here:
  - Appending one JSON object per line.
  - Size-based rotation with a bounded number of backups.
  - Concurrency-safe writes from multiple goroutines.
- What does NOT belong here:
  - Structured telemetry (spans, metrics) — see
    `internal/observability/otel`.
  - Replay of triage events — see `internal/observability/replay`.
  - User-facing error surfacing. Logs are for maintainers; users see
    state via `app.Event.Emit`.

## Extension points

- To add a new log category: declare a new typed struct (next to
  `ProviderEventEntry`), call `logValue` from a helper. Keep the
  public `Log`/`LogProviderEvent` surface intact.
- To change rotation policy: adjust `defaultMaxBytes` or the backup
  count. Keep rotation size-based — the observability/replay package
  owns the thread-partitioned flavor.

## Anti-patterns

- Do NOT swallow rotation errors. `rotate` always returns a typed
  error; callers (and the regression test) depend on it.
- Do NOT use the `Logger` from hot paths that can't afford a mutex-
  guarded file write. For the triage hot path, use
  `observability/replay` instead.
- Do NOT share a `Logger` across processes. The file lock is
  Go-level; concurrent processes rotate out from under each other.

## References

- `internal/observability/replay` — higher-throughput per-thread
  flavor.
