# ADR-007: `SetEventHook` Test Seam

Status: accepted
Date: 2026-04-18

## Context

Go tests need to synchronize on "the router finished handling event E."
Without a synchronization point, tests either:

- **Poll SQLite in a loop** until the expected row appears. Flaky,
  slow, and obscures test intent.
- **Use `time.Sleep`.** Forbidden by our test discipline
  (see [`conventions.md`](../conventions.md)).
- **Subscribe to `provider:item_event`.** Works for timeline mutation
  events but doesn't fire for inline passthroughs (text deltas,
  approvals routed only to the approval channel, etc.).

Earlier iterations added a generic `provider:event` channel that every
handler emitted on after processing, purely so tests could listen.
This polluted the production event stream with a channel no
subscriber actually used.

## Decision

Expose a test-only observer on the router via `SetEventHook`:

```go
func (r *Router) SetEventHook(hook func(provider.ProviderEvent)) {
    r.mu.Lock()
    r.eventHook = hook
    r.mu.Unlock()
}
```

Production code leaves the hook nil. Tests install a hook that pushes
to a channel or asserts on the event. The hook fires after every
`Handle` call.

## Rationale

- **No production channel pollution.** The frontend never sees the
  hook; no wire-level passthrough exists for it.
- **Single synchronization primitive.** Any test can grab one.
- **Low cost.** A nil-check on every `Handle` call is free.

Considered alternatives:

- **Generic `provider:event` channel.** Rejected: every handler
  would have to emit, production code paying for a channel only
  tests read.
- **Persist-only synchronization (poll SQLite).** Rejected: doesn't
  cover inline passthroughs; forces tests to reach into storage.
- **Inject a mock store that fires on write.** Rejected: tests would
  have to inject a mock even when they don't care about storage;
  the hook abstracts over both persisted and non-persisted paths.

## Consequences

- The `emitInline` method on the router is deliberately a no-op
  marker: the router documents that it does NOT emit a
  wire-level passthrough for typed-channel events, and tests use
  the hook instead. The header comment on `emitInline` calls this
  out.
- Any new handler must respect the hook contract. `Handle` fires
  it after the case statement returns. Adding a handler that
  short-circuits before the hook fires breaks tests relying on
  it.
- The hook is protected by the router's mutex so `SetEventHook`
  can be called from any goroutine; production stays lock-free on
  the read path (the nil-check is racy, but a stale nil is fine,
  since the hook is test-only).
