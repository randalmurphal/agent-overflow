# internal/logging/

Structured NDJSON file logging with size-based rotation. One log file
per logger; up to three backups (`.1` / `.2` / `.3`).

## Layout

- `logger.go`: `Logger` type, `NewLogger`, `Log`, `Close`, and the
  internal `rotate` flow (delete `.3`, shift `.2→.3`, `.1→.2`,
  `current→.1`, create new current).
- `provider_events.go`: `ProviderEventEntry` shape, the
  `LogProviderEvent` helper for raw provider stdin/stdout bytes, and the
  `AGENT_OVERFLOW_DEBUG` gate that decides whether that logger exists
  at all.
- `engine_events.go` holds the `EngineEventEntry` shape and `LogEngineEvent`,
  the workflow run-lifecycle stream (`engine-YYYY-MM-DD.ndjson`): one
  line per engine-significant decision (a park and the engine's own
  diagnosis of it, a cancel, a resume, a definition re-read, a rebuild
  action, a fan-out wider than the capacity it will contend on). Unlike
  the provider log it takes **no env gate**: a run parks once, and
  there is no second chance to have turned the log on beforehand.
- `prune.go`: `Dir` (the one spelling of `<baseDir>/logs`), `logKinds`
  (every daily-file prefix this package mints), `dailyLogPath`, and
  `PruneOlderThan`. A kind is minted and pruned through the same list, so
  a new stream cannot ship with retention that silently ignores it.
  - **The directory holds TWO streams and the sweep covers both.** The
    second is not this package's: `internal/observability/goroutinedump`
    writes its SIGUSR1 dumps here (it asks `Dir` where "here" is), one
    file per signal, named by the moment it was taken. They have no kind
    stub, no rotation suffix, and no open handle to protect, so they are
    matched by `goroutinedump.FilePrefix` (imported, never re-spelled)
    and judged on mtime alone with no active-file guard. Retention has to
    reach them for the reason it reaches anything: a dump is a full stack
    listing of a wedged process, taken exactly when that listing is
    largest, and the one directory this package prunes must not be the one
    place that accumulates forever. The import direction is one-way by
    design (`goroutinedump` is stdlib-only and knows nothing about this
    package), which is what keeps the prefix from drifting.
  - Anything ELSE that lands in this directory must join the sweep in the
    same change, as a `logKinds` entry if it is a daily log, or as its
    own matcher if it is not. A file shape the sweep does not recognise is
    skipped silently, which is retention that lies.

## Responsibility boundary

- What BELONGS here:
  - Appending one JSON object per line.
  - Size-based rotation with a bounded number of backups.
  - Concurrency-safe writes from multiple goroutines.
- What does NOT belong here:
  - Structured telemetry (spans, metrics). See
    `internal/observability/otel`.
  - Replay of triage events. See `internal/observability/replay`.
  - User-facing error surfacing. Logs are for maintainers; users see
    state via `app.Event.Emit`.

## Extension points

- To add a new log category: declare a new typed struct (next to
  `ProviderEventEntry` / `EngineEventEntry`), call `logValue` from a
  helper, mint its path through `dailyLogPath`, and add its prefix to
  `logKinds` so retention covers it. Keep the public
  `Log`/`LogProviderEvent`/`LogEngineEvent` surface intact.
- To change rotation policy: adjust `defaultMaxBytes` or the backup
  count. Keep rotation size-based. The observability/replay package
  owns the thread-partitioned flavor.

## Anti-patterns

- Keep `ProviderEventEntry.Data` a `json.RawMessage`, never a quoted
  string. Re-escaping every provider frame was ~24% of backend allocation
  during streaming turns (measured 2026-08-24). The encoder compacts the
  raw value, so NDJSON framing stays safe, and a non-JSON payload still
  falls back to the quoted form.
- Do NOT swallow rotation errors. `rotate` always returns a typed
  error; callers (and the regression test) depend on it.
- Do NOT use the `Logger` from hot paths that can't afford a mutex-
  guarded file write. For the triage hot path, use
  `observability/replay` instead.
- Do NOT share a `Logger` across processes. The file lock is
  Go-level; concurrent processes rotate out from under each other.
- Do NOT put model-authored text in an `EngineEventEntry.Message`. The
  engine writes its own prose there; the durable, user-facing copy of a
  park's cause is the attempt row's `park_cause`, and this stream is the
  diagnostic trail around it, including for the parks that never
  reached an attempt row at all.

## References

- `internal/observability/replay`: higher-throughput per-thread
  flavor.
