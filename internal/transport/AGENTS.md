# transport/

HTTP+WebSocket transport between the Svelte frontend (whether the
Wails-embedded webview or a remote browser/desktop client) and the Go
backend.

## What this package owns

- HTTP listener serving the embedded SPA, a `/bootstrap.json` manifest,
  and the `/ws` upgrade endpoint.
- A small JSON wire frame: `{type:"rpc"|"event"|"replay", ...}`.
- Reflection-based RPC dispatch against a receiver's exported methods.
  Method IDs are FNV-1a 32-bit of `<package>.<typeName>.<methodName>`
  so they match Wails' `internal/hash.Fnv` and the existing generated
  TypeScript bindings keep working without translation.
- Per-channel bounded ring buffer for event-push replay on reconnect.
  In-memory only — the ring is a network jitter buffer, not a history
  store (see root CLAUDE.md principle 3).
- Ephemeral token authentication (`?token=<value>`).
- Boot-phase dispatcher ready-gate (`Dispatcher.HoldUntilReady` /
  `SignalReady`). main.bootTransport holds the gate so /bootstrap.json
  and the WS upgrade can serve immediately while App.ServiceStartup
  wires stores/subsystems; RPCs queued during the window park at
  `InvokeForOrigin` until App signals ready. Resolve runs BEFORE the
  wait so LocalOnlyMethods refusals stay timing-indistinguishable from
  unregistered-method probes.

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

## Wire frames

- **Client → Server**:
  - `{type:"rpc", id, methodId|method, params:[...]}` — invoke
  - `{type:"replay", lastSeqByChannel:{...}}` — request missed events
- **Server → Client**:
  - `{type:"rpc", id, result|error}` — response
  - `{type:"event", channel, seq, data, gap?}` — push

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

- Root `CLAUDE.md` § "Implemented (was previously deferred) — Remote/web
  access" for the cross-cutting boundary rules.
- `docs/architecture/data-flow.md` — how triage events reach the bus.
- `frontend/bindings/agent-overflow/app.ts` — generated TS bindings
  the wire-format must keep working.
- `frontend/src/lib/transport/` — the wsClient + `@wailsio/runtime` shim
  on the other side of this wire.
