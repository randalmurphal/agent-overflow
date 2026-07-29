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

## Scoped tokens (the `ao` CLI surface)

`scopedtoken.go` + `httprpc.go` add a SECOND, strictly narrower credential
class for the `ao` CLI (spec §5, D15). It is not a second API: the same
dispatcher, frame types, and method table serve it.

- **Route.** `POST /rpc` (`ScopedRPCPath`), `Authorization: Bearer <token>`,
  one `ClientFrame` in and one `ServerFrame` out. A CLI process makes one call
  and exits, so it gets a POST rather than a WebSocket with a replay ring.
  Loopback-only, and a non-loopback peer gets a 404 so the route stays
  unfingerprintable. The server's own session token is **not** honoured here —
  this surface can never be wider than the table below, however it is reached.
- **Registry.** The app owns it (`App.aoTokens`, mutated only from the session
  map in `app_session_manager.go`); this package consults it through the narrow
  `ScopedTokens` interface. A token exists exactly as long as the provider
  session it was minted for, so a resolved scope always names a live session.
  An unknown token and a revoked one are both a bare 401, by design.
- **Scope.** `CallerScope` travels on the request context
  (`WithCallerScope` / `CallerScopeFrom`), never as a parameter the caller
  could supply. `interactive` is a human-driven thread whose every invocation
  passes the provider's own bash-approval UX; `phase` is an unattended workflow
  phase carrying the grants its workflow FROZE at start.
- **Method table.** `ScopedTokenMethods` is a closed allow-list mapping method
  name to the grants that admit it. Anything absent — every non-workflow RPC,
  every `LocalOnly` method outside the table — is `method_not_found` for a
  scoped token, exactly as an unregistered method would be. An interactive
  scope may call everything listed; a phase scope needs one of the listed
  grants, and gets the typed `grant_required` refusal (`ErrCodeGrantRequired`)
  naming what to add. That refusal is deliberately distinct from
  `method_not_found`: the route is loopback-only and the caller is our own CLI,
  so naming the grant leaks nothing while making a misconfigured workflow
  fixable.
- **What this layer does NOT decide.** Row-level scoping ("which runs may this
  phase touch") depends on the run record, not the method name. The bound
  methods enforce it from the scope on their context — see `app_workflow_cli.go`.

Adding a method to `ScopedTokenMethods` widens what a compromised agent session
can do. Do it only for methods whose row-level authorization is enforced from
`CallerScopeFrom`, and add it to `LocalOnlyMethods` too.

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

## Event-channel visibility

`event_visibility.go` filters pushed events per connection origin:

- `loopbackOnlyEventChannels` — frames carrying local-terminal bytes or
  identity/billing data that a LAN peer must not see.
- `remoteOnlyEventChannels` — frames that exist purely to hide WAN
  round-trip latency (`highlight:seed`) and are waste on a local pipe.
  The producer is ALSO gated on `Server.HasRemoteClient()` (an atomic
  count of non-loopback WS connections), so no work happens when only
  loopback clients are attached; the wire filter is what keeps the
  frames off loopback pipes while a remote viewer keeps the producer
  running. Caveat: SSH-tunneled remotes arrive as loopback and are
  invisible to the probe — they keep the RPC path.
  (`highlight:diff_seed` used to sit here too; it goes to every client
  now because its persist-time seeds can be parse-primed — better than
  the loopback RPC recompute — and local clients consume them as
  in-place cache upgrades.)
- `ephemeralEventChannels` — both seed channels are also excluded from
  replay-ring retention (`eventbus.go` gives them a zero-capacity
  ring: sequence tracking only). Seeds are point-in-time cache warmers
  — replaying superseded frames after a reconnect is useless, and each
  frame can carry large span/hash arrays that would otherwise sit in
  the ring up to `DefaultRingCapacity` deep. Replay for these channels
  returns nothing and no gap marker.
- `latestOnlyEventChannels` — unkeyed whole-state channels
  (`system:stats`) get a capacity-1 ring: the newest frame fully
  supersedes all prior ones, so a default-depth ring would retain
  hundreds of stale samples forever and replay them all on reconnect.
  Replay delivers the single newest frame and never a gap marker —
  evicted frames are superseded state, not lost history. Keyed
  channels (git:status, provider:usage, discussion:state, mcp:status)
  must NOT join this set: capacity 1 would evict other keys' latest
  frames.

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
  - `{type:"ping"}` — keepalive heartbeat (see below). Consumers that
    switch on known frame types skip it for free; the SPA uses its
    arrival as the liveness signal for stale-socket detection

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

## Keepalive and connection-death detection

Every connection runs a keepalive loop (conn.go `keepalive`; cadence
and pong timeout default to 10s and are overridable per server via
`Config.KeepaliveInterval` / `Config.KeepalivePongTimeout` — test
knobs, not production tuning):

- A client-visible `{type:"ping"}` frame every keepalive interval.
  Two jobs: keeps intermediary connection state warm — the
  Windows↔WSL2 localhost relay tore down mid-session connections with
  a clean FIN (incident 2026-07-28) — and gives browser clients, which
  cannot observe protocol pings, a guaranteed traffic floor. The SPA's
  stale-socket watchdog (`wsClient.ts STALE_TRAFFIC_THRESHOLD_MS`, 3
  heartbeat periods) force-closes a connected socket that has received
  nothing for that long: a half-open TCP (peer gone, no FIN) never
  fires a close event on its own. The watchdog arms per connection —
  the first ping frame proves this server heartbeats, and the proof
  resets on close — so version/deployment skew in either direction
  can't reconnect-loop an idle-but-healthy connection. It also stands
  down while a remote backend has RPCs in flight (a single large
  response frame can legitimately silence the wire past the
  threshold). Keep the two constants in ratio when changing either.
- Every 3rd tick additionally round-trips a protocol-level ping with a
  pong timeout, detecting half-open connections server-side (writes
  into a dead TCP window buffer silently). Pongs are only surfaced
  while the read loop sits in `ws.Read`, so a missed pong is only
  treated as fatal when the reader was actually parked in Read with no
  recent frame (`inRead` + `lastReadAt`) — a reader busy streaming a
  replay or waiting on the RPC semaphore proves nothing about the
  peer. On a convicting timeout the conn is closed so the handler
  tears down instead of lingering.
- Every wire write goes through `writeRaw`, bounded by `writeTimeout`
  (30s). A peer that stops draining would otherwise block a write
  forever while holding `writeMu`, wedging the event pump and the
  keepalive loop; on expiry coder/websocket tears the connection down.

Every connection close logs ONE line — graceful closes included — with
peer address, duration, and close reason (`closeReason`): close status
1005 with no close frame is the intermediary-teardown signature, 1006
a network drop, 1000/1001 a client navigation. Per-write errors on an
already-closed conn stay suppressed (`isClosedError`).

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
