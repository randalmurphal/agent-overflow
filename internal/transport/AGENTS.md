# transport/

This package implements the HTTP and WebSocket protocol shared by the embedded
webview, `agent-overflow --connect`, remote browsers, the CLI RPC surface, and
attached backends. See
[transport.md](../../docs/architecture/transport.md) for protocol mechanics and
[remote-access.md](../../docs/specs/remote-access.md) for the trust model.

## Package boundary

Transport owns listeners, HTTP routes, WebSocket framing, credential carriers,
request dispatch, authorization metadata, event replay, and live-connection
teardown. Callers inject ports, certificates, backend identity, session checks,
and receivers. Do not import `internal/identity` or `internal/store`; use the
narrow hooks in `Config`.

`Server.Start` returns after binding. Asynchronous serving failures arrive on
`Server.ServeErr()`. Every listener created by this package must go through
`bindListener`, including startup, fallback, rebind, and rollback. Auxiliary
listeners are already-owned transport endpoints and retain their documented
local-only restrictions.

TLS and cleartext HTTP share one bound address. Certificate selection is by
SNI: the configured canonical domain receives the domain certificate and every
other name receives the pinned self-signed certificate. Certificate swaps must
take effect on the next handshake without rebinding or dropping connections.
Explicit IPv4 addresses, including `0.0.0.0`, must bind as IPv4 for WSL relay
compatibility.

## Routes and authorization

Every route on the mux belongs in `internal/surfaces`. Preserve the route's
locality, authentication, origin, scope, request-size, and rate-limit policy.
`/healthz` is the intentional unauthenticated exception. Activation gating
covers the rest of the handler, including credentials, transfers, bundles, and
WebSocket upgrades.

Ordinary SPA, authentication, attachment, bundle, and RPC responses use
`WriteSecurityHeaders` with the server-selected CSP. Preview proxy responses
are the explicit exception because their bytes and policy belong to the
development server.

Browser-facing credential failures use the documented non-disclosing HTTP
shape. RPC authorization failures use the wire error envelope. Do not expose
internal errors, paths, or panic text; log full details with a correlation ID.
Classify missing durable rows as `not_found`, distinct from
`method_not_found`.

Origin validation is part of browser authentication because cookies are not
port-scoped. Derive the scheme from the TLS state, not forwarding headers.
Requests without `Origin` remain eligible for the separate credential and peer
checks used by native clients.

The launch credential identifies one backend process. It is acceptable without
a durable session only for a kernel-reported loopback peer. Every off-host
WebSocket upgrade must name a live session whose binding admits that peer.
Apply the same session liveness, binding, and device-proof checks to every
credential carrier, including a redeemed single-use ticket.

Per-peer budgets on invitation, redemption, token, and ticket routes are part
of the protocol. Derive peer identity from the accepted socket unless a
specifically trusted proxy boundary has already established another source.

## RPC metadata and generation

Every exported bound method is a wire RPC. It requires `//ao:scope <name>` and
a route. `thread`, `project`, and `workspace` routes are inferred only from the
supported first-parameter shapes; all other methods declare
`//ao:route home|selected|all`. Scope answers whether a caller may act. Route
answers which attached backend receives the call.

Fresh-confirmation operations also declare the supported step-up metadata and
must pass the runtime step-up check. Method IDs remain FNV-1a 32-bit hashes of
`<package>.<typeName>.<methodName>` for Wails compatibility.

Run `make methodgen` after changing bound methods, scope or route vocabulary, or
generator inputs. Commit both generated outputs. If the generator learns a new
input, add it to its input manifest so cached tests remain valid.

Additional receivers widen the production RPC surface. Register only the
intended receiver and preserve stable package/type labels when moving its Go
implementation. Harness-only receivers remain local-only.

## Events and connection state

Register every event channel with one `ChannelPolicy`. Scope, audience,
retention, entity filtering, and background behavior are policy, not emitter
choices. An unregistered channel is restricted to local clients and logged once
when its ring is created. When widening a channel's audience, recheck the scope
of every producer because receive exposure can turn a remotely callable
producer into a remote steering path. Keep the backend registry and frontend
channel mirrors aligned through their cross-language tests.

The per-channel ring is a bounded reconnect buffer. It is not durable history.
A cursor outside the current sequence space receives `gap:true`; clients then
reload authoritative state. Include `seq` even when it is zero. A full
subscriber buffer records the affected channel and announces the loss on the
next deliverable frame. Latest-only channels instead let the next value
supersede the dropped one.

Live frames and replay may interleave. Preserve per-channel ordering and the
replay completion marker. Entity filters run before drop accounting: an event a
client chose not to watch is not a transport gap. Unknown or empty entity keys
fail open to delivery.

`watch` frames replace the connection's complete watched-thread set and must be
re-sent before replay after reconnect. `lease` describes a platform-paused
client, not page focus or visibility. New connections start active. Flush
coalesced item deltas in sequence order before pass-through frames or return to
active state.

All connections coalesce event frames to bound webview message overhead.
Non-loopback connections may additionally use compression. Keep memory costs
bounded per connection and preserve write deadlines so a peer that stops
reading cannot stall delivery.

## Session teardown and client identity

Recheck durable session liveness on every RPC. Session-bearing connections also
recheck periodically and observe their lifetime cap. After attaching a new
connection to `SessionConns`, check liveness again to close the race with a
concurrent revocation.

Revocation teardown stops event delivery, cancels connection work, and closes
the socket. Call close callbacks outside the registry lock and use the shared
connection cleanup stack. Do not store revocation tombstones in transport;
durable identity remains authoritative. Never register an empty session ID.

`transport.ClientFromContext` distinguishes the durable browser-profile device
ID from the per-page-load connection ID. Use the connection ID for echo
suppression. Two tabs share a device ID and must still receive one another's
writes. Empty identity is valid for in-process and background calls.

## Wire compatibility and validation

All WebSocket messages are JSON text frames. Unknown additive fields remain
compatible; malformed frames fail visibly. Preserve hello capability flags,
backend and launch identity, replay baselines, and refusal codes across mixed
client/backend versions.

Test policy boundaries through the real HTTP or WebSocket seam where practical:
loopback versus off-host peers, every credential carrier, origin variants,
revocation during upgrade, cursor gaps on both sides of the ring, subscriber
overflow, reconnect ordering, rebind rollback, and temporary accept failures.
