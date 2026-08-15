# internal/observability/

Opt-in OpenTelemetry tracing/metrics and an opt-in per-thread NDJSON
replay writer. Both are off by default: construction of the zero-config
providers returns no-op implementations so instrumented call sites pay
at most one interface dispatch.

One subpackage is deliberately NOT opt-in — `goroutinedump`, which arms a
signal handler at boot. Opt-in is the right default for anything that
costs per call site; it is the wrong default for the one tool a wedged
process needs, because the wedge is discovered after the process started.

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
- `pprofserve/` — opt-in loopback-only `net/http/pprof` listener
  (`AGENT_OVERFLOW_PPROF=1` → `127.0.0.1:6363`, or an explicit
  loopback host:port). Deliberately a separate listener so profiling
  never rides the authenticated transport wire. The env var crosses
  the WSL boundary via WSLENV passthrough at both hops (dev
  supervisor `childEnv`, `wsllauncher.LaunchOptions.PassthroughEnv`).
- `goroutinedump/` — always-armed SIGUSR1 handler that writes a full
  `pprof` debug=2 goroutine dump (`goroutines-<ts>.txt`, 0600 under a
  0700 dir) into the logging directory. Stdlib-only; `install_windows.go`
  is a no-op stub. It is the one thing here that is NOT opt-in, and
  deliberately so: `pprofserve` needs an env var set before the process
  started, which is never true of the process that is wedged NOW
  (incident 2026-08-15, a send stuck under a per-thread lock in a
  stripped binary). The cost is one parked goroutine.
  - Dumps are throttled to one per `MinInterval` (10s) and the
    suppression is LOGGED, because anyone able to signal the process can
    ask for one and an unthrottled loop both fills the disk and starves
    the process it is diagnosing. A human's repeat rate is nowhere near
    the bound, so an operator always gets the second dump they asked for.
  - Retention is `internal/logging`'s: `PruneOlderThan` sweeps
    `FilePrefix` files out of the same directory it prunes logs from.
    That prefix is the seam between the two packages — logging imports
    it rather than restating the name.
- `integration_test.go` — end-to-end test against the otel and replay subpackages.

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
