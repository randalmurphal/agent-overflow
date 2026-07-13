# transport/

HTTP+WebSocket transport between the Svelte frontend (whether the
Wails-embedded webview or a remote browser/desktop client) and the Go
backend.

## What this package owns

- HTTP listener serving the embedded SPA, a `/bootstrap.json` manifest,
  and the `/ws` upgrade endpoint.
- A small JSON wire frame:
  `{type:"rpc"|"event"|"replay"|"subscribe"|"batch", ...}`.
- Reflection-based RPC dispatch against a receiver's exported methods.
  Method IDs are FNV-1a 32-bit of `<package>.<typeName>.<methodName>`
  so they match Wails' `internal/hash.Fnv` and the existing generated
  TypeScript bindings keep working without translation.
- Per-channel bounded ring buffer for event-push replay on reconnect.
  In-memory only — the ring is a network jitter buffer, not a history
  store (see root CLAUDE.md principle 3).
- Ephemeral token authentication (`?token=<value>`).

## What this package does NOT own

- The receiver (App). The dispatcher takes an `any` and reflects.
- TLS termination. Local binds are always plain `ws://`; real public
  exposure goes behind Tailscale Serve / SSH tunnel / reverse proxy.

## Method-level authorization

`internalmethods.go` defines two filter sets the dispatcher consults:

- `InternalServiceMethods` — Wails framework hooks plus `//wails:ignore`
  methods. Never registered. Defense-in-depth alongside the codegen
  filter.
- `LocalOnlyMethods` — privileged methods (RCE-equivalent, session
  control, settings mutation, attachment writes, FS bookkeeping,
  credential retrieval/enumeration). `Dispatcher.ResolveForOrigin`
  refuses these from non-loopback peers, returning the same
  `method_not_found` shape an unregistered method would — the
  privileged surface stays unenumerable from the LAN.

The classification list is the source of truth. Method bodies do not
re-check origin; adding a new App method that touches FS / process /
settings / credentials must add the name to `LocalOnlyMethods` (and
the `methods_gen_test.go` integrity test catches drift).

A reverse proxy on the same host makes remote peers appear loopback and
defeats `LocalOnlyMethods` locality; proxy from a different host, or do not
front privileged use with a same-host proxy.

## Additional receivers

`Dispatcher.Register` accepts more than one receiver. The only second
receiver today is the agent test harness's `Harness` type, registered
solely by the `--harness` boot path with
`RegisterOptions{LocalOnly: true}` — the whole receiver is refused for
non-loopback peers, and outside harness mode its methods don't exist
on the wire at all. Rules for any future receiver:

- Registration must be gated by the boot path that needs it; a
  receiver that exists on every boot belongs on `App` instead.
- Method names must not collide with `App` methods (name-based
  dispatch shares one namespace) — use a distinctive prefix, as
  `Harness*` does.
- Receiver-level `LocalOnly` is coarse by design. If a future receiver
  needs per-method classification, extend `internalmethods.go` rather
  than re-checking origin in method bodies.

## Wire frames

- **Client → Server**:
  - `{type:"rpc", id, methodId|method, params:[...]}` — invoke
  - `{type:"replay", lastSeqByChannel:{...}}` — request missed events
  - `{type:"subscribe", channels:[...]}` — opt into a narrow live-event set;
    ordinary SPA clients omit this and continue receiving all visible channels
- **Server → Client**:
  - `{type:"rpc", id, result|error}` — response
  - `{type:"event", channel, seq, data, gap?}` — push
  - `{type:"batch", events:[{channel, seq, data, gap?}, ...]}` —
    coalesced events (any connection; multi-event windows only)
  - `{type:"replay", id?}` — completion marker for a replay request;
    strict-order consumers buffer interleaved live events until this arrives

`gap:true` is the "your replay seq fell outside the in-memory ring,
re-fetch via list endpoints" signal. The server cannot reconstruct
arbitrary history from SQLite — that's intentional per CLAUDE.md
principle 3.

## Code generation

`methodgen/` parses the Go AST of every `*.go` in the repo root for
`func (a *App) <Name>(...)` declarations, honors `//wails:ignore`
directives, and emits `methods_gen.go` with the static name → FNV-ID
list. Run via `go run ./internal/transport/methodgen` and committed.

`methods_gen_test.go` is a CI gate: it re-runs the generator into a
tempfile and bytes-diffs against the committed output. Adding a new
exported `App` method without regenerating fails the test.

## Per-connection transport policy

`connProfile` (conn.go) captures transport policy at upgrade time:

- **All connections**: events coalesce in a per-connection buffer
  (16 ms window / 50 event threshold) and ship as one
  `type:"batch"` frame. Single-event windows fall through to regular
  `type:"event"` frames. Coalescing applies to loopback too — the
  receiving webview pays per-message (macrotask + JSON.parse +
  effect flush), so batching protects the render loop during
  streaming bursts; latency is bounded at one window.
- **Non-loopback only**: `permessage-deflate` with context takeover
  (~1.5 MB per connection). Loopback skips compression — bytes are
  free on a local pipe, CPU isn't.

The profile is immutable for the connection's lifetime. Replay events
(`handleReplay`) always use immediate dispatch regardless of profile.

## Conventions specific to this package

- Wire-bound errors carry only generic prose (`"internal error"`,
  `"bad parameter"`). Full text + correlation id is logged
  server-side. Internal panic / file paths must never reach the wire.
- Subscriber buffers drop oldest on overflow and mark themselves
  "behind"; the next event the slow subscriber sees carries
  `Gap: true` so the client knows to re-fetch.
- `Server.Start` returns when the listener is bound; the HTTP serve
  goroutine surfaces async failure via `Server.ServeErr() <-chan error`.

## References

- Root `AGENTS.md` § "Permanent invariants" for the cross-cutting
  transport-boundary rule.
- `docs/architecture/data-flow.md` — how triage events reach the bus.
- `frontend/bindings/agent-overflow/app.ts` — generated TS bindings
  the wire-format must keep working.
- `frontend/src/lib/transport/` — the wsClient + `@wailsio/runtime` shim
  on the other side of this wire.
