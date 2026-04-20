# internal/observability/

Opt-in OpenTelemetry tracing/metrics and an opt-in per-thread NDJSON
replay writer. Both are off by default: construction of the zero-config
providers returns no-op implementations so instrumented call sites pay
at most one interface dispatch.

## Layout

- `otel/` — OpenTelemetry wiring.
  - `provider.go` — `Provider` that owns tracer + meter providers
    configured from user settings. Callers receive this explicitly;
    nothing uses the otel package globals.
  - `span.go` — `StartSpan` helper.
  - `shutdown.go` — graceful flush on shutdown.
  - `config.go` — settings-driven config loader.
- `replay/` — per-thread NDJSON event log for debug replay.
  - `writer.go` — append-only writer with size-based rotation.
  - `manager.go` — cache of writers keyed by thread id; idle cleanup.
  - `record.go` — on-disk shape (`{ts, threadId, kind, data}`).
- `integration_test.go` — end-to-end test against both subpackages.

## Responsibility boundary

- What BELONGS here:
  - Span emission / metric counters / NDJSON replay records.
  - Zero-cost no-op providers when the user hasn't opted in.
  - Bounded-channel fire-and-forget writes (replay drops events and
    bumps a metric when the channel is full).
- What does NOT belong here:
  - Feature-level telemetry decisions. Call sites instrument; this
    package only provides the wiring.
  - Persisting replay events past rotation. Rotation is size-based by
    design.

## Extension points

- To add a new tracer / meter: extend `otel/provider.go` and propagate
  via `Provider`, never via the otel package globals.
- To add a new replay record kind: add a constant + writer call. The
  reader (manual / scripted) reads `kind` to dispatch.

## Anti-patterns

- Do NOT call `otel.Tracer(...)` or `otel.Meter(...)` from outside this
  package. Callers receive the `Provider` explicitly.
- Do NOT block the triage loop on replay writes. Replay is
  best-effort; dropped events are visible via a metric.
- Do NOT assume telemetry is enabled. The disabled-manager path must
  keep compiling and shipping.

## References

- `docs/architecture/observability.md` — on/off config, backends
  supported, what we instrument.
