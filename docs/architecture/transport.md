# Transport

Mechanism and rationale behind `internal/transport`, the HTTP+WebSocket wire
shared by the embedded webview, `agent-overflow --connect`, and remote browser
clients. The rules an agent needs on every edit live in
`internal/transport/AGENTS.md`; this file holds the walkthroughs that guide
points at.

## Listen-port pinning

### Why the port is stable per install

The webview's origin is host plus port. An ephemeral port changes it every
launch, which wipes every origin-scoped browser store: localStorage and the
IndexedDB thread replica (`docs/architecture/thread-replica-sync.md` §6.0).
Port obscurity was never an access control here (the bootstrap token and the
Host/Origin checks are), so pinning costs nothing.

### Where the pin lives

`main_transport_port.go` (`pinTransportPort`) owns it, not this package.
`transport.Config.Port` is injected and the package never reads a config file.

Whenever the resolved port would be 0, which covers the desktop default and an
explicit `--listen host:0` (what the Windows WSL launcher passes), the boot path
reads `transport-port.json` from the boot settings dir, beside `client-id.json`
and using the same `atomicfile` pattern, and injects it as `Config.Port`. After
`Start`, `transportPortPin.adopt` re-reads `Server.Addr()` and persists whatever
actually got bound.

First boot, a missing file, and an invalid one (garbage JSON, or a port outside
1 to 65535) are all "no pin": bind ephemeral, then record. An explicit non-zero
`--listen host:port` wins outright and neither reads nor writes the file.
Persistence is best effort. An unresolvable settings dir or a failed write logs
and leaves the run ephemeral, and never blocks boot.

### Bind failure: `Config.EphemeralPortFallback`

This is the transport half. With a non-zero `Port`, a bind that fails *because
of the port* (`portUnavailable`: EADDRINUSE, EACCES, and their WSA spellings)
retries exactly once on port 0 and logs both the failure and where it landed.
Any other bind error, notably a bind address this host does not own, still fails
`Start` loudly, since port 0 would fail identically.

`adopt` then records the new port, so a permanently squatted port churns the
origin once rather than every launch. Callers who named a port explicitly leave
the flag off. `clearOnFailedBind` deletes the pin file when `Start` failed while
a pinned port was requested, since reaching that point means an error class the
fallback predicate missed, and keeping the pin would replay the identical
failure forever.

### Rebind uses a narrower predicate on purpose

`Rebind` (the LAN toggle) is untouched by the above. `app_network.go` computes
the new addr from the live `Server.Addr()` port, so a host flip keeps the pinned
port, and `Rebind` never falls back to an ephemeral port: silently moving a live
server's port would break every connected client's origin.

Its own recovery uses the strictly narrower `addrInUse` (EADDRINUSE,
WSAEADDRINUSE). That path cures a bind by closing our live listener and
retrying, which can only help when the address was in use. A permission or
reservation refusal survives the close, so widening the predicate there would
destroy a working listener for an error it cannot fix.

### The pin can be honoured and still be wrong: `--reset-transport-port`

A bind that succeeds proves nothing about reachability. Under the Windows/WSL
launcher the backend binds inside the distro while the window connects from the
Windows host, and Hyper-V/WSL2 excluded port ranges break that hop. Those ranges
are re-seeded on every Windows reboot and routinely cover the ephemeral range an
adopted pin comes from. The in-server fallback and `clearOnFailedBind` both key
on a bind failure, so neither can see this: the pin is honoured perfectly and
the launcher's `/connectivity-error` page comes up identically on every launch,
forever, with the mitigation that page suggests already true.

Only the launcher can observe it, so the signal is explicit rather than
inferred. `cmd/agent-overflow-windows` (`launchAndProbe`) classifies a probe
that never got a single HTTP response (`errBackendUnreachable`) as unreachable,
retires that backend, and relaunches it once with `--reset-transport-port`. The
flag name has one definition, `wsllauncher.ResetTransportPortFlag`, spelled by
the launcher's argv and declared by the backend's flag set. The backend deletes
`transport-port.json` before consulting it, logs the removal, and boots
normally: ephemeral bind, then adopt.

A reset with no pin is an ordinary boot. A reset alongside an explicit
`--listen host:port` leaves the file alone, because that boot never consults it.
One retry only: a fresh port costs the user every origin-scoped browser store,
and a second unreachable port means the forwarding path itself is broken, which
is what the error page covers.

## Replay rings and gap markers

The per-channel ring (`eventbus.go`) is a network jitter buffer, not a history
store (root `AGENTS.md` principle 3). The server cannot reconstruct arbitrary
history from SQLite, so when a reconnecting client asks for something the ring
no longer holds, the honest answer is "re-fetch through the list endpoints".
That answer is `gap:true` on the next frame for that channel.

### A cursor can fall outside the ring at either end

- **Below the oldest retained seq.** Eviction lost what the client wanted. The
  ordinary case.
- **Above the current head.** The client is holding a sequence space that is not
  ours, because a restarted backend re-seeds every channel from 1. Answering
  "nothing missed" would leave it dropping every live event below its stale
  cursor forever, since the client dedups on seq.

### The marker is a resync instruction, not a late event

Because of the above-head case, a gap marker's seq can be *lower* than the
client's cursor. Clients must therefore honour `gap:true` before their own dedup
check, and reset the channel cursor to the marker's seq in both directions
(`wsClient.handleEventEntry`).

It is also why the latest-only newest-frame substitution applies to the eviction
side only: to an ahead cursor the newest frame's seq would read as a duplicate.

Retention classes shape what `Replay` returns, not whether the rule holds.
`RetentionEphemeral` channels answer with nothing and no gap marker, and
`RetentionLatestOnly` channels answer with their single newest frame and no gap
marker for an evicted cursor, because those frames are superseded state rather
than lost history. An above-head cursor still gaps in both classes: that is a
client-state fault, not a retention question.

### Live-connection drops are announced by the server too

`gap:true` also covers drops within a live connection. When an event does not
fit a subscriber's buffer, `Subscriber.deliver` flags that channel in the
subscriber's `gapped` set and announces the loss on the next opportunity: the
next event that fits on the flagged channel is re-encoded for that subscriber
with `gap:true`, and any OTHER flagged channels get standalone
`{gap:true, data:null}` markers (the same shape `Replay` sends) flushed ahead
of whatever delivers next. Latest-only channels are never flagged, because
their next frame supersedes the lost one, which mirrors `Replay`'s carve-out.

Before 2026-08-29 the server never recorded a drop, and detection relied
entirely on the client noticing a seq forward-skip on a LATER same-channel
delivery. A flood starves exactly that: during a subagent fan-out storm the
dropped channel's next frames were themselves dropped, so no skip was ever
observable and a pane sat truncated for 30-40s on a healthy connection.

`wsClient.handleEventEntry` keeps the client half: an event whose seq is more
than one past the channel's cursor is treated as a gap, with the same
synthetic `transport:gap` dispatch, and the carried event is still delivered,
which is real data. Without it, a single drop on an edge-triggered channel
(`git:status`, `pr:updated`, and `mcp:status` emit exactly one frame per state
change) leaves every consumer of that entity stale until the entity next
changes. Both detection paths persist a diagnostic through
`reportFrontendDiagnostic`, so a storm leaves evidence in
`frontend-errors.jsonl` rather than only in a devtools console nobody has
open.

### Forward-skip detection is scoped to one connection

It fires only when the channel's previous event arrived on the current socket.
Across a reconnect a forward skip is expected and not a drop, because `Replay`
answers an ephemeral channel with nothing and a latest-only channel with just
its newest frame. A client judging those against a carried-over cursor would
resync spuriously on every reconnect. Within a connection there is no such
ambiguity: every event on a visible channel is either delivered or dropped.

## The scoped-token route

`POST /rpc` (`ScopedRPCPath`, `httprpc.go`) is the one-shot HTTP RPC the `ao`
CLI speaks. A CLI process makes one call and exits, so it gets a POST rather
than a WebSocket with a replay ring: one `ClientFrame` in, one `ServerFrame`
out, body capped at `maxScopedRPCBody`.

HTTP status carries transport-level outcomes only (bad verb, unreadable body,
unauthenticated). Everything the dispatcher can answer, including the
authorization refusals, comes back 200 with a `ServerFrame` error envelope, so
the CLI has exactly one place to look for a machine-readable code.

`invokeScoped` takes a method NAME only. Numeric ids exist so generated bindings
can skip a string lookup, and a CLI has no generated bindings, so accepting ids
here would mean keying the allow-list twice. The name is filtered by
`AuthorizeScopedMethod` against the caller's scope, then goes through the same
`ResolveForOrigin` and `InvokeForOrigin` path the WebSocket uses, with the scope
on the context. Non-loopback peers were already refused with a 404 before any of
that, so `isLoopback` is true by construction and the CLI receives the method's
real error text, which is the only diagnostic a headless caller has.

The token registry lives in the app (`App.aoTokens`); this package consults it
through the narrow `ScopedTokens` interface. `registerAOTokenLocked` and
`revokeAOTokenLocked` are its only mutators and both run from the session-map
mutators in `app_session_manager.go`, so a token is registered exactly when its
session enters the map and revoked exactly when it leaves. A resolved scope
therefore always names a live session, and an unknown token is
indistinguishable from a revoked one: both are a bare 401.

## Additional receivers

`Dispatcher.Register` accepts more than one receiver, and the only one besides
the repo-root `App` is the harness's `Harness`, registered solely by the
`--harness` boot path with `RegisterOptions{LocalOnly: true}`. The whole
receiver is refused for non-loopback peers, and outside harness mode its methods
do not exist on the wire at all. `Config.Harness` is the manifest half of the
same switch: it makes `/bootstrap.json` carry `"harness": true`, and `main.go`
sets it from the very expression that registers the receiver, so the manifest
can never claim a harness whose methods are absent. It announces a mode and
grants nothing. The SPA keys its harness bridge import on it so an ordinary boot
never loads that module.

Rules for any future receiver:

- Gate registration on the boot path that needs it. A receiver that exists on
  every boot belongs on `App` instead.
- Do not collide with `App` method names, since name-based dispatch shares one
  namespace. Use a distinctive prefix, as `Harness*` does.
- Receiver-level `LocalOnly` is coarse by design. A receiver that needs
  per-method classification extends `internalmethods.go` rather than re-checking
  origin in method bodies.

## Event coalescing

Every connection buffers pushed events for one 16 ms window or 50 events,
whichever comes first, and ships them as a single `type:"batch"` frame.
Single-event windows fall through to an ordinary `type:"event"` frame.

Coalescing applies to loopback too. The receiving webview pays per message (a
macrotask, a `JSON.parse`, an effect flush), so batching is what protects the
render loop during streaming bursts, and latency stays bounded at one window.

Replay (`handleReplay`) ships through the same batch envelope in chunks of the
same 50-event threshold, but without the timer: the whole backlog is already in
hand, so chunking adds no latency. A reconnect during heavy streaming can drain
up to `DefaultRingCapacity` (1000) events, and per-event frames gave that worst
case the least protection.

Ordering is preserved end to end. `writeBatchFrame` writes one chunk at a time
under `writeMu`, `spliceBatchFrame` keeps slice order, every consumer iterates
entries in order, and the `type:"replay"` completion marker still lands last.

## Keepalive and connection death

Three mechanisms, one per failure mode.

**Client-visible `{type:"ping"}` frames**, one per keepalive interval. Two jobs:
they keep intermediary connection state warm, because the Windows to WSL2
localhost relay tore down mid-session connections with a clean FIN (incident
2026-07-28), and they give browser clients, which cannot observe protocol pings,
a guaranteed traffic floor.

The SPA's stale-socket watchdog (`wsClient.ts STALE_TRAFFIC_THRESHOLD_MS`, three
heartbeat periods) force-closes a connected socket that has received nothing for
that long, because a half-open TCP connection with the peer gone and no FIN
never fires a close event on its own. It makes no silence verdict while
`document.hidden`, when browser scheduling may delay both its interval and
WebSocket message delivery; becoming visible resets the traffic clock before
verdicts resume. The watchdog arms per connection: the first ping frame proves
this server heartbeats, and the proof resets on close,
so version skew in either direction cannot reconnect-loop an idle but healthy
connection. It also stands down while a remote backend has RPCs in flight, since
one large response frame can legitimately silence the wire past the threshold.

**Protocol-level pings**, on every third tick, with a pong timeout. These detect
half-open connections server-side, where writes into a dead TCP window buffer
silently. Pongs are only surfaced while the read loop sits in `ws.Read`, so a
missed pong convicts only when the reader was actually parked in Read with no
recent frame (`inRead` plus `lastReadAt`). A reader busy streaming a replay or
waiting on the RPC semaphore proves nothing about the peer. On a convicting
timeout the conn is closed so the handler tears down instead of lingering.

**A write deadline** (`writeTimeout`) on every wire write through `writeRaw`. A
peer that stops draining would otherwise block a write forever while holding
`writeMu`, wedging the event pump and the keepalive loop with it. On expiry
coder/websocket tears the connection down, which is exactly the teardown that
peer needs.

### Close logging

Every connection close logs one line, graceful closes included, with peer
address, duration, and close reason (`closeReason`). Close status 1005 with no
close frame is the intermediary-teardown signature, 1006 is a network drop, and
1000 or 1001 is a client navigation.

The duration in that line is the same quantity the client's reconnect ladder
judges itself on. `wsClient` resets its backoff only after a connection survived
`BACKOFF_RESET_AFTER_MS`, so a relay that tears down long-lived sessions keeps
reconnecting fast, while an accept-then-close backend backs off instead of
storming.

## References

- `internal/transport/AGENTS.md` for the authz, replay, and classification rules.
- `docs/architecture/data-flow.md` for how triage events reach the bus.
- `docs/architecture/root-decomposition.md` § Wire compatibility for why method
  IDs hash under `main.App` regardless of where the code lives.
- `frontend/src/lib/transport/` for the client half.
