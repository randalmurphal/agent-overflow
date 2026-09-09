# `internal/observability`

Optional OpenTelemetry, bounded replay capture, loopback profiling, and operator-triggered goroutine dumps. See [observability](../../docs/architecture/observability.md).

- Inject tracers, meters, and instruments through `otel.Provider`; do not use OpenTelemetry package globals.
- Disabled telemetry must return working no-op implementations.
- Replay writes are bounded and asynchronous. A full channel drops with a visible metric rather than blocking triage.
- `pprofserve` remains loopback-only on its separate listener.
- Goroutine dumps remain available after boot without telemetry opt-in, are written with private permissions, throttled, and covered by logging retention.
- Replay records accept opaque JSON from callers. Keep admission and redaction
  decisions at the instrumented call site where the payload meaning is known.
