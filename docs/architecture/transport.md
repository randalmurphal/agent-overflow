# Transport

`internal/transport` provides the HTTP and WebSocket protocol shared by the
embedded webview, `agent-overflow --connect`, remote browsers, and attached
backends. This document describes the protocol mechanisms. Package editing rules
live in [internal/transport/AGENTS.md](../../internal/transport/AGENTS.md).

## Listener lifecycle

### Stable ports

The webview origin includes the port, so changing an ephemeral port discards
origin-scoped browser storage and changes the page-cookie name. Port stability
is persistence, not access control.

`main_transport_port.go` owns the per-install pin in
`transport-port.json`; transport receives the selected port in `Config.Port`.
Resolution order is:

1. an explicit non-zero `--listen` port;
2. `network.listenPort` from host settings;
3. the saved transport pin;
4. an ephemeral port, which is adopted after a successful bind.

An explicit CLI port does not read or write the pin. A configured settings port
fails loudly if unavailable because published endpoints already name it. The
ordinary saved pin may use `Config.EphemeralPortFallback` for port-specific
bind failures and then adopt the replacement. Other bind errors remain fatal.
The isolated harness opts out of persisted network settings.

`Rebind` keeps the current port for a host-only change and does not move to an
ephemeral port. It creates the replacement listener before closing the current
one. Its close-and-retry path applies only to address-in-use errors; other bind
failures leave the working listener intact.

Every package-created listener passes through `bindListener`. Explicit IPv4
addresses use `tcp4`, including `0.0.0.0`, so WSL's Windows relay receives an
IPv4 listener. The Windows launcher may perform one explicit
`--reset-transport-port` retry when the WSL backend bound successfully but
cannot be reached from Windows.

### TLS on the same address

A configured listener accepts TLS and cleartext HTTP on one address.
`tlssniff.go` classifies the first byte within the HTTP header-read deadline
and hands TLS connections to `tls.Server`. Classification runs outside the
accept loop so an idle peer cannot block other accepts. Temporary accept errors
are passed through to `http.Server` without becoming a permanent listener
failure.

`CertificateSource` selects a certificate for each handshake. The canonical
domain receives the configured domain certificate. Other names, including
address-based and SNI-less clients, receive the self-signed certificate that
paired native clients pin. Both certificate slots may be swapped without a
rebind.

Auxiliary listeners are injected rather than created here. They do not gain the
same-port TLS wrapper and must preserve their explicitly limited route and peer
policy.

## Credentials and request admission

The launch credential identifies one backend process. It has three carriers:

| Carrier | Use |
|---|---|
| `ao_page_<port>` HttpOnly cookie | browser HTTP requests after bootstrap |
| `Authorization: Bearer` | native and same-host process clients |
| `?token=` | browser/Node WebSocket APIs that cannot set handshake headers |

`Credential.Authenticate` applies one constant-time check to all carriers.
Routes either call it directly or use the ticket exchange built on it.

### Page bootstrap

Browser URLs carry a single-use page ticket, not the launch credential. The SPA
presents `?t=` to `/bootstrap.json`, which consumes the ticket and sets the
HttpOnly, SameSite=Strict page cookie. The SPA then removes the ticket from the
visible URL.

Native webviews load a bare URL marked `?host=webview`. Their owning process
mints a ticket after `WindowRuntimeReady` and injects it through
`uiwindow.DeliverPageTicket`. A reload receives a new ticket. `/pageurl`
provides fresh URLs or separate URL/ticket values to authenticated native
clients that need another navigation.

Page tickets and session-bound WebSocket tickets use the shared single-use
ticket implementation. A page ticket has no subject or deadline. A WebSocket
ticket names a durable session and has a short lifetime.

### Browser origin and peer locality

`OriginAllowed` runs before browser credential admission on the WebSocket,
bootstrap, and page-URL routes. Cookies are host-scoped rather than port-scoped,
so another listener on the same host could otherwise cause a credentialed
WebSocket request. The expected scheme comes from the request's TLS state.
Headerless native requests proceed to their separate credential and peer checks.

The launch credential alone may open a WebSocket only from a kernel-reported
loopback peer. An off-host upgrade must name a live durable session through its
session cookie, header, or single-use ticket. The session's binding class must
admit the actual peer, and key-bound devices must present a request-bound device
proof. A ticket names a session but does not replace the liveness, binding, or
proof checks.

Authentication failures on browser-facing routes use the same non-disclosing
404 shape as an unavailable route. Structured authentication routes return
their documented refusal codes and hints.

### Health and activation

`/healthz` returns version and backend identity without credentials so clients
can distinguish restart from unavailability before they possess a valid
credential. It provides no CORS read permission and still uses the host guard.

During supervised activation trials, `Config.WaitForActivation` gates every
other route, including credential rotation, tickets, transfers, bundles, and
WebSocket upgrades. A disconnected request stops waiting through its context.
Activation failure is HTTP 503, distinct from credential refusal.

## HTTP RPC and additional receivers

`POST /rpc` is the bounded one-shot RPC surface for the `ao` CLI. It accepts a
method name rather than a numeric ID and returns dispatcher outcomes in the
ordinary `ServerFrame` envelope. HTTP status represents transport failures
such as an invalid HTTP verb, unreadable body, or missing authentication.

Each scoped token contains a fixed allow-list and expiry. The dispatcher still
applies the registered method scope, route, and argument-dependent checks. A
token does not widen a method's authority.

Additional receivers use stable package/type labels because those labels feed
method IDs. Receiver registration exposes all generated methods for that
receiver, so production registration is an authorization decision. Local
harness receivers stay local-only.

## Method metadata and routing

`methodgen` scans bound methods and emits the Go `MethodMeta` table and the
frontend route table. Method IDs are FNV-1a 32-bit hashes of
`<package>.<typeName>.<methodName>`.

Every exported bound method has:

- a `//ao:scope <name>` annotation;
- a route, either inferred from a supported first parameter or declared with
  `//ao:route home|selected|all`; and
- step-up metadata when fresh owner confirmation is required.

Scope determines whether the caller may invoke a method. Route tells a
multi-backend client which connection should carry it. The server receiving the
frame does not forward it to another backend.

`thread` and `project` routes are inferred from first parameters named
`threadID` and `projectID`. `workspace` is inferred from a first
`gitapp.WorkspaceRef`. An explicit route overrides inference for methods whose
first ID is not their execution location. Run `make methodgen` after changing
methods, vocabulary, receiver specifications, or generator inputs.

## Event replay and filtering

Each registered channel has one `ChannelPolicy` describing scope, retention,
entity filtering, and background behavior. The event ring is an in-memory,
bounded reconnect buffer. SQLite remains the authoritative history.

The hello frame carries:

- backend and launch identity;
- capability flags;
- the visible channel heads as `replayBaseline`; and
- connection-specific authorization information.

A new launch invalidates cursors from an earlier sequence space. On reconnect,
the client asks for replay from its saved per-channel cursors. Live and replay
frames may interleave, so clients reconcile by sequence and wait for the replay
completion marker rather than relying on arrival order.

### Gap markers

`gap:true` instructs a client to reload authoritative state. It is returned
when a cursor is below retained history, above the current head, or names a
sequence space for a channel with no ring. The marker's `seq` may be lower than
the client's cursor and must be encoded even at zero.

Ephemeral channels do not replay retained frames. Latest-only channels return
the current value rather than reporting an eviction gap. Both still report an
above-head cursor because it belongs to another sequence space.

When a subscriber buffer is full, the server records the affected channel. The
next deliverable event for that channel carries `gap:true`; other affected
channels receive standalone gap markers before later delivery. Latest-only
channels do not need a marker because their next frame supersedes the loss.
Client-side forward-sequence detection remains a second loss signal within one
connection.

### Watched entities and paused clients

A `watch` frame replaces the connection's complete watched-thread set.
Connections that never send one receive all events. On reconnect the client
re-sends its set before asking for replay. Entity-filtered channels withhold
events outside the set; other channels continue to support navigation,
notifications, and summary state. Empty or unrecognized entity attribution
fails open to delivery.

Withheld frames are not transport loss and do not produce gap markers. The
frontend therefore disables inferred forward-gap handling only for registered
entity-filtered channels after it has sent a watch set.

A `lease` frame reports whether the platform has paused the client. It is not
page visibility, focus, or pane selection. New connections start active.
Background policy may withhold highlight seeds and merge provider item deltas.
Returning active flushes pending deltas in channel sequence order before later
pass-through frames.

## Framing, coalescing, and keepalive

All messages are JSON text frames; binary frames are rejected. RPC responses,
single events, event batches, replay completion, gap markers, watch state,
lease state, screen presence, and keepalive have distinct frame shapes.
Unknown additive fields remain compatible.

Every connection coalesces events over a small bounded window to reduce webview
message overhead. A single event keeps its ordinary frame shape; multiple
events use `type:"batch"`. Non-loopback connections may also negotiate
permessage-deflate. Coalescing preserves channel sequence order and respects
the configured event-count bound.

The server sends application-visible ping frames on the heartbeat cadence and
periodically verifies a protocol pong while the reader is parked. Every write
has a deadline. Close logs record peer, duration, and a specific server-side
cause when known. The frontend's stale-socket threshold is defined relative to
the heartbeat period and does not judge browser-hidden intervals where timers
and delivery may be throttled.

`transport.heartbeat.v1` in hello arms that watchdog before the first ping
and permits a client `ping` frame requesting an immediate heartbeat. Resume
uses this probe to verify an open socket; repeated wake signals do not extend
its deadline. Older servers arm the watchdog by sending their first heartbeat
and receive no probes. The connection banner distinguishes resume verification
from replay and snapshot recovery.

## Session lifetime and revocation

`SessionConns` maps durable session IDs to current WebSockets. It exists so a
revocation can stop event delivery immediately rather than waiting for the next
RPC. Empty session IDs are never registered.

Admission checks the session before upgrade. After registry attachment, the
server checks it again to close the race with revocation between those steps.
Session-bearing connections then recheck liveness periodically and enforce the
configured maximum connection lifetime. Loopback page connections retain their
documented exemption from the network-session lifetime cap.

Revocation teardown:

1. closes the event subscriber;
2. cancels connection work; and
3. closes the WebSocket to wake the reader.

The steps are idempotent and run without holding the registry lock. Ordinary
connection teardown uses the same cleanup stack. Transport stores no revocation
tombstone; the durable identity rows remain authoritative for later admission.

The per-RPC scope hook also consults current session liveness, so an established
socket cannot continue invoking methods from authorization captured at upgrade
time.

## Client attribution

The upgrade URL carries a durable browser-profile device ID and a per-page-load
connection ID. Bound methods read them with
`transport.ClientFromContext(ctx)`. The generated TypeScript signature omits
the leading Go context parameter.

The device ID supports attribution. The connection ID supports echo
suppression. Tabs in one browser profile share a device ID and must still
receive one another's writes. Both identifiers may be empty for background,
in-process, and test calls.

## Device-facing credential routes

Invitation minting, redemption, confirmation, refresh, ticket minting,
passkeys, recovery, and own-device enrollment use dedicated bounded HTTP
routes. They share transport authentication and refusal encoding while
`internal/identity` owns their durable state and cryptographic decisions.

Per-peer budgets are applied before expensive parsing or cryptography and are
bounded in memory. The accepted socket is the default source of peer identity.
Trusted-proxy handling must be explicit and limited to the configured proxy
boundary.

Recoverable refresh is specified in
[session-renewal.md](session-renewal.md). Remote access roles, session binding,
pairing, and step-up behavior are specified in
[remote-access.md](../specs/remote-access.md).

## Validation map

The highest-value transport tests cover:

- stable-port precedence, fallback, rebind rollback, and IPv4 socket family;
- same-port TLS classification, SNI selection, certificate swaps, and temporary
  accept errors;
- every credential carrier across loopback and off-host peers;
- origin, binding, device-proof, activation, and rate-limit boundaries;
- generated method metadata and frontend registry parity;
- replay ordering, both directions of cursor gaps, subscriber overflow,
  entity filtering, and background coalescing;
- blocked writers, keepalive timeout, session revocation during upgrade, and
  cleanup races; and
- mixed-version hello, refusal, refresh, and additive-field compatibility.
