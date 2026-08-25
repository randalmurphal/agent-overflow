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
- Where the listen port comes from. `Config.Port` is injected; this
  package never reads a config file.

## The listen port is pinned per install

An ephemeral port makes the webview's origin (host + port) change every
launch, which wipes every origin-scoped browser store — localStorage and
the IndexedDB thread replica (`docs/specs/thread-replica-sync.md` §6.0).
So whenever the resolved port would be 0 — the desktop default AND an
explicit `--listen host:0`, which is what the Windows WSL launcher
passes — `main.go` (`main_transport_port.go`, `pinTransportPort`) reads
a pinned port from `transport-port.json` in the boot settings dir
(alongside `client-id.json`, same atomicfile pattern) and injects it as
`Config.Port`. After `Start` it re-reads `Server.Addr()` and persists
whatever actually got bound. First boot, a missing file, and an invalid
one (garbage JSON, port outside 1–65535) are all "no pin": bind
ephemeral, then record. An explicit non-zero `--listen host:port` wins
outright and neither reads nor writes the file. Persistence is
best-effort — an unresolvable settings dir or a failed write logs and
leaves the run ephemeral, never blocks boot. Port obscurity was never an
access control here (the bootstrap token and the Host/Origin checks
are), so pinning costs nothing.

`Config.EphemeralPortFallback` is the transport half: with a non-zero
`Port`, a bind that fails **because of the port** (`portUnavailable` —
EADDRINUSE, EACCES, and their WSA spellings) retries exactly once on
port 0 and logs both the failure and where it landed; any other bind
error — notably a bind address this host does not own — still fails
Start loudly, since port 0 would fail identically. `main.go` then adopts
the new port into the file, so a permanently squatted port churns the
origin once rather than every launch. Callers who named a port
explicitly leave the flag off. `Rebind` (the LAN toggle) is untouched by
all of this: `app_network.go` computes the new addr from the live
`Server.Addr()` port, so a host flip keeps the pinned port, and Rebind
never falls back to an ephemeral port — silently moving a live server's
port would break every connected client's origin. Rebind's own recovery
uses the strictly narrower `addrInUse` (EADDRINUSE / WSAEADDRINUSE):
that path cures a bind by CLOSING our live listener and retrying, which
can only help when the address was in use — a permission/reservation
refusal survives the close, so widening the predicate there would
destroy a working listener for an error it cannot fix.

### The pin can be honoured and still be wrong: `--reset-transport-port`

A bind that SUCCEEDS proves nothing about reachability. Under the
Windows/WSL launcher the backend binds inside the distro while the
window connects from the Windows host, and Hyper-V/WSL2 excluded port
ranges — re-seeded on every Windows reboot, routinely covering the
ephemeral range an adopted pin comes from — silently break that hop. The
in-server fallback and `clearOnFailedBind` both key on a bind FAILURE,
so neither can ever see this: the pin is honoured perfectly and the
launcher's `/connectivity-error` page comes up identically on every
launch, forever.

Only the launcher can observe it, so the signal is explicit rather than
inferred. `cmd/agent-overflow-windows` classifies a probe that never got
a single HTTP response (`errBackendUnreachable`) as unreachable,
retires that backend, and relaunches it ONCE with
`--reset-transport-port` (`wsllauncher.ResetTransportPortFlag` — one
definition, spelled by the launcher's argv and declared by the backend's
flag set). The backend deletes `transport-port.json` before consulting
it, logs the removal, and boots normally: ephemeral bind, then adopt. A
reset with no pin is an ordinary boot, and a reset alongside an explicit
`--listen host:port` leaves the file alone, because that boot never
consults it. One retry only — a fresh port costs the user every
origin-scoped browser store, and a second unreachable port means the
forwarding path itself is broken, which is what the error page covers.

## Token refusal is a 404, and the SPA depends on it

Both credentialled entry points — `/bootstrap.json?t=` (`handleBootstrap`)
and the `/ws` upgrade (`upgrade`) — answer a wrong or empty token with
`http.NotFound`, deliberately indistinguishable from "no such path" so a
LAN scanner can't fingerprint us. That 404 is the only positive
auth-rejection signal on the wire, and it is the one the SPA keys its
terminal state on: `wsClient` refetches the manifest during a reconnect
outage, and a 401/403/404 there latches `'unauthorized'` and STOPS the
reconnect ladder for a session served over the network (tokens are
per-launch, so only a freshly-opened share link can recover it). A
transient failure must therefore keep its own status — the readiness gate
stays 503 and the startup-failure page stays 500; neither may become a
404, or a client that is merely early would be told its credential is
dead. See `frontend/src/lib/transport/bootstrap.ts`
(`BootstrapRejectedError`) and `wsClient.ts` (`enterCredentialDead`).

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

- **One method on this route BLOCKS.** `WorkflowAgentWatchRun` holds its request
  until the run it names moves, bounded by the app's own
  `maxWorkflowWatchHold` (25s). Nothing in this package special-cases it — a
  POST gets its own goroutine, and there is no per-route concurrency bound — but
  two server timeouts do bracket it: `HTTPReadTimeout` / `HTTPWriteTimeout`
  (60s each) must stay comfortably above that hold, or the transport would kill
  a healthy blocked call and every quiet minute would read as a dead backend.
  Keep the ordering `hold < CLI rpcTimeout (30s) < HTTP write timeout` in mind
  before changing any of the three.

`GrantNotRequired` (`"*"`) is the table's way of saying "this method's authority
is ENTIRELY row-level": it admits every scoped token, phase or interactive,
whatever grants the phase froze. It is not a grant — no workflow may declare it,
and `def.KnownGrant` does not know it, which a test pins in both directions.
Campaign memory is what it exists for: recording what the work learned is part of
doing the work, exactly as returning an envelope is, and a `grants:` line
standing between an element and its own campaign's memory would mean every
workflow that forgot one silently relearns everything each wave. Use it only for
a method that is part of doing the work rather than an extra capability, AND
whose row scoping is enforced from `CallerScopeFrom`. A method that widens what a
phase may REACH still needs a grant of its own.

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

## Event-channel policy registry

`event_channels.go` holds `channelPolicies`, ONE authored row per
channel the app emits: `{Channel, Audience, Retention, Why}`. It is the
source of truth for both questions decided per channel — who may
receive its frames and how deep its replay ring is — and it cannot be
generated, because emit sites are spread across the root package,
`internal/triage`, `internal/workflow` and others, and several build
their channel name at runtime. **Adding a channel means adding a row.**

- `Audience` — `AudienceAny` / `AudienceLoopbackOnly` /
  `AudienceRemoteOnly`.
- `Retention` — `RetentionDefault` (full ring) / `RetentionEphemeral`
  (capacity 0) / `RetentionLatestOnly` (capacity 1). The class-level
  doctrine, including the UNKEYED membership rule for latest-only,
  lives on the constants.
- `Why` — the decision. A `Why` containing `"unreviewed"` marks a row
  that inherited the fail-open default rather than one anyone decided.
  `TestChannelPolicyUnreviewedWorklist` prints them (`go test
  ./internal/transport/ -run UnreviewedWorklist -v`) and never fails.

A channel with NO row still gets the fail-open default
(`unregisteredChannelPolicy`: broadcast, full ring). That is deliberate
for now — see its `TODO` for the planned fail-closed flip and what
blocks it. Two harness-only emit paths (`HarnessEmit`,
`harness.Replayer`) publish onto arbitrary caller-named channels and
are unregistrable by construction; the file header documents them
alongside the three dynamic families that DO resolve to registered
names.

`TestChannelPolicyPreservesFrozenClassification` freezes the contents
of the four hand-authored maps the registry replaced. Changing one of
those lists is a behavior change, not a refactor.

## Event-channel visibility

`event_visibility.go` filters pushed events per connection origin. Its
two sets are DERIVED from `channelPolicies` at init — do not edit them
directly. They survive as maps only because this is the one hot
registry consumer (per event per subscriber, and again per event per
connection); the two retention classes below are cold and read the
registry directly through `channelRetention`:

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
- `RetentionEphemeral` — channels excluded from replay-ring
  retention (`eventbus.go` gives them a zero-capacity ring: sequence
  tracking only). Both seed channels are here because seeds are
  point-in-time cache warmers — replaying superseded frames after a
  reconnect is useless, and each frame can carry large span/hash
  arrays that would otherwise sit in the ring up to
  `DefaultRingCapacity` deep. `updater:install` is here for the
  opposite reason: it is an imperative directive (swap the app binary
  and restart), valid only for the install in flight when it was
  emitted, so replaying it to a launcher that reconnects would
  spontaneously restart the app on a stale instruction. Replay for
  these channels returns nothing and no gap marker — except for an
  above-head cursor, which is a client-state fault rather than a
  retention question (see the gap discussion under Wire frames).
- `RetentionLatestOnly` — unkeyed whole-state channels
  (`system:stats`) get a capacity-1 ring: the newest frame fully
  supersedes all prior ones, so a default-depth ring would retain
  hundreds of stale samples forever and replay them all on reconnect.
  Replay delivers the single newest frame and never a gap marker for
  an evicted cursor — those frames are superseded state, not lost
  history (an above-head cursor still gaps). Keyed
  channels (git:status, provider:usage, discussion:state, mcp:status)
  must NOT join this set: capacity 1 would evict other keys' latest
  frames.

To change any of the four memberships, edit the channel's row in
`channelPolicies`; nothing else needs touching.

## Events Are Entity-Keyed

A pushed frame is addressed by the ENTITY it describes — a cwd
(`git:status`), a PR key (`pr:updated`), a thread id, a project — never
by the subscription that happens to be listening. Subscription ids stay
legitimate on the RPC RESULT that hands out the unsubscribe /
ConnState-cleanup handle (`GitStatusSubscriptionResult.ID`): that is a
per-caller lease, not an address.

The reason lives on the client. Two panes routinely watch one entity —
two threads on one worktree is the default for project-root threads, and
"implement this plan in a new thread" inherits the source worktree. A
subscription-keyed frame forces each pane to keep a private copy filtered
by its own handle, and those copies drift: they disagreed about whether
there was anything to commit for minutes at a time before `git:status`
was re-keyed (audit 2026-08-08). One entity-keyed frame heals every
consumer.

It follows that the producer is refcounted per entity, not per caller: N
subscribers on one cwd share one `gitwatch.Subscription`, one forwarding
goroutine, and one frame per change. Pause/resume composes across
subscribers (active if ANY subscriber is active), and fetch errors ride
the payload so consumers can show them.

`TestWirePayloadsAreEntityKeyedNotSubscriptionKeyed` (repo root) fails on
any struct field that serializes as `subscriptionId`.

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

A replay cursor can fall outside the ring at EITHER end, and both gap:

- **Below the oldest retained seq** — eviction lost what the client
  wanted. The ordinary case.
- **Above the current head** — the client is holding a sequence space
  that isn't ours (a restarted backend re-seeds every channel from 1).
  Answering "nothing missed" would leave it dropping every live event
  below its stale cursor forever, because the client dedups on seq.

That second case makes a gap marker a RESYNC INSTRUCTION, not just a
late event: its seq can be lower than the client's cursor, so clients
must honour `gap:true` before their own dedup check and reset the
channel cursor to the marker's seq in both directions
(`wsClient.handleEventEntry`). It is also why the latest-only
newest-frame substitution applies to the eviction-side gap only — the
newest frame's seq would read as a duplicate to an ahead cursor.

`gap:true` is the RECONNECT half of the story, and the server is the
only party that can raise it. The other half is client-side: a live
event whose seq is more than one past that channel's cursor means the
events between them were dropped into a full subscriber buffer
(`Subscriber.deliver`), which the server never records and no later
frame announces. `wsClient.handleEventEntry` treats that forward skip as
a gap — same console warning, same synthetic `transport:gap` dispatch —
and still delivers the carried event, which is real data. Without it a
single drop on an edge-triggered channel (`git:status`, `pr:updated`,
`mcp:status` emit exactly one frame per state change) leaves every
consumer of that entity stale until the entity next changes.

That detection is scoped to ONE connection: it fires only when the
channel's previous event arrived on the current socket. Across a
reconnect a forward skip is expected and not a drop — `Replay` answers
an ephemeral channel with nothing at all, and a latest-only channel
with just its newest frame — so a client that judged those against a
carried-over cursor would resync spuriously on every reconnect. Within
a connection there is no such ambiguity: every event on a visible
channel is either delivered or dropped.

## Code generation

`methodgen/` parses the Go AST of every `*.go` in each scanned
directory for `func (a *<Receiver>) <Name>(...)` declarations, honors
`//wails:ignore` directives, and emits `methods_gen.go` with the static
name → FNV-ID list. Run via `go run ./internal/transport/methodgen` and
committed.

What it scans is the `receiverSpecs` list in `methodgen/main.go`: one
`{Dir, Receiver, Package, TypeName}` tuple per receiver, merged and
sorted by method name. `Package`/`TypeName` are the FQN labels the
method hashes under, not facts about where the code lives — a service
promoted into `internal/<pkg>` keeps `{Package: "main", TypeName:
"App"}` and its IDs never move (see
`docs/architecture/root-decomposition.md` § Wire compatibility). A
method name claimed by two specs is a codegen error naming both FQNs,
mirroring the dispatcher's `byName` collision refusal.

Today the list holds exactly one entry: the repo-root `App`. `Harness`
is deliberately not in it — the generated table is the App allow-list
`bootTransport` passes on the App registration alone, while `Harness`
registers unfiltered and receiver-level `LocalOnly` under `--harness`
only. Adding a spec widens both the production allow-list and the
LAN-safety classification gate in `methods_gen_test.go`.

`methods_gen_test.go` is a CI gate: it re-runs the generator into a
tempfile and bytes-diffs against the committed output. Adding a new
exported `App` method without regenerating fails the test.
`methodgen/main_test.go` covers the multi-spec path itself against
`methodgen/testdata/`.

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

The profile is immutable for the connection's lifetime. Replay
(`handleReplay`) ships through the same `type:"batch"` envelope, in
chunks of the same 50-event threshold, but without the timer — the
whole backlog is already in hand, so chunking adds no latency. A
reconnect during heavy streaming can drain up to
`DefaultRingCapacity` (1000) events; per-event frames gave the worst
case the least protection. Ordering is preserved end to end
(`writeBatchFrame` per chunk under `writeMu`, `spliceBatchFrame` keeps
slice order, every consumer iterates entries in order), and the
`type:"replay"` completion marker still lands last.

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
a network drop, 1000/1001 a client navigation. The duration in that
line is the same quantity the client's reconnect ladder judges itself
on — `wsClient` resets its backoff only after a connection survived
`BACKOFF_RESET_AFTER_MS`, so a relay that tears down long-lived
sessions keeps reconnecting fast while an accept-then-close backend
backs off instead of storming. Per-write errors on an
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
