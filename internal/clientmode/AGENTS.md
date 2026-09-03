# internal/clientmode/

`agent-overflow --connect` remote-client mode. The desktop binary skips
booting a local transport and points its Wails webview at a remote-hosted
backend, reached through this stub. What the operator typed decides which
credential the stub carries — a `ws://…?token=` endpoint on the same
machine, or a pairing link / paired backend across a network, resolved
before `Serve` by `main_connect.go`.

## Layout

- `clientmode.go` is the public surface: `Config`, `PairedUpstream`,
  `ParseConnectURL`, `Serve`, `Server`. The stub HTTP server, the
  credential exchange, the page-ticket mint, the manifest and the URL
  parser live here.
- The CARRY does not. `internal/backendproxy` owns the two proxies and
  the manifest fetch, because the desktop's attached backends
  (`internal/attachedbackends`) make the identical hop once per machine
  and a duplicate of that code gets the credential half wrong. This
  package builds one `backendproxy.Carrier` in `Serve` and calls
  `CarryUpgrade`, `CarryTransfer` and `FetchBootstrap`; `PairedUpstream`
  is an alias of the seam declared there. Admission stays HERE — the
  origin rule and this stub's own cookie — because who may reach the hop
  is the caller's question, not the carrier's.
- `clientmode_test.go`: URL-parsing tests, shell-serving and manifest
  tests, the credential and origin rules on `/ws` and `/attachments/`,
  the drift guard between the relay's prefix and the backend's two route
  patterns, the forwarded session
  credential and its refresh-on-refusal, the paired upstream's carried
  probe and per-upgrade ticket, the upstream revalidation verdicts,
  static-asset serving, idempotent shutdown.

## Responsibility boundary

- What BELONGS here:
  - Validating the operator-supplied `--connect` URL.
  - Booting a tiny loopback HTTP server that serves the embedded SPA
    bundle, answers `/bootstrap.json`, carries `/ws` to the upstream, and
    relays attachment bodies under `/attachments/`.
  - Holding the upstream credential, which never leaves this process —
    either the launch token or the paired device session behind
    `Config.Paired`.
- What does NOT belong here:
  - The WebSocket client itself. That's `frontend/src/lib/transport/
    wsClient.ts`. This package only carries the socket it opens.
  - Anything about how a device credential is obtained, held or renewed:
    keys, pairing, refresh rotation, and certificate pinning are
    `internal/deviceclient`'s. This package declares the three-method
    `PairedUpstream` it needs and knows nothing behind it.
  - Deciding what to verify the remote server's TLS certificate against.
    In token mode there is nothing to decide — the OS trust store answers
    it, as `wss://` always did. In paired mode the decision was made when
    the device paired, and it arrives as a ready `http.RoundTripper` this
    package dials through for both the probe and the proxy.
  - Any RPC dispatch, event bus, replay ring, or upgrade of its own.
    `internal/transport` owns those, and `clientmode` deliberately skips
    that whole stack so connecting clients don't ship a second backend.

## The upstream credential stays server-side

The page never receives the upstream credential, in any form a script
could read. The flow mirrors a local boot exactly:

1. `Serve` builds a `transport.Credential` for THIS stub's own page — the
   same type, and therefore the same `Authenticate`, the transport server
   uses. It is unrelated to whatever authenticates the upstream hop.
2. `AppURL` stamps `?host=webview&mode=client&cid=…` and NO credential:
   this process owns the window, so `main_desktop.go` hands the page its
   ticket by `uiwindow.DeliverPageTicket(window, stub.MintPageTicket)`
   rather than writing one into a URL that is copyable and lands in logs
   (`internal/pagehost`). `AppURL` therefore mints nothing and is stable
   across calls; `MintPageTicket` is the mint, once per document.
3. `handleBootstrap` calls `Credential.Exchange`, which validates the
   ticket and sets this stub's own HttpOnly cookie for this origin. Every
   later request rides the cookie.
4. `handleWS` checks `transport.OriginAllowed` and `Credential.Authenticate`,
   then hands the request to `backendproxy.Carrier.CarryUpgrade`, which
   deletes the local `Cookie` and `Origin` headers and attaches the
   upstream's own credential for the hop: `Authorization: Bearer
   <upstream token>` in token mode, and in paired mode a single-use
   `?ticket=` minted from THIS request (see below).

The hop REPLACES the query rather than forwarding it (the operator's
endpoint owns it, and the page's own marker and client id mean nothing
upstream), so anything the page must still be identified by has to be
re-emitted explicitly. `backendproxy`'s `upstreamQuery` does that for the two declared
client-identity parameters (`did`, `conn`), parsed and re-rendered
through `transport.ParseClientIdentity` so only bounded values cross and
an operator parameter of the same name wins. Without it the upstream
would see an anonymous connection, and since the `ui_state` scope became
connection-derived that means no bucket at all.

So the SPA's socket is same-origin (it dials this stub), and the
cross-origin hop happens in Go where no page script can observe it.

## The hop also names a session

The bearer token says which BACKEND this stub was configured for; it says
nothing about which connection is asking. Reaching the upstream over
loopback or the LAN, the stub would otherwise be trusted for its topology
alone — the same problem the WSL launcher has, in the same shape, which is
why the mechanism lives in `internal/relaysession` and not twice.

`backendproxy.New` builds a `relaysession.Source` against the upstream's
`/bootstrap.json` (`relaysession.BootstrapURL`, which is also what derives
the URL `FetchBootstrap` probes — one derivation, so the endpoint this
stub dials and the endpoint it asks for a credential can never name
different backends). The proxy's `Rewrite` then deletes any
inbound `X-AO-Session` and sets the one this process fetched: a browser
cannot put a header on an upgrade, but a local non-browser client holding
this stub's cookie could, and a forwarded one would let it name a session
it never obtained.

Failure degrades on a SAME-HOST upstream and blocks on an off-host one,
and the difference is the upstream's rule rather than this package's: a
`/ws` upgrade from a non-loopback peer must name a session
(`internal/transport/AGENTS.md`). An upstream with no session core to
speak of leaves the header off, and the upgrade then carries the bearer
token alone — which the upstream admits when this stub is on its machine
(the SSH-tunnel and same-host cases) and refuses when it is not.
`ModifyResponse` marks the credential stale on any non-101 answer — a
refused upgrade is the one signal it has gone dead — and passes the
response through untouched: the verdict on whether the TOKEN is still
honoured belongs to the `/bootstrap.json` probe below, which is the one
place that maps upstream status onto the SPA's terminal state.

## A paired stub carries its own session, not the backend's

`Config.Paired` is the CROSS-HOST mode, and it is an alternative to
`Config.Token` rather than an addition — `Serve` refuses both and
refuses neither. `agent-overflow --connect <pairing link>` runs the
ceremony in the terminal (`main_connect.go`) and hands `Serve` the
resulting `*deviceclient.Client`; a stub started that way holds no launch
credential at all and never obtains one, which is the shape spec §4
requires off-host: a session that is this DEVICE's, revocable and scoped,
rather than the backend's own.

Three things are carried, and each is minted per request because each is
single-use by construction:

- The manifest probe presents the session credential plus a proof signed
  over THAT request (`PairedUpstream.Authorize`). A proof binds the
  method and the path, so one cannot be prepared at `Serve` and reused.
- The carried upgrade presents a single-use `?ticket=` minted from the
  request `handleWS` is holding (`PairedUpstream.Ticket`), and
  deliberately **not** the session header. Only a spent ticket both names
  a session and stands in for the launch credential on `/ws`
  (`internal/transport/AGENTS.md`), which is the whole of how a device
  with no launch credential is admitted; sending the header alongside it
  would put a credential on the wire nothing reads and cost an ECDSA
  signature per handshake. `upstreamQuery` lets the ticket win a
  collision outright for the same reason — it is this handshake's
  credential, not configuration the operator's URL owns.
- Both go through the pinned `RoundTripper`, so the certificate the probe
  agreed with is the certificate the socket rides, and a certificate that
  changed under this process fails both at once rather than leaving a
  probe that says the backend is fine and a socket that cannot reach it.

`relaysession` is not built in this mode and `ModifyResponse` marks
nothing: a ticket is spent by definition, and the session behind it
renews on its own schedule rather than on a refusal. A mint or authorize
failure answers 503, never the credential channel's 404 — the SPA's
terminal state is reserved for a verdict the BACKEND gave, and the run
that started this stub already told the person, in the terminal, what a
dead pairing needs.

**Carrying `/ws` rather than pointing the browser at the upstream is the
design choice this package is built around.** The alternative — hand the
page the upstream's `wsUrl` — cannot work under a cookie model: this stub
cannot set a cookie for another origin, so the page would need a
credential it could read, which is precisely what the boot flow removed.
Carrying it also means the SPA code is byte-identical across embedded,
`--connect`, and remote-browser boots: one origin, one cookie, one
`validateWsUrl` rule with no exemptions. The cost is one extra process
hop per frame on the `--connect` path, and the proxy is written to keep
that hop transparent: the handshake key, subprotocols, and the
compression extension all pass through, so the browser and the upstream
negotiate with each other and this process only splices bytes.

## Anti-patterns

- Do NOT reintroduce a `window.__AO_BOOTSTRAP__` injection, a
  `sessionStorage` stash, or any other route by which page script can
  read a credential. The shell is served verbatim; a first-load
  `/bootstrap.json` fetch is how the page learns its manifest, and the
  round trip it costs is what buys the HttpOnly cookie. The page ticket
  the host injects is not an exception: it is single-use, it is spent by
  that same fetch, and what it buys is the cookie no script can read.
- Do NOT serve the upstream credential to a local GET, in either mode.
  Anything on this listener that would hand out the token, the device
  session, a proof or a socket ticket is a way for any process on the
  host to become a client; the ticket exchange exists so it doesn't need
  to be.
- Do NOT add RPC dispatch, an event bus, or a real upgrade here. `/ws` is
  a byte carrier with no protocol knowledge; the whole point of
  `--connect` is to skip the local transport.
- Do NOT attach an upstream credential to the `/attachments/` relay. Those
  routes accept none: the single-use ticket already on the query is the
  whole admission, and it was minted by an RPC the upstream's scope gate
  authorized. What IS checked here is this stub's OWN page credential,
  and dropping that check would turn a LAN-capable listener into an open
  relay to the backend's transfer routes.
- Do NOT rewrite the URL on that relay. The path names the attachment and
  the query carries the ticket; both were minted by the upstream, for the
  upstream, and touching either is only a way to get them wrong.
- Do NOT bind to anything other than 127.0.0.1. The loopback server
  is the boot shim for the local Wails webview; nothing else should
  reach it.

## `/bootstrap.json` is also the credential-revalidation probe

The SPA refetches the manifest mid-outage to learn whether its session is
still honoured. The stub answers by asking the upstream from Go
(CORS-free) with whichever upstream credential it holds, then maps the
verdict onto the shapes
the SPA already knows (transport/AGENTS.md § "Credentials and refusal
shapes"): a refusal is 404, everything transient — network failure
included — is 503, so the reconnect ladder survives. This is what lets
the SPA's terminal `unauthorized` state cover `--connect` clients, whose
page cannot ask the upstream anything itself.

The body is the stub's OWN manifest, never the upstream's: `wsUrl` must
name THIS origin, since that is where the page's socket goes and the only
`wsUrl` `validateWsUrl` accepts. The manifest carries no credential. The
ticket exchange runs BEFORE the probe, so a page load during an upstream
outage still lands its cookie and its retry costs no ticket.

## References

- `frontend/src/lib/transport/bootstrap.ts`: `defaultBootstrap` fetches
  `/bootstrap.json`, forwarding its one-time ticket on first contact —
  here the injected one, read through
  `frontend/src/lib/transport/pageHost.ts`.
- `internal/transport/credential.go`: the credential type, the ticket
  exchange, and the origin rule this package reuses rather than restates.
- `internal/transport/server.go`: the in-process HTTP+WS server
  skipped on `--connect`.
- `internal/backendproxy`: the hop itself, shared with the desktop's
  attached backends.
- `internal/relaysession`: the forwarded session credential, shared with
  the WSL launcher. Its package doc carries the full contract.
- `internal/deviceclient`: the other side of `PairedUpstream` — the
  device key, the pairing ceremony, the rotating session it holds on
  disk, and the pinned dial. `main_connect.go` is what wires the two
  together for `agent-overflow --connect`.
