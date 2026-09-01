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
Port obscurity was never an access control here (the page credential and the
Host/Origin checks are), so pinning costs nothing. The page cookie's name
carries the port, so a stable port also means a stable cookie name.

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

## The credential channel

One launch, one session token, one validation function
(`Credential.Authenticate`, `credential.go`). Everything below is about how
that token reaches a request, never about a second policy.

### A page URL carries a ticket, not the token — and a window we own carries neither

A page URL travels: through window history, shell arguments, launcher logs,
`--print-url-fd` output, and screenshots. So the most it ever carries is a
**one-time page ticket** (`?t=`), and what a ticket buys is exactly one cookie.

```
Server.AppURL()  ──mint──▶  http://127.0.0.1:34567/?t=<ticket>&cid=…
                                      │
       browser loads the shell, SPA fetches /bootstrap.json?t=<ticket>
                                      │
       Credential.Exchange: validate ─▶ consume ticket ─▶ Set-Cookie
                                      │
       ao_page_34567=<token>; HttpOnly; SameSite=Strict; Path=/
                                      │
       bootstrap.ts strips ?t= from the URL; reload rides the cookie
```

A URL is the only channel that reaches a BROWSER, so that is the browser's
path. A WEBVIEW window is a different situation: the Go process that mints the
ticket also holds the window and can evaluate script in the document it just
loaded, so the credential never has to enter the URL at all — and the reasons
above are reasons not to put it there.

```
Server.WebviewPageURL()  ─────▶  http://127.0.0.1:34567/?host=webview&cid=…
                                      │
       page loads, announces itself, SPA waits on window.__aoPageTicket
                                      │
  uiwindow.DeliverPageTicket ─mint─▶ ExecJS(pagehost.DeliveryScript(ticket))
                                      │
       SPA fetches /bootstrap.json?t=<ticket> ─▶ same Exchange, same cookie
```

`?host=webview` is a marker, not a credential: it tells the page its ticket is
arriving by injection so it waits for one instead of booting bare.
`internal/pagehost` holds the marker, the two names the script writes
(`window.__aoPageTicket` and the `ao:page-ticket` event, one per race
direction) and the script itself, stdlib-only so the Windows launcher can link
it. The trigger is Wails' `WindowRuntimeReady`, which the SPA raises for itself
— it replaces `@wailsio/runtime`, so nothing else in the page will — and a
ticket is minted per announcement, which is what gives a reloaded document a
live one. All three window hosts share `uiwindow.DeliverPageTicket`: the
desktop and isolated windowed boots, the Windows/WSL launcher, and the
`--connect` stub. **No ticket appears in a webview URL.**

Minted tickets are held oldest-first and bounded at `maxOutstandingTickets`
(16). The bound matters because the settings panel's LAN share URL mints one
per render; evicting the oldest keeps the newest URL — the one a user just
copied — always valid.

There is **one ticket mechanism** (`ticket.go`), shared by the page ticket and
the WebSocket ticket below. Mint a CSPRNG token over an already-authenticated
channel, let the first presentation spend it. The two users differ in exactly
two parameters — the page ticket has no subject and no deadline, the WS ticket
names a session and lives 30 seconds — and a third single-use token is a third
set of parameters, never a third implementation.

The exchange is single-use by construction: the second `/bootstrap.json`
presenting the same ticket finds nothing to consume, and with no cookie either
it gets the standard 404. A bookmarked `?t=` URL from a previous launch behaves
identically, because tokens and tickets are both per-launch: the SPA sees a
refused manifest and latches its existing `unauthorized` state. Nothing wedges.

### Three carriers, one check

| Carrier | Who uses it | Why not one of the others |
|---|---|---|
| `ao_page_<port>` cookie | every browser request after the exchange | script cannot read it back |
| `Authorization: Bearer` | WSL launcher probe + notification socket, `ao-harness`, a same-host `--connect` stub's upstream hop, e2e's `/pageurl` calls | keeps the credential out of URLs, process listings, logs |
| `?token=` | the browser and Node WebSocket APIs | those APIs build a URL and nothing else — no handshake headers |

`Authenticate` reads all three and ends in the same `ConstantTimeEqual`.
`Exchange` is `Authenticate` plus the ticket consumption and the `Set-Cookie`.
A route that needs a credential calls one of those two; there is no third path,
and adding one is the mistake this shape exists to prevent.

### The launch credential admits an upgrade only from this machine

`Authenticate` says which backend LAUNCH a client belongs to. It does not say
WHICH client, and that is the whole difference: a connection carrying only the
launch credential has no session id, so `CloseSession` has nothing to reach it
by and the per-RPC gate has no grant set to read. It is unattributable and
unrevocable by construction.

That is fine while the peer is one of this host's own processes, and it is not
fine off-host. So `handleWS` requires a non-loopback peer to NAME a live durable
session (spec §4, "Local clients") — through the spent `?ticket=`, the
`X-AO-Session` header, or the `ao_session_<port>` cookie, the three carriers
`SessionForRequest` already reads. Locality is `loopback.PeerAddress` over the
kernel-reported peer, the same predicate the event filter, the step-up proof and
`internal/app`'s `bindingAdmitsPeer` use.

| Peer | Presents | Upgrade |
|---|---|---|
| loopback | launch credential, no session | admitted — the webview, `ao-harness`, the e2e rig, the launcher's notification socket, the `--connect` stub's carried hop when it is same-host |
| loopback | a live session | admitted |
| non-loopback | a live session (ticket, header, or cookie) | admitted — a paired browser, and a `--connect` stub that paired with this backend (`internal/deviceclient`), both on a ticket |
| non-loopback | launch credential alone | `http.NotFound` |
| non-loopback | nothing | `http.NotFound` |

This narrows what a sessionless credentialled connection may be; it loosens
nothing. The launch credential is still demanded wherever it was demanded
before, and the refusal is the same 404 a bad credential and a missing route
both answer, so a LAN scanner still learns nothing from which rule refused it.

The LAN share URL keeps loading the page — `/bootstrap.json` is unchanged, and
the manifest must not be stricter than the socket it describes. What changes is
what happens next: a networked page that never paired reaches a refused upgrade,
and the SPA presents that as a pairing prompt rather than as an outage
(`frontend/src/lib/transport/AGENTS.md`).

### Why the origin check is load-bearing, not defence in depth

Cookies are scoped by host and path — **not by port**. Any page served by any
other listener on the same host therefore has our page cookie attached to
requests it makes at us, and a WebSocket handshake is not subject to the
cross-origin read rules that keep such a page out of `/bootstrap.json`. SameSite
does not close this: same-site ignores ports too.

So `OriginAllowed` runs first on `/ws`, `/bootstrap.json` and `/pageurl`, and it
answers from the request rather than a stored string: scheme from the TLS state,
authority from the `Host` header, plus whatever patterns a LAN bind added. A
request carrying no `Origin` at all is a client that is not a browser and passes
to the credential and peer rules — that is the exception that keeps `ao-harness`
and the launcher on the same validation function instead of a bypass. `upgrade`
hands coder/websocket `InsecureSkipVerify: true` precisely because this package
has already made the decision, so one rule holds on loopback and LAN alike.

The TLS state, and deliberately not the `X-Forwarded-Proto` header that the
manifest's socket URL reads (`requestIsHTTPS`). The two are the same question
with different stakes: a wrong scheme on `wsUrl` produces a URL the browser
cannot connect to, while a wrong scheme here would let a caller widen an
allow-list by describing itself. A deployment behind a TLS-terminating proxy
therefore still allow-lists its origin explicitly.

### `/pageurl`: a ticket is spent, and some clients navigate twice

`PageURLPath` answers a fresh page URL to a caller that already holds the
credential. It exists because several consumers navigate more than once per
launch and a ticket is spent by the document it was minted for. It has two
answer shapes, one per delivery channel:

| Shape | Answer | Consumer | When it asks |
|---|---|---|---|
| default | plain text: one ticketed URL | `ao-harness open` / `info` / `attach` / `up` | any time it prints or opens a URL for a human |
| default | plain text: one ticketed URL | e2e `HarnessApp.open()` | every navigation, since each test gets a fresh cookie jar |
| `?host=webview` | JSON `{url, ticket}` | Windows/WSL launcher | the reload keybinding re-navigates WebView2 |

The webview shape keeps the halves apart so the URL the launcher navigates to
stays bare and the ticket goes to `ExecJS`. `Config.DecoratePageURL` supplies
the renderer for both, so `main.go` keeps the single rule for what a shell adds
to a page URL (`?cid=`, the harness marker) and this package does not restate
it. Two binaries call the route without linking this package
(`internal/wsllauncher`, `internal/harnessclient`); each restates the path
behind a drift-guard test rather than pulling the server into a launcher.

### `/healthz`: the one route that asks for nothing

`HealthPath` answers `{version, backendId}` to any caller. Every other route
on this listener spends a credential; this one cannot, because both consumers
run at the moment there is no valid credential to spend. The SPA's pre-WS
compatibility check runs before a socket exists, and the update watchdog is
asking whether the backend it was talking to is still the same build — and a
credentialled health route answers 404 to a *restarted* backend, which is
indistinguishable from down and is precisely the state the probe exists to
observe. Neither field authorizes anything, and both are already in the
manifest the bundle serves.

Two things it still does. It sends no `Access-Control-Allow-Origin`, so a
foreign page may issue the request and can never read the reply, and it goes
through the same `loopbackHostGuard` the credentialled routes use. Readiness
is not folded in: `/bootstrap.json`'s 503 stays the "booting" answer, because a
probe that reports booting as unreachable would defeat both consumers.

### `--connect` carries the socket rather than handing over the credential

The stub (`internal/clientmode`) holds the upstream credential server-side and
gives its page the same thing a local boot gives it: a ticket on the URL, then a
cookie for the stub's own origin. Its `/ws` is a `httputil.ReverseProxy` that
checks origin and credential locally, deletes the local `Cookie` and `Origin`
headers, and attaches the upstream's own credential for the hop.

Which credential that is depends on how the stub was started, and it is exactly
one of two:

| Started with | Holds | Presents on the hop |
|---|---|---|
| `--connect ws://host:port/ws?token=…` | the upstream backend's launch token | `Authorization: Bearer <token>`, plus the backend's own local-channel session forwarded through `internal/relaysession` |
| `--connect <pairing link>` or a paired backend name | a rotating device session and a device key (`internal/deviceclient`) | a single-use `?ticket=` minted per upgrade, over TLS pinned to the certificate the pairing payload named |

The first is a SAME-HOST attach: a launch credential alone cannot admit an
off-host upgrade, so a stub across a network has to be the second. The second
carries nothing on the header arm — only a spent ticket both names a session and
stands in for the launch credential, and a paired device has no launch
credential to present — while the stub's `/bootstrap.json` probe, which is an
ordinary HTTP request rather than an upgrade, carries `X-AO-Session` plus a proof
minted for that request.

The alternative — point the page straight at the upstream — cannot work under a
cookie model, because the stub cannot set a cookie for another origin; the page
would need a credential it can read, which is the thing the design removes. The
carry also keeps SPA code identical across embedded, `--connect`, and remote
browser boots: one origin, one cookie, and `validateWsUrl` with no exemptions.

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
- Receiver-level `LocalOnly` is coarse by design, and it is the ONLY locality
  gate left on the dispatcher — there is no per-method origin partition to
  extend. A receiver that needs per-method authorization joins the generated
  scope table (a `receiverSpecs` entry plus an `//ao:scope` on every method) so
  the per-call gate reads it, rather than re-checking origin in method bodies.

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
1000 or 1001 is a client navigation. `session revoked` is its own reason: a
revocation tears the socket down by cancelling its context, which at the error
alone reads identically to a server shutdown.

The duration in that line is the same quantity the client's reconnect ladder
judges itself on. `wsClient` resets its backoff only after a connection survived
`BACKOFF_RESET_AFTER_MS`, so a relay that tears down long-lived sessions keeps
reconnecting fast, while an accept-then-close backend backs off instead of
storming.

## The device-facing credential routes

Three POSTs (`authroutes.go`) are the only routes a client reaches without the
launch credential, because they are how a client that has never met this backend
gets one: `/auth/pair` redeems a pairing link, `/auth/token` rotates a credential
pair, `/auth/ticket` mints the single-use ticket the `/ws` upgrade spends.

```
device                                   backend
  │  POST /auth/pair {token, keyThumbprint}   │
  │──────────────────────────────────────────▶│  spends the link, enrolls the key,
  │                                           │  mints a session with activated_at unset
  │◀── {credential, refreshSecret,            │
  │     awaitingConfirmation, verification} ──│
  │                                           │
  │  (owner matches the six digits on the     │
  │   minting surface → ConfirmPairing)       │
  │                                           │
  │  POST /auth/ticket   (session credential) │
  │──────────────────────────────────────────▶│
  │◀── {ticket, expiresAtMs} ─────────────────│
  │  GET /ws?ticket=…                         │
  │──────────────────────────────────────────▶│  spend, re-check liveness, upgrade
```

The transport owns the wire and nothing else: `AuthEndpoints` is a two-method
interface it declares and the App satisfies over `internal/identity`, and the
DTOs are dumb. A refusal is `401` with `{"reason": "<code>"}` — the typed code
from `internal/identity`'s closed set, which the client's presentation module
turns into a sentence. The device key rides `X-AO-Device-Key` and is never read
from the body: a proof a caller may write into the document it is proving
something about is not a proof.

A session credential itself reaches a request two ways and is read one way
(`SessionCredential`): the `X-AO-Session` header for a client that can set one,
and the HttpOnly `ao_session_<port>` cookie the bootstrap exchange plants for the
browser, which cannot set headers on a WebSocket handshake. The header wins when
both are present — a relay forwarding a credential deliberately outranks an
ambient cookie.

## Per-peer request budgets

Four budgets carry a token bucket per peer: `/bootstrap.json`, `/pageurl`,
`/rpc`, and the three `/auth/*` routes together. `/healthz` and the SPA assets carry none, and `/ws` carries none because
one upgrade opens a long-lived connection whose credential came from the ticket
exchange that preceded it.

The budget bounds work, not guessing. A 256-bit launch token is not reachable by
any request rate; what a rate reaches is the backend's own cost per request, and
on `/pageurl` the eviction of tickets other pages are about to present.

The `/auth/*` routes share ONE table on purpose: they are alternative ways for
the same peer to ask this backend for a credential, so a peer that has spent its
budget on one must not simply move to the next.

The refusal is `429` with `Retry-After` rather than the credential channel's
`404`, and that difference is load-bearing rather than cosmetic: the SPA treats a
401/403/404 on the manifest as terminal and stops reconnecting, so a 404 here
would convert a burst into a permanent logout. A rate-limit refusal is transient
and has to look like one.

Loopback peers are limited on the same terms as anyone else, so the path is
exercised continuously in development and in the e2e suite rather than running
for the first time on a LAN bind. The peer table is bounded and self-cleaning:
an entry whose budget has refilled is indistinguishable from a peer never seen,
so inserts drop idle peers instead of a sweep goroutine doing it on a timer.

## Revoking a live connection

A revoked session row stops the next call. It does nothing to a socket that is
already streaming, which will keep receiving events until something closes it.
`SessionConns` (`internal/transport/sessionconns.go`) is that something: a map
from session id to the connections carrying it, and one `CloseSession` that
tears all of them down synchronously.

Two seams keep the direction of dependency clean. `Config.SessionForRequest`
resolves a request's session before the upgrade and may refuse it (with the same
`404` a bad credential gets), and `SessionConns` satisfies the one-method
interface `internal/identity` declares for itself. Neither package names a type
from the other, so transport stays store-free.

The teardown itself is three ordered steps — close the event subscriber, cancel
the connection context, then `CloseNow` the socket — because only the first of
those makes "delivery has stopped" true at the moment `CloseSession` returns
rather than whenever the parked reader notices. Deregistration rides
`ConnState.RunCleanups`, the same pass that releases every other per-connection
resource.

The registry keeps no record of a revoked session, so a later connection on that
id attaches like any other. Refusing it is the database row's job, checked per
call rather than latched at upgrade time.

Revocation reaches open sockets synchronously, but two other ways a session stops
reach nothing at all: it EXPIRES, or something outside this process revokes it.
So a connection that names a session also re-validates on an interval
(`Config.SessionRecheckInterval`, 60s) and caps its own lifetime
(`Config.MaxRemoteConnLifetime`, 12h) to force a periodic re-ticket.

**Loopback connections are exempt from the lifetime cap.** The cap exists so a
credential that travels a network is re-presented periodically; the local page's
session is re-minted at boot and travels none, so capping it would cost the
webview a visible reconnect and buy nothing. The re-check still applies — a local
session can expire like any other. `resolveWatchWindows` holds every default and
exemption in one function so none of it is invisible at the call site.

All three server-side teardowns cancel the connection context, which at the
terminal error alone is indistinguishable from a shutdown. `connHandler.closeCause`
is what makes the close log say which one ran: `session revoked`, `session no
longer live`, or `connection lifetime reached`.

## References

- `internal/transport/AGENTS.md` for the authz, replay, and classification rules.
- `docs/architecture/data-flow.md` for how triage events reach the bus.
- `docs/architecture/root-decomposition.md` § Wire compatibility for why method
  IDs hash under `main.App` regardless of where the code lives.
- `frontend/src/lib/transport/` for the client half.
