# transport/

HTTP+WebSocket wire between the Svelte frontend (the Wails-embedded webview,
`agent-overflow --connect`, or a remote browser) and the Go backend. Mechanism
walkthroughs live in
[docs/architecture/transport.md](../../docs/architecture/transport.md).

## What this package owns

The HTTP listener (embedded SPA, `/bootstrap.json`, `/healthz`, the `/ws`
upgrade, the two `/attachments/` byte routes, `POST /rpc` for the `ao` CLI,
`/browser-cdp` where a pane host exists, and the three attached-backend
subtrees where this installation has attached to other machines) plus any AUXILIARY listener a
caller hands it (§ Auxiliary listeners), the JSON wire frame, token
authentication, the per-connection authorization policy, per-peer request
budgets on the credential surfaces, reflection-based RPC dispatch, a
per-channel bounded ring for event replay on reconnect, and the live-session
registry that lets a revocation reach connections that are already open. Method
IDs are FNV-1a 32-bit of `<package>.<typeName>.<methodName>`, matching Wails'
`internal/hash.Fnv`, so the generated TypeScript bindings keep working. The ring
is in-memory only: a network jitter buffer, not a history store (root
`AGENTS.md` principle 3).

Not owned here: the receiver (the dispatcher takes an `any` and reflects), the
CERTIFICATES it terminates TLS with (`Config.Certificates` is injected by the
boot; `internal/servercert` mints the self-signed one and `internal/acmecert`
obtains the domain one, and nothing here mints, persists, renews or fingerprints
either), and where the listen port comes from (`Config.Port` is injected, never
read from a file here).

## Same-port TLS

With a certificate configured, this listener answers a TLS client and a cleartext
one on the ONE address it binds (`tlssniff.go`): the first byte classifies —
`0x16` is a TLS handshake record and no HTTP request line starts with it — and a
TLS connection is handed to `tls.Server` while everything else is the plain HTTP
this server always spoke. A second https port was the alternative and would have
made the pinned endpoint a different authority from the one the share URL, the
cookie names and the origin allow-list all derive from.

- **`bindListener` (server.go) is the ONE place a listener is created**, so boot,
  the ephemeral fallback, a rebind, its close-and-retry and its rollback all
  produce the same wrapped listener. A bind that skipped the wrap would silently
  drop TLS on exactly the paths a user reaches by toggling LAN access, and a
  pinning client reads that as the backend disappearing. An auxiliary listener
  is ACCEPTED rather than created, so it does not go through here and gets no
  sniff wrapper — argued in the next section.
- **Uniform on every bind.** Loopback and LAN behave identically; no mode decides
  when TLS is available.
- **Classification runs off the accept loop**, one goroutine per connection,
  bounded by `HTTPReadHeaderTimeout` — the same budget net/http would have given
  the first byte of that request. Inline, one peer that connected and said
  nothing would hold up every other client for that window, repeatedly, at no
  cost to itself.
- **Temporary accept errors are delivered once, never cached as terminal.**
  `http.Server` owns the retry and backoff. The sniff listener must keep
  accepting afterwards; otherwise existing WebSockets work while every new
  HTTP request hangs until restart. Close also releases pending error delivery.
  `TestTLSSniffRecoversAfterTemporaryAcceptError` covers a live connection
  surviving the failure alongside a fresh connection recovering.
- **Which certificate answers is decided per handshake, by SNI**
  (`certsource.go`). `CertificateSource` holds two swappable slots. A
  ClientHello naming the configured canonical domain gets the DOMAIN
  certificate — an ACME issuance or the user's own file pair, both installed by
  `internal/app`'s reconciler; every other handshake, SNI-less ones included,
  gets the SELF-SIGNED one. That default is the mechanism, not a fallback: a
  pinning client's SNI is whatever address it paired on, and answering it with
  a domain certificate reads as the backend having been replaced. Both slots
  swap live, so a renewal takes effect on the next handshake with no rebind and
  no dropped socket.
- **What the self-signed half buys is confidentiality, not authorization.**
  Nothing trusts it; its value is that the pairing payload carries its
  fingerprint, so a client that owns its own TLS configuration pins these exact
  bytes (spec §7, "Domainless TLS for Go-native clients"). A browser cannot pin,
  so `network.Settings.URL` stays `http://` and `Insecure` stays true for a LAN
  bind — **unless** a domain certificate is loaded, which is the one case a
  browser gets `https://` and no warning. Every credential check is the same on
  every half.
- `nil` `Config.Certificates` installs no wrapper at all, which is what every
  boot without a certificate and every test that does not ask for one runs on. A
  non-nil source holding no certificate YET still installs it and refuses
  handshakes until one arrives, because certificates arrive and renew while the
  process runs.

## Rebinding

`Rebind` closes the retired listener before returning, while its accepted
HTTP requests drain asynchronously and hijacked WebSockets continue. Darwin
allows a wildcard bind over loopback; leaving that old loopback listener
open until a background Shutdown makes an immediate toggle back fail with
address-in-use. `TestRebind_RapidLANTogglesReleaseRetiredListeners` covers
repeated toggles with an existing WebSocket.

## Auxiliary listeners

Bootstrap may carry bounded `routes` from the boot-injected `ComputerRoutes`
getter, only after ordinary manifest admission and once backend identity exists.
These are credential-free HTTPS candidates, not new authorities. The client
verifies their TLS trust and `/healthz` backend ID before sending any credential.
No listener or browser-origin policy changes merely because a route is advertised.

`auxlistener.go` serves a listener the CALLER acquired, with this server's
routes, credentials, Host and Origin rules, per-RPC scope gate and session
registry (spec §7, "Multi-listener, one session store"). `internal/tailnet` is
the caller it exists for: a netstack listener on the owner's tailnet, no second
API, no second credential class, no route that exists on one listener and not
the other. A revocation lands on both in the same instant because both consult
the one live-session registry.

- **The caller owns the listener.** This package accepts on it and stops when
  told to. It never binds one, never rebinds one, and never reopens one that
  failed; whatever produced it decides when it exists.
- **Per-listener isolation.** An accept loop that ends reports to the CALLER's
  sink, never `Server.ServeErr`, and never touches the main listener. The spec
  names the failure: one integration failing to start degrades that listener
  only, and putting it on the shared channel would make a tailnet that could
  not accept read as the app's own transport dying.
- **Its own `*http.Server`, not the active one.** `Rebind` retires the active
  server and shuts it down, and `http.Server.Shutdown` closes every listener it
  is serving — so an auxiliary attached to the active server would be closed by
  an unrelated LAN-bind toggle, symptom-free except that the tailnet stopped
  answering. Same `buildHTTPServer`, so the handler, timeouts and connection
  context are the same object graph; a different instance so its lifetime is
  its own.
- **No TLS sniffing, deliberately.** A tailnet listener's bytes were already
  encrypted and authenticated by WireGuard before this package saw them, and
  the node's own HTTPS listener arrives here having ALREADY terminated TLS.
  Either way the first byte is not ours to classify.
- **Nothing about admission is relaxed.** `r.RemoteAddr` on a tailnet listener
  is the peer's real `100.64/10` address, so the off-host rule below applies
  unchanged: a non-loopback peer must name a live session. That is the property
  the whole arrangement rests on, and `internal/tailnet`'s two-node integration
  test exercises it against a peer nothing faked.

## The dev-server preview gateway

`previewgateway.go` and `previewproxy.go` hold one TLS listener per port in
this machine's preview set, each reverse-proxying to the dev server on the
SAME port number of loopback, over whichever scheme that dev server speaks
(spec §7, the port gateway). It is the only
listener in this tree that carries somebody else's application, and it is
deliberately NOT an auxiliary listener: nothing about the app's mux,
credential or scope gate applies to it, because none of those bytes are ours.

- **The same port number on both sides.** A dev server's absolute URLs, its
  `<base href>` and every HMR client that derives its socket from
  `location.host` keep working only if the port does not move. A
  path-prefixed proxy breaks Vite's client, which is the client that matters.
- **The forwarding set is an allow-list, never an argument.** Only ports the
  scan attributed to a thread's own session or terminal, plus ports the owner
  named by hand. A loopback proxy that forwarded anywhere would reach every
  host-local service on the box.
- **Its own origin, never the app's.** A different port is a different browser
  authority, so nothing served here reaches the SPA's scripts or storage. The
  one thing a shared HOST still leaks is cookies, which is why the preview
  cookie is named per port, why the port is checked against the grant as well
  as against the cookie name, and why the app's page cookie is honoured only
  by routes that also check an EXACT-PORT origin allow-list
  (`pagecookie_contract_test.go`).
- **The session credential never arrives.** `MintURL` returns a URL carrying
  a single-use 60s ticket bound to `(principal, port)`; the first hit spends
  it for an opaque cookie and 302s to the same address without it. Every later
  request re-checks the principal through `Config.SessionLive` — nothing here
  may cache that answer, exactly as on the WebSocket path.
- **The outbound Cookie header is stripped by NAMESPACE, not by name.**
  Every cookie this app sets is `ReservedCookiePrefix` + something
  (`credential.go`), and the three prefixes — page, session, preview —
  are derived from that one constant, so `stripAppCookies` drops any
  cookie whose name starts with it and a fourth cookie added later is
  covered before it exists. Name-by-name stripping is the version of
  this that works until somebody adds a cookie, and what leaks is a
  credential to an application this process did not write.
  `devgateway_contract_test.go` drives all three, including another
  port's preview cookie, which the browser attaches because a host is
  shared and a port is not.
- **Browser grants have their own lifetime.** Losing all app connections stops
  discovery, but `ReleaseIdlePorts` retains the gateway while tickets, cookie
  handoffs, or unexpired grants exist. Ticket minting and idle retirement share
  the gateway lock; an exchange holds a counted reservation from before ticket
  consumption through grant publication. There must be no gap where idle cleanup
  closes the browser's listener. Explicit port removal and shutdown still retire
  it immediately. Retention never bypasses per-request or socket revocation.
- **A retired listener CUTS the sockets it handed out**
  (`previewconns.go`). net/http STOPS TRACKING a connection the moment
  a handler takes it over from the server, which is what an UPGRADE
  does, so `Server.Shutdown` neither waits for an upgraded HMR
  socket nor severs one, and `Listener.Close` only stops new accepts: a
  port removed from the set left every socket it had given out
  streaming, which is the one path where "stop sharing" did not stop
  anything. `ConnContext` is the only place net/http offers the accepted
  connection before the handler runs, so each listener holds its live
  set there and the proxy handler holds one entry for the whole life of
  the request — which for an upgrade is the whole life of the socket,
  because `httputil.ReverseProxy` does not return until the copy in both
  directions is done. A `*tls.Conn` is cut through `NetConn()`: closing
  the TLS wrapper writes close_notify first and takes the write lock a
  stream in flight is holding.
- **A held socket is re-asked the question it was admitted on, on a
  clock**, not on its next request, because an upgraded socket makes no
  further requests (`previewLivenessInterval`, 10s). Each connection
  carries the TOKEN of the grant that admitted it, not the principal,
  and the sweep runs the same `stillAdmits` the proxy handler runs per
  request: grant present, for this port, not lapsed, principal live. So
  a grant that reaches its 12h TTL under an open HMR socket ends the
  socket the same way it would have ended the next request. Coarse on
  purpose: the check exists so a revoked device stops receiving within a
  human's idea of "at once", and every tick costs one session-store read
  per open preview. The HOST presence has no session row and is never
  cut for one; its grants die with the process.
- **The path is validated BEFORE the ticket is minted.** A ticket book
  is bounded and evicts its oldest entry to make room, so minting first
  meant a call that was always going to be refused still spent a slot
  and invalidated a ticket another page was about to present.
- **TLS on every network path.** The cookie is `Secure` and a browser
  will not store one from a cleartext origin that is not localhost. A tailnet
  with HTTPS turned off therefore has no preview address, which the list says
  rather than silently serving something that cannot hold a cookie.
  `NewContentPreview` is the sole local exception: authenticated on-host HTML
  previews replace all sources with a literal `127.0.0.1` HTTP listener and use
  a non-Secure cookie. Remote requests never select that source. Each confined
  directory has a separate gateway/grant book; `internal/filepreview` owns its
  handler and lifetime. See `docs/architecture/file-previews.md`.
  `Config.FilePreviews` advertises `preview.files.v1` only on execution hosts
  implementing the RPC; a frontend-only controller has no files to preview.
- **The address is asked per bind, never captured.** Every source answers
  `PreviewHost()` immediately before each bind: a tailnet node comes and goes,
  and `PreviewLANSource.LANIP` is a FUNCTION because LAN access is a setting
  somebody toggles and the address moves with the network. A source that
  answers "" is skipped, so the ordered list is the whole address policy.
- **`SetPorts` reconciles; it does not rebuild.** It runs on every discovery
  tick, so an unchanged port keeps the listener it had — rebinding three times
  a second would drop every live HMR socket. A failed bind is a NOTE on that
  port, not an error the caller handles: address-in-use means the dev server
  already bound beyond loopback, so the page is reachable already.
- **A `PreviewTarget` is a port AND a scheme, and the scheme is part of the
  listener's identity.** A dev server on loopback may be TLS; the probe that
  found it knows which, and the value travels from `devscan.DevServer.Scheme`
  to the dial rather than being assumed. A port whose scheme CHANGED is a
  different upstream, so it is rebuilt rather than kept — an unchanged one
  still is not. The upstream transport skips certificate verification for the
  same reason the probe does: the hop is a loopback literal this process
  chose, and verifying would refuse every https dev server while proving
  nothing about the one hop involved. `Origin`, when present, is rewritten to
  that same scheme.
- **No `WriteSecurityHeaders`, and that is the one route excluded by name
  from `TestEveryHTTPRouteCarriesThePolicy`.** A policy this process invented
  for an application it did not write would silently break it.
  `devgateway_contract_test.go` is what replaces that gate, and the e2e pair
  `preview-gateway.spec.ts` / `compact-preview-gateway.spec.ts` drives the
  same rules from a paired browser against a dev server that RECORDS what it
  received, so a rewrite that would have made a real one refuse fails on the
  record rather than on a green screen.

**Every header rule is a verified fact, not a guess.**
`docs/references/dev-server-proxy.md` records them with the version and date
they were verified against (Vite 8.2.2, 2026-09-02). Two of them fail in ways
that look like success: the Host check applies to the WebSocket UPGRADE as
well as the HTTP path, and a changed path hangs the upgrade with no response
at all. `vite-ping` bypasses the host check entirely, so a ping-only probe
proves nothing — the contract test drives a real upgrade. Change a rule only
with a new spike, and update that file in the same commit.

## Every new App method is also a wire RPC, so annotate it

Adding an exported method to `App` puts it on the wire, so every one carries a
`//ao:scope <name>` directive in its doc comment naming the capability it
exercises — the same comment-directive form as `//wails:ignore`. `methodgen`
**fails the run** listing every unannotated method, and again for a name the
vocabulary does not declare. There is no default and no silence: the generator
is the gate, and it runs before a method reaches the wire at all.

`scopes.go` is the vocabulary — the scope names a session can be granted
(`docs/specs/remote-access.md` §5) plus the two values that are method
PROPERTIES rather than grants — and the tier each resolves to: **session**
(the floor), **observe** (`threads:read`, `files:read`, `settings:read`),
**execute**, **host**. It restates `internal/identity`'s grantable names rather
than importing them, since this package stays store-free; `internal/app`
imports both and `TestScopeVocabularyMatchesIdentity` fails in either
direction, and neither non-grant value may appear in identity's list.

The two non-grants:

- **`host`** marks a call with no remote form. Authorized by presence alone.
- **`session`** is the FLOOR: any connection that named a live session passes,
  because that is the whole requirement. It carries the calls whose real
  authority is decided per ARGUMENT (`UpdateSettings`, gated key by key
  against §6's three tiers) or is simply "this session is writing its own
  bucket" (`GetUIState` / `SetUIState` / `DeleteUIState`, whose bucket comes
  from the connection and never from a parameter, and `RegisterPushToken` /
  `UnregisterPushToken`, whose DEVICE comes from the connection's session for
  exactly the same reason — a device id taken as an argument would be a way to
  have somebody else's phone woken). A device granted nothing but reads setting
  its own font size is the case it exists for. The step-up ceremony pair
  (`BeginPasskeyStepUp` / `FinishPasskeyStepUp`) is there for a stronger reason:
  it is how a session SATISFIES the gate that just refused it, so requiring any
  grant would leave step-up reachable only to sessions already holding
  something — and the calls behind step-up are the ones no standing grant opens.
  `TestSessionFloorMethodsAreTheSpecSet` pins WHICH methods carry it, because
  the floor admits everybody: an annotation that drifted onto a method whose
  authority its NAME decides would be an ungated surface.

`//ao:stepup` marks the calls §4 requires a fresh per-call proof for: minting a
pairing link, pairing the selected computer with an agent peer,
BEGINNING a passkey registration, network bind / exposure
changes, provider custom-env writes, MCP config writes, the WSL distro
preference, worktree-setup recipe writes (stored argv that runs unattended
on every worktree cut), and installing or withdrawing the push sender
credential (the service-account key this backend wakes every registered phone
with).
`TestStepUpMethodsAreTheSpecSet` pins that list, because a dropped directive
turns a mandatory proof into an ambient standing grant and nothing else in the
tree would notice.

**A multi-call ceremony is proved ONCE, at the call that starts it.**
`FinishPasskeyRegistration` deliberately carries no directive: it is
unreachable without the in-memory, single-use, minutes-long ceremony handle
that only a step-up-proven begin mints, so the registration IS proved and a
second proof would guard nothing the first does not. It is also worse than
nothing — the second ceremony gets answered by the credential the
authenticator just created and this backend has not stored yet, so a REMOTE
registration failed on its own success. The test's `want` map says so where
the absence is, since an omission reads as an oversight otherwise.

`internalmethods.go` holds `InternalServiceMethods`: Wails framework hooks and
`//wails:ignore` methods, never registered, as defense in depth beside the
codegen filter. That is all it holds.

### One gate decides what an off-host caller reaches

**There is no per-method origin partition.** `LocalOnlyMethods`, the
`transitionalReachability` override map, and the frozen `preScopeTableLocalOnly`
list they were held against are deleted (wave 6d2). They existed because a
launch-credential client could be off-host and unnamed, so "is this peer on this
machine" was the only fact available to judge it by. It no longer is: wave 6d1's
admission rule requires every non-loopback `/ws` upgrade to name a session, and
`internal/app`'s `bindingAdmitsPeer` refuses a `loopback-only` session presented
by a non-loopback peer. An off-host caller is therefore always a named session,
and what it may call is the scope gate's answer.

Deleting it changed reachability deliberately and in one direction: the twelve
diff / workspace-content methods that were pinned local-only in 2026-05 now
answer a session granted `files:read` (two of them `threads:read`), and the
twenty-one thread / project / discussion bookkeeping mutations ride
`threads:operate`. That is what those scopes are for.
`TestWorkspaceContentAnswersASessionGrantedTheScope` and
`TestBookkeepingMutationsRideThreadsOperate` (`reachability_test.go`) pin the new
truth by name, and `TestHostScopedMethodsStayRefusedForEveryOffHostSession` pins
the floor the deletion must not lower.

What survives on the dispatcher is `RegisterOptions{LocalOnly}`, which marks a
whole RECEIVER rather than a method. The harness is its only user: host tooling
registered unfiltered under `--harness`, carrying no `//ao:scope` annotations at
all, so no grant could reach it even if a session tried.
`Dispatcher.ResolveForOrigin` refuses it from a non-loopback peer with the same
`method_not_found` shape an unregistered method returns, so that receiver stays
unenumerable off-host (`dispatcher_localonly_test.go`). `isLoopback` still
travels with the connection for two more reasons: `InvokeForOrigin` redacts
method-error text for a non-loopback peer, and host presence is one of the two
proofs that satisfy step-up.

Method bodies do not re-check origin. A reverse proxy on the same host makes
remote peers appear loopback and defeats this locality, so proxy from a
different host instead.

### The per-RPC scope gate

`authorize.go` is where that answer is computed. `AuthorizeSessionMethod` asks
**"was this session granted this capability"**, and it runs only for a
connection that named a durable session — a launch-credential client passes
through untouched, because it names none, and the admission rule already puts it
on loopback.

- **Nothing caches.** The grants are re-read per call through
  `Config.SessionScopes`, satisfied in `internal/app` over
  `identity.Sessions.Live`. A revocation lands after the upgrade that admitted
  the connection, so a grant read once at upgrade time would outlive it (§4
  "Revocation": no RPC authorizes from state cached at upgrade time).
- **`host` is decided by presence, never by the grant set** — refused without
  it, admitted with it. No session may hold `host`, and the embedded webview's
  own local-channel session calls host-scoped methods constantly. A method name
  the generated table does not carry classifies as `host` for the same reason:
  fail closed.
- **`session` is decided the same way, by having got here.** Only a connection
  that named a live session is judged, and its liveness was re-read for this
  call, so the floor admits an EMPTY grant set. Looking for `session` in the
  grant set would refuse every session.
- **Step-up goes through `stepUpProven`**, one function whose doc comment
  carries §4's argument. TWO proofs satisfy it, and §4 names both: standing
  at the machine, or a passkey assertion this backend verified moments ago.
  Neither is standing — host presence is the kernel's answer about this
  connection's peer, and a step-up token is single-use, expires in minutes,
  and is bound to the session that asked (`Config.StepUpProof`, satisfied in
  `internal/app` over `identity.SpendStepUpToken`). The passkey path did not
  loosen the set; it made the set reachable at all to an owner who is not at
  the machine, which was previously impossible by construction.
- **The proof is resolved ONCE per call, into `CallerProof`, and carried on
  the ctx** (`WithCallerProof`). It has to be: resolving it SPENDS the token,
  so a method's own argument recheck must READ the gate's answer rather than
  ask again — `internal/app`'s `requireStepUp` reads
  `StepUpProvenFromContext`. That is also why the token rides
  `ClientFrame.StepUpToken` rather than a parameter: no method signature
  mentions it, and both the gate and the in-method recheck need it.
- **Two typed refusals**, following the `ErrCodeGrantRequired` precedent:
  `scope_required` carries the missing scope in `FrameError.Scope` (a FIELD,
  because prose does not survive the wire for a non-loopback caller and a
  client explaining a disabled surface has to branch on something stable), and
  `step_up_required` is its own code because no grant can satisfy it.

**A method's annotation is the FLOOR, not the whole answer.** Literally so for
the methods scoped `session`, whose whole authority is rechecked. Authority that
depends on a call's ARGUMENTS is rechecked inside the method — selecting an
autonomous runtime mode, writing a host-tier settings key — using
`transport.ScopeRequired` / `StepUpRequired` / `AuthRefused`. Those errors reach
the wire as themselves: `Dispatcher.processResults` consults `AuthzFrame` before
its correlation-id redaction, so the client that most needs to know which scope
it lacks is not told "method failed". The helpers live in
`internal/app/app_authz.go`; see that package's guide.

## Every route on this mux is also a row in internal/surfaces

`buildHTTPServer` registers patterns as plain literals (or constants
holding one) because that is what `internal/surfaces`' AST gate reads:
add a route here without a `Route` row there and the gate fails. Keep the
pattern a literal or a constant for the same reason — a computed pattern
is a route the gate cannot see.

Four packages outside this one build a URL for `/bootstrap.json` or `/ws`
without linking this server (`relaysession`, `clientmode`, `deviceclient`,
and the Windows launcher through the first of those), so both patterns are
exported constants — `BootstrapPath` and `WSPath` — and the drift guards in
`relaysession` and `deviceclient` pin their copies to them. A rename that
missed a copy is otherwise a client dialing a route this server does not
serve, which only a live session notices.

`/healthz` is the one deliberately unauthenticated route besides the
bundle. It answers version + backend id because its two consumers — the
pre-WS compatibility check and the update watchdog — run precisely when
no valid credential is held; a token-gated health route answers 404 for a
restarted backend, which is indistinguishable from down and is the exact
condition it exists to detect. It is still not readable cross-origin: no
`Access-Control-Allow-Origin`, plus the same `loopbackHostGuard` the
credentialled routes use. Readiness stays `/bootstrap.json`'s 503 and
must not be folded in here.

## Hello frame

Every upgraded connection is written a `{"type":"hello"}` frame FIRST,
synchronously, before the event pump or keepalive goroutines exist
(`conn.go writeHello`). The ordering is the contract: a client that reads
hello first seeds its compatibility state before anything else lands and
needs no "have I been told yet" branch on every other frame. Racing it
against the pump would make that guarantee probabilistic.

It carries `protocolVersion`, `capabilities`, `backendId`,
`backendName`, `serverTimeMs`, `replayBaseline`, and the three bundle fields
(`bundleId`, `bundleVersion`, `minShellBuild`).

`replayBaseline` captures every registered channel's head (including zero)
atomically with subscribing to live delivery, filtered by the same audience
and scope gates as events. Clients seed only missing cursors; a reconnect's
hello must never advance existing cursors past events missed during the
outage. Tracking only previously received channels loses a channel's first
event during disconnect — notably `turn_completed`, leaving a finished turn
looking active. Baselines describe the subscription boundary, not historical
delivery; `notification:activated` retains its separate cold-launch replay.

- **Nothing gates on the version.** Features negotiate through capability
  flags: a client asks "does this backend have X" and degrades on the
  answer instead of inferring from a number (spec §9). `ProtocolVersion`
  moves only for a change that alters what an EXISTING frame or field
  means; adding a frame type, field, or channel is additive and does not
  move it. Additive-only is what makes the swap window — an old bundle
  live against a just-updated backend — safe.
- **Capability names are frozen by tests**, and the frozen lists spell
  each name as a literal so a rename cannot slip through it. A name is
  stable forever once shipped, because a client on an older bundle may
  still ask about it; retiring one means the backend stops advertising
  it, never that it starts meaning something else. A flag says a behavior
  EXISTS — it is never authorization, which is re-checked per RPC. **Ship
  a flag in the same release as the behavior it names.** Added later it
  lies about every build in between, which advertises nothing while
  having the behavior — so the flag lands with the change even when
  nothing reads it yet. The reader can come later; the flag cannot.
- `capabilities` serializes as `[]`, never `null`, so "advertises
  nothing" stays distinguishable from "too old to send this frame".
- **One flag varies by DEPLOYMENT, and none varies by caller.**
  `CapabilityBrowser` is advertised only when the backend actually has a
  browser engine (`Config.BrowserAvailable`, resolved per accept because
  the Manager picks its engine during the App's startup): a serve host
  advertises it only when a Chromium is installed on the machine, and a
  `--connect` backend never does. That is still a property of the
  BACKEND, which is the line that matters — anything that varies by who
  is asking is authorization and belongs in the scope table. The
  conditional set is a second package-level slice
  (`serverCapabilitiesWithBrowser`), so an accept picks one rather than
  allocating, and the frozen prefix's bytes are identical either way.
- **`backendName` is display, never identity.** It is the host's name
  (`internal/appidentity.HostDisplayName`, the same string the pairing
  payload shows a device deciding whether to trust an offer) and it exists
  so a client attached to several backends can label them
  (`docs/specs/remote-access.md` §10, "Machine name"). Two backends may
  legitimately answer the same one, so nothing keys on it and `backendId`
  stays the identity. `Config.BackendName` is a plain string rather than a
  getter like `BackendIdentity`: a hostname is knowable at boot and does
  not arrive with the store. There is deliberately NO setting behind it —
  the display name IS the hostname, and a nickname is the client's, kept
  from pairing time. Unset omits the field, which reads as unknown, the
  same answer a backend too old to send it gives.
  `/bootstrap.json` carries it too, and the two are not redundant: a page
  decides what to label a backend before it opens a socket, and the
  manifest is the only thing a page holding no credential can read.
- `serverTimeMs` is sampled per accept, not cached at boot: the field
  exists so a client can measure its own skew, and a cached value would
  be wrong by the process uptime.
- **The three bundle fields are on the frame, not behind a route**
  (wave 6g-a). They describe the SPA this backend serves — `bundleId`
  is `internal/bundle`'s CONTENT id, `bundleVersion` is `main.version`,
  `minShellBuild` is the lowest Android `versionCode` this bundle's
  native seams can run on. The one client that reads them compares them
  against something it already holds on every connection, so a shell
  that had to fetch a document to learn "nothing changed" would pay a
  round trip per connect forever. All three are omitted when
  `Config.Bundle` is nil or its manifest cannot be built, which reads as
  "this backend does not supply bundles" — the same answer a backend too
  old to send them gives, and the one that leaves a phone running what
  it has. The manifest behind them is cached behind a `sync.Once`, so
  the first connection of a process pays the walk on its own goroutine
  and every later one is a struct copy.

Every frame consumer must ignore what it does not recognize — unknown
frame types, unknown event channels, and unknown fields on frames it does
know — without erroring and without dropping the parts it does
understand. That tolerance is what lets a frame or field be added in one
release and consumed in the next. The Go clients get it by having no
`default` in their type switch (`wsllauncher/notification_client.go`,
`harnessclient`); keep it that way. The SPA's client counts unknown input
instead, so the condition stays observable — see
`frontend/src/lib/transport/AGENTS.md` and the future-dialect fixtures in
`wsClient.test.ts` and `e2e/tests/transport-forward-tolerance.spec.ts`.

## Credentials and refusal shapes

`credential.go` owns the launch credential and is the only place a request is
authorized; `auth.go` holds the primitives it compares with. `NewToken` returns
32 random bytes (256 bits) base64url-encoded, and `New` mints one when
`Config.Token` is empty, so a token is per launch, never persisted, and a stale
bookmarked URL cannot reach a new boot. `ConstantTimeEqual` is the only
comparison, and it answers `ErrEmptyToken` when either side is empty: an unset
server token must never authorize an unset client.

`Credential.Authenticate` is the one validation function. Three carriers reach
it, and they all end in that same comparison:

- **The page cookie** `ao_page_<port>`, which every request from a loaded page
  rides (the manifest refetch, the `/ws` upgrade).
- **`Authorization: Bearer`**, presented by a client that is not a browser: the
  WSL launcher's probe and notification socket, `ao-harness`, the `--connect`
  stub dialing an upstream on this machine. A `--connect` stub that PAIRED
  holds no launch credential and presents none — it names its device session
  instead, which is what the admission rule below requires of it anyway.
- **`?token=`**, for clients that can build a URL and nothing else — the browser
  and Node WebSocket APIs cannot set handshake headers.

Do not add a fourth check anywhere. A route needing a credential calls
`Authenticate`; `/bootstrap.json` calls `Exchange`, which is `Authenticate` plus
consuming the page ticket and writing the cookie. When `Exchange` refuses, the
manifest falls back to `SessionForRequest`: a live durable session (a paired
device's credential header, or a session cookie that outlived the launch that
planted it) already admits the `/ws` upgrade, and the manifest must not be
stricter than the socket it describes. The fallback never writes the local
page channel's session cookie — that credential is reserved for requests the
page credential admitted, from a peer on this machine. On the upgrade itself, a spent WS ticket naming a
live session stands in for `Authenticate` (the ticket was minted moments ago
by presenting that session's credential); the ambient-cookie arm gets no such
waiver, and the Origin check runs on every upgrade regardless.

**A page gets a one-time ticket, never the session token — and which channel
carries it depends on who opened the page.** A BROWSER gets it in the URL
(`?t=`), because a URL is the only channel that reaches one: `Server.AppURL` at
boot, `MintPageTicket` for the LAN share URL, the `PageURLPath` route
(`/pageurl`) for every later navigation. A WEBVIEW window this process owns gets
it by `ExecJS` instead, and its URL is bare: the same Go process that mints the
ticket also holds the `WebviewWindow`, and a page URL is copyable, lands in logs
and window diagnostics, and outlives its single use in shell history and error
reports. Either way the first `/bootstrap.json` presenting the ticket exchanges
it for the cookie; the SPA strips a URL ticket from the address bar, and a
reload rides the cookie. Outstanding tickets are bounded
(`maxOutstandingTickets`, oldest evicted), so the newest URL a user just copied
is always live.

**The webview channel is `internal/pagehost`** — one stdlib-only package (the
Windows launcher links it without linking this server) holding the marker param
the bare URL carries (`host=webview`), the two names the injected script writes
(`window.__aoPageTicket` and the `ao:page-ticket` event), the `/pageurl` JSON
answer, and `DeliveryScript`, the ONE place that script is rendered.
`internal/uiwindow.DeliverPageTicket` is the Wails half, shared by all three
window hosts (`main_desktop.go`, `cmd/agent-overflow-windows`,
`internal/clientmode`): it answers `events.Common.WindowRuntimeReady` by minting
a ticket and `ExecJS`ing it, once per document, so a reload gets a fresh one and
re-delivering a spent one is harmless. The SPA raises that readiness itself
(`frontend/src/lib/transport/pageHost.ts`), because it replaces
`@wailsio/runtime` and nothing else in the page will.

**There is ONE ticket mechanism** (`ticket.go`), and every user shares it. A
`ticketBook` mints a CSPRNG token, hands it out over a channel that is already
authenticated, and lets the FIRST presentation spend it — with a constant-time
compare, an eviction rule, and a bound. The four books differ in exactly two
parameters:

| | subject | deadline |
|---|---|---|
| page ticket (`Credential.tickets`) | none — a launch has one page credential, so the ticket only decides who receives it | none — a URL ticket is produced for a person to open, and a launcher's fixed `?t=` URL must still work an hour later |
| WS ticket (`Server.wsTickets`) | the session id it names | `wsTicketTTL` (30s) — a client mints one immediately before it dials |
| attachment download (`Server.attachmentDownloadTickets`) | the `(thread, attachment)` pair it admits | `attachmentTicketTTL` (30s) — same reason as the WS ticket |
| attachment upload (`Server.attachmentUploadTickets`) | the thread, filename, content type and exact byte count the stored row will carry | `attachmentTicketTTL` (30s) |

Do not add a fifth implementation. A new single-use token is a new book with
those two parameters set; building it separately means a second constant-time
compare and a second place for "single use" to be got subtly wrong.

The two attachment books are separate FROM EACH OTHER on purpose, and that is
the one place this file recommends more books rather than fewer: a ticket minted
to read must not be spendable to write, whatever its subject would parse as, and
one shared book would make that a property of subject parsing instead of a
property of the mechanism. Because a
ticket is spent by the page it was minted for, anything that navigates more than
once asks `/pageurl` for a fresh one rather than reusing the boot URL:
`ao-harness open`/`info`/`attach`/`up` and the e2e `HarnessApp.open()` take the
plain-text ticketed URL, and the WSL launcher's reload keybinding takes the
`?host=webview` JSON answer, which keeps the two halves apart so the URL it
navigates to stays bare. That route is itself credentialled, and the two
binaries that call it without linking this package restate its path behind a
drift-guard test (`internal/wsllauncher`, `internal/harnessclient`).

Cookie attributes are argued where they are set (`pageCookie`): HttpOnly always,
so page script cannot read the credential back; SameSite=Strict; `Path=/`;
host-only; session lifetime, matching a per-launch token; and Secure only when
`r.TLS` is set, since a Secure cookie planted over the listener's cleartext half
would never be stored. That is `r.TLS` and never `X-Forwarded-Proto`, which the
manifest's socket scheme DOES read (`requestIsHTTPS`): a header a caller can set
about itself is safe where the worst outcome is a URL that will not connect, and
not where the outcome is a credential the browser silently drops.
The name carries the listen port because cookies are scoped by host and path but
**not** by port — two backends on one host (the app and a harness instance, an
app and a `--connect` stub) would otherwise overwrite each other's.

**The launch credential admits a `/ws` upgrade only from THIS machine.** It
names a backend LAUNCH, never a client, so a connection carrying nothing else
has no session id: `CloseSession` cannot reach it and the per-RPC gate has no
grant set to read. Unattributable and unrevocable is tolerable for the host's
own processes and nowhere else, so `handleWS` requires a non-loopback peer to
NAME a live durable session (spec §4, "Local clients") — through the spent
`?ticket=`, the `X-AO-Session` header, or the `ao_session_<port>` cookie, the
carriers `SessionForRequest` already reads. Locality is
`loopback.PeerAddress`, the same predicate everything else here uses; there is
no second one. This NARROWS what a sessionless credentialled connection may be
and loosens nothing — the launch credential is still demanded wherever it was
demanded before. A nil `SessionForRequest` therefore refuses every non-loopback
peer: a server that cannot resolve a session cannot admit one either, and the
host's own clients are unaffected.

| peer | presents | upgrade |
|---|---|---|
| loopback | launch credential, no session | admitted |
| loopback | a live session | admitted |
| non-loopback | a live session | admitted |
| non-loopback | launch credential alone | `http.NotFound` |
| non-loopback | nothing | `http.NotFound` |

`/bootstrap.json` is deliberately NOT narrowed the same way. The LAN share URL
must keep loading the page, because the person holding it has to be able to
act — and what they see is the SPA's pairing prompt, not an outage
(`frontend/src/lib/transport/AGENTS.md`). `wsadmission_test.go` pins the matrix,
and its `remotePeerListener` is the one way this package produces a non-loopback
peer: a listener whose accepted connections report a LAN address, which is
exactly what net/http copies into `Request.RemoteAddr`. `auxlistener_test.go`
reuses it rather than growing a second, so an auxiliary listener is proven to
apply the same matrix.

Both credentialled entry points answer a refused credential with
`http.NotFound`: `/bootstrap.json` (`handleBootstrap`) and the `/ws` upgrade
(`upgrade`). The 404 is deliberately indistinguishable from "no such path" so a
LAN scanner cannot fingerprint us, and it is the only positive auth-rejection
signal on the wire. The SPA keys terminal state on it: a 401/403/404 on the
manifest refetch latches `'unauthorized'` and stops the reconnect ladder for a
network-served session. So a transient failure must keep its own status. The
readiness gate stays 503 and the startup-failure page stays 500, and neither may
become a 404, or a client that is merely early gets told its credential is dead.
See `frontend/src/lib/transport/bootstrap.ts` (`BootstrapRejectedError`) and
`wsClient.ts` (`enterCredentialDead`).

**A method error's TEXT does not survive the wire for a non-loopback
caller** — it is replaced with `method failed (id: <cid>)` and the prose
goes to the server log. So a client-side check that reads an error
MESSAGE to classify a failure works on the desktop and silently does
nothing over the network. Anything a client must branch on gets a stable
CODE instead (`frame.go`), and the method wraps the matching sentinel:
`ErrTemporarilyUnavailable` for "retrying may work",
`ErrAlreadyHandled` for "another client already decided this". When you
add one, wrap rather than replace, so callers testing for the concrete
cause keep working.

### Two refusal shapes, and which one applies

A request the backend cannot attribute to anything gets the 404 above and
learns nothing. A caller whose SESSION credential was read and then refused
gets `ErrCodeAuthFailed` with a `reason` naming which check refused it —
built by `AuthFailure`, never by a struct literal, because the two fields
are only meaningful together.

The split is about what the answer discloses. The 404 hides whether a route
exists from someone who has presented nothing; the typed reason tells a
paired client which of its own credentials went wrong, which it needs in
order to say anything more useful than "not allowed". A refusal that is
already attributable discloses nothing by being specific.

`reason` values are the wire spellings of `internal/identity`'s closed set.
Transport carries the string and does not interpret it: `internal/identity`
owns the vocabulary (transport cannot import it without pulling the store in
behind it), and `frontend/src/lib/transport/authReason.ts` is the one place
it becomes a sentence. `TestFrontendHintsCoverEveryRefusal` pins the two
sets together. A code is stable forever once shipped; an older bundle may
still be mapping it.

## The device-facing credential routes

`authroutes.go` holds five POSTs (spec §4), the only routes on this mux a
client reaches WITHOUT the launch credential — because they are how a client
that has never met this backend gets one.

| route | what the caller presents | registered when |
|---|---|---|
| `/auth/pair` | a single-use pairing token, plus the proof of the key the device generated first — signed in `X-AO-Device-Key`, or a bare identifier in the body for a device that cannot sign | `Config.AuthEndpoints != nil` |
| `/auth/token/recover` | a saved client-chosen successor plus the predecessor and a fresh device proof; older hosts must never consume this exchange | recovery-capable `AuthEndpoints` |
| `/auth/token` | a rotating refresh secret in the body, its device proof in `X-AO-Device-Key` | `Config.AuthEndpoints != nil` |
| `/auth/passkey/begin` | nothing at all, and no body | `Config.AuthEndpoints != nil` |
| `/auth/passkey/finish` | an assertion over the challenge that begin issued, plus a device proof in `X-AO-Device-Key` | `Config.AuthEndpoints != nil` |
| `/auth/ticket` | the session credential it already holds | `Config.SessionForRequest != nil` |

Rules that hold across all five:

- **This package owns the wire, not the decision.** `AuthEndpoints` is a
  five-method interface declared here and satisfied by an app-side adapter over
  `internal/identity` — the same direction and the same reason as
  `ScopedTokens`. The DTOs (`PairingRedemption`, `SessionRenewal`,
  `TokenGrant`, `PasskeyChallenge`, `PasskeyAssertion`) are dumb: nothing here
  validates a token, verifies a signature, interprets a reason code, or knows
  what a session row is. The WebAuthn options and the browser's response cross
  as raw JSON, because a typed copy here would be a second definition of the
  specification's shape that agrees with the library's only until it grows a
  field.
- **A grant publishes what it carries.** `TokenGrant.Scopes` is on both
  redemption and rotation responses, always an array and never null, because
  the client has no other way to learn what its own session may do — and an
  absent field has to stay distinguishable from an empty grant set, or a
  backend too old to send one reads as "granted nothing". It is DISCLOSURE, not
  authorization: the gate re-reads the session row per call
  (`Config.SessionScopes`), so a client editing the copy changes nothing.
  `frontend/src/lib/transport/scopes.ts` is the consumer.
- **A refusal is 401 with `{"reason": "<code>"}`**, whatever refused it. The
  code is the whole message; prose belongs to
  `frontend/src/lib/transport/authReason.ts`, which can phrase it for the
  surface the person is looking at. Mapping codes onto distinct statuses would
  put the same fact in two places.
- **A proof never comes from the body.** `SessionRenewal.DeviceProof` and
  `PairingRedemption.DeviceProof` are read from `DeviceKeyHeader` and both JSON
  tags are `-`. A proof a caller may write into the same document it is proving
  something about is not a proof. The same rule is why `Method` and `Path` are
  filled in HERE from the request: a signed proof binds them, and one that named
  its own target would prove nothing about where it was presented.
  `PairingRedemption.KeyThumbprint` survives in the body because it is not a
  proof — it is an identifier a keyless device asks to be known by, and the app
  side ignores it entirely whenever a header proof is present.
- **Origin runs before anything else**, for the reason it does on the bootstrap
  exchange: these routes hand out credentials, and a request another origin
  initiated must never be answered with one.
- **One budget for all three** (`authRateLimit`), because they are alternative
  ways for the same peer to ask this backend for a credential.
- `DeviceKeyHeader` carries one of two shapes, and this package does not
  distinguish them — which one a given device may present is
  `internal/identity`'s answer, read off the device row (`proof_kind`, migration
  v81). A device that enrolled an ECDSA P-256 key presents a compact JWS signed
  over THIS request (`internal/identity/deviceproof.go`); one that could not
  presents its bare enrollment thumbprint, which is the plain-HTTP LAN browser
  of spec §15 constraint 6 — no secure context, so no `crypto.subtle`, so
  deliberately no signed path. A device that enrolled a key is never accepted on
  the bare shape, on any route. Phase 5 swapped the VALUE and moved no call
  site, which is what the header being named for the KEY bought.

**Only the SIGN-IN ceremony is a route.** A passkey has three uses and the
other two — registering a credential, and proving step-up — are made from a
surface that already holds a session, so they are bound methods
(`internal/app`). Registration in particular must never be reachable without
one: it is what a later sign-in trusts. What sign-in gets a route for is that
its caller holds nothing, which is the whole point of it.

**Availability is a per-request ANSWER, not a registration condition.** The
routes exist whenever the identity seam does; a backend with no canonical
domain answers `passkey_unavailable`. Making registration conditional would
put a rebind inside "the owner named a domain", and unavailable is the only
answer a client can explain anyway. The reachable half is published twice, and
the two say different things: `CapabilityPasskeys` in the hello frame says this
backend SPEAKS the ceremonies (a dialect fact, frozen with the rest of the
list), while `Bootstrap.PasskeysAvailable` says one can be RUN right now (a
configuration fact, which has to reach a page that holds no credential and has
opened no socket).

**The owner-facing half of pairing is deliberately not here.** Minting a link,
reading the verification number, confirming and cancelling are Go API on
`identity.Sessions`, called from an already-authenticated in-process surface.
Putting them on the wire would add an unauthenticated-by-construction surface
for a caller that does not exist.

**The session credential has two carriers and one reader**
(`SessionCredential`): `X-AO-Session` for a client that can set headers, and the
HttpOnly `ao_session_<port>` cookie for the browser, which cannot set them on a
WebSocket handshake. The **header wins** when both are present — a relay
forwarding a credential on purpose (the WSL launcher) is making a statement
about whose request this is, while an ambient cookie is the browser's default.
This package decides where the string is; `internal/identity` decides what it
means. The cookie is planted by `/bootstrap.json` from
`Config.PageSessionCredential`, so a local client acquires the page credential
and its session in the SAME exchange and neither needs a route of its own.

**That plant is for LOOPBACK peers only.** `PageSessionCredential` hands out
the backend's own `loopback-only` session, and the presentation side refuses
that binding class from an off-host peer (`internal/app`'s
`bindingAdmitsPeer`, argued in `internal/identity/AGENTS.md`). Planting it
anyway would hand a page a credential that stops working the moment it is
used. The LAN share URL still gets the page cookie and still loads — the
person holding it has to reach the pairing prompt — it just arrives with no
local channel.

## The CDP tunnel route

`/browser-cdp` (`webview2host.CDPTunnelPath`) is a credentialled entry point of
its own: the Windows launcher dials it to carry the embedded browser pane's CDP
traffic into the WSL backend. It is registered ONLY when `Config.CDPTunnel` is
set, which the executable does only on the WSL deployment, so on every other
build the path does not exist.

Same credential and same locality rules as `/ws`, and deliberately no wider:
`loopbackHostGuard` on the Host header, the launch credential through `upgrade`
(never a session ticket — `sessionProven` is false here), and
`loopback.PeerAddress` on the peer, all answering 404. The socket is handed
whole to `CDPTunnelEndpoint.ServeCDPTunnel` — a byte-stream multiplexer, not an
RPC surface — so no method table, replay ring, or event policy applies to it.
This package does not speak the frame protocol; the interface is one method so
that `internal/cdprelay` owns the codec and this package owns only who may
reach it. A LAN peer that could open this route would be driving a real browser
window on the user's desktop, which is why none of the three checks is
optional. No `//ao:scope` annotation applies: this is not an RPC surface,
so the per-call gate never sees it.

## The attached-backend routes

`/ws/backend/<id>`, `/bootstrap/<id>.json` and `/backend/<id>/attachments/…`
(`attachedroutes.go`) are how ONE page drives several computers
(`docs/specs/remote-access.md` §10). The desktop's answer is not a second
window and not a cross-origin fetch: the local backend carries one socket per
attached machine on its own listener, so every backend the page talks to is
same-origin, the page holds exactly one credential, and the pinned device
session for each remote machine never leaves this process. The phone realizes
the same seam by opening those sockets itself, which is why the manifest
PUBLISHES the URLs (`Bootstrap.Backends`) instead of the SPA composing them.

Registered only when `Config.AttachedBackends` is set, and satisfied by
`internal/attachedbackends`. This package asks that seam two questions —
which profiles exist, and give me the carrier for this id — and nothing else.
The hop behind it is `internal/backendproxy`, shared with `--connect`.

Three checks, and they are one function (`attachedCarrier`) so the answer to
"may this caller use another machine's credential" cannot differ between the
routes: the loopback PEER, the live origin allow-list, and this listener's PAGE
credential. Narrower than `/ws` by that first check, deliberately. An off-host
client realizes this seam with its own paired session to each backend;
carrying it through here would lend a device THIS machine's pinned credential
for a machine it never paired with, and would make a revocation on the far
backend mean nothing to the client actually driving it. Every refusal is 404,
so an unattached backend, an unadmitted caller and an absent path are
indistinguishable. `Bootstrap.Backends` is likewise emitted only for a request
these routes would admit — a menu of doors that answer 404 is worse than no
menu.

`/bootstrap/<id>.json` is the one that is not a pure pipe: it answers the far
side's manifest narrowed to `AttachedManifest`'s closed list (store identity,
name, launch id) with `wsUrl` rewritten to name THIS listener, because that is
where the page's socket goes and the only `wsUrl` the SPA accepts. The far
side's `harness`, `pageMarker` and `passkeysAvailable` describe the listener a
page loaded from and are dropped rather than forwarded. Reachability is
published nowhere: probing every attached machine to answer one page load
would make a boot as slow as the slowest sleeping laptop, and each socket is
the only current answer anyway.

No `//ao:scope` annotation applies to any of the three — they are not RPC
surfaces. The four App methods that MANAGE the set are (`ListBackends`,
`AddBackend`, `RemoveBackend`, `RenameBackend`: all `host` scope, all `home`
route).

## Origin allow-list and peer locality

**`OriginAllowed` (credential.go) gates `/ws`, `/bootstrap.json` and
`/pageurl`, ahead of the credential check.** It deliberately does not gate the
`/attachments/` byte routes; § Attachment bytes says why. A request with no `Origin` header is
a client that is not a browser (or a same-origin GET, which browsers send without
one) and passes to the credential and peer rules. A request *with* an `Origin`
passes only when it names the authority this request was addressed to — scheme
from the TLS state, authority from the `Host` header, so the answer stays true
across a rebind, a port change, and every spelling of loopback — or matches a
pattern the LAN bind adds. Those patterns name EXACT PORTS
(`internal/network.OriginPatterns`, which takes the bound port): until wave 9
they were `http://localhost:*` and its siblings, so a document served by any
other port on this machine named an admitted origin — and this machine now
also runs the dev-server preview listeners on other ports of the same hosts.
`pagecookie_contract_test.go` is the structural half: it reads the source of
this package and `internal/clientmode` and fails on any function that reads the
page cookie without asking the origin question in the same body, and it drives
the real routes with a real cookie and a preview-shaped Origin. A reader that
fails it gets the check, never an entry on its exemption list. The TLS state,
deliberately, not `requestIsHTTPS`: a
caller-supplied header must not widen an authorization check, so a deployment
behind a TLS-terminating proxy allow-lists its origin explicitly rather than
talking its way past this one (the spec calls a reverse proxy unsupported until
phase 3's model lands, not silently degraded). `internal/network.OriginPatterns` produces those
patterns and this package enforces them. Read the list live, through
`currentOriginPatterns()` per request rather than `Config.OriginPatterns`, since
`SetOriginPatterns` and `Rebind` rotate it under `mu`. Sockets already upgraded
keep their handshake-time policy.

The check is load-bearing on loopback too, which is why the empty list no longer
means "accept anything". Cookies are scoped by host and not by port, so a page
served by any other listener on this machine has our page cookie attached for it
by the browser, and a WebSocket handshake is not subject to the cross-origin read
rules that keep that page out of `/bootstrap.json`. SameSite cannot cover the
gap — same-site ignores ports. `upgrade` therefore hands coder/websocket
`InsecureSkipVerify: true` and owns the decision itself, so one rule applies on
loopback and LAN alike.

**The `loopbackHostGuard` on `/bootstrap.json`, `/ws`, `/healthz`, `/rpc` and the
two `/attachments/` routes is a separate rule, and its mode signal is the BIND
ADDRESS.** While the live listen
address is loopback it 404s any Host header that is not a loopback name
(`loopback.HostHeader` accepts only `127.0.0.1`, `localhost`, and `::1`, and
refuses every DNS name — including one that resolves to 127.0.0.1, which is the
case it exists for). It read the origin allow-list's emptiness until wave 8d, and
that was the wrong signal twice: a boot honouring a PERSISTED LAN preference set
no patterns, so every LAN client got a 404 until the user toggled the setting
again, and adding a canonical domain's origins would have switched the guard off
for every OTHER name as a side effect of naming one.

A configured canonical domain (`Config.CanonicalHost` / `SetCanonicalHost`) adds
exactly ONE accepted name and stays INSIDE the guard rather than switching it
off. That is what naming a backend buys: it answers to its name — including
through a proxy on this machine that terminates TLS and forwards to the loopback
bind, which sends the domain in the Host header — while every other DNS name is
still refused. It is a host admission and never an authorization; every
credential check downstream is unchanged.

An auxiliary listener adds its own names the same way
(`SetAuxiliaryHosts` / `AuxiliaryHosts`): the tailnet node's MagicDNS name and
its tailnet addresses, set only once a listener exists on it and cleared the
moment that listener goes away, so the guard never admits a name nothing is
serving. Both lists are compared after `hostLabel` folds the header to one
spelling (port stripped, IPv6 brackets removed, lowercased), because a Host
header is written by the client and `Node.ts.net:443` is the same name as
`node.ts.net`. Same rule as the canonical domain: names go INSIDE the guard,
and the guard never switches off.

**One page origin is admitted that no request can derive: the phone
shell's** (`shellorigin.go`, `ShellOrigin` =
`https://shell.agent-overflow.invalid`). The shell serves the SAME bundle
from a fixed origin of its own and reaches the backend across a network,
so every request it makes is cross-origin by construction rather than by
configuration — there is nothing per-install to derive it from. The
constant is safe to hard-code precisely because `.invalid` is reserved
(RFC 6761 §6.4): no resolver answers it and no registry sells it, so no
page on any network can hold that origin, and the shell has it only
because Capacitor assigns its WebView document that authority locally. A
pattern, a setting, or an "any https origin the owner adds" knob would
each be a wider door than the one string that cannot be reached. The
constant is mirrored in `mobile/capacitor.config.ts`; change one and the
other stops working.

`shellOriginAllowed` is the ONE place that answers it, and both
`OriginAllowed` (which the `/ws` upgrade runs) and the CORS middleware
call it — so "may open a socket" and "may read the answer" cannot drift
apart. `AO_SHELL_ORIGIN_EXTRA` names one additional admitted origin and is
HARNESS-ONLY: `e2e/tests/compact-shell-origin.spec.ts` serves the bundle
from a throwaway server on an ephemeral port, which no fixed constant can
predict. Nothing in a shipped build sets it and no setting writes it; it
is an env var rather than a config field because a config field is a knob
somebody can turn on in a running install.

**The routes the shell fetches cross-origin answer CORS for that ONE
origin, through one middleware** (`withShellCORS`, composed in
`buildHTTPServer`): `/bootstrap.json`, the five `/auth/*` routes, and the
two `/attachments/` routes. Four rules, each of which is what would
otherwise go wrong:

- **The origin is echoed exactly, never `*`.** A wildcard would let every
  page on the internet read whatever a ticket in a URL authorizes.
- **`Vary: Origin` is stamped unconditionally**, including on the
  same-origin answer that carries no allow header at all — the header set
  varies by origin even when it varies by becoming empty, and a cache
  keyed without it would hand one page another's permission.
- **`Access-Control-Allow-Credentials` is never written.** A shell page
  holds no cookie here and presents its session in a header. The flag
  would invite browsers to attach ambient credentials to exactly the
  routes `OriginAllowed` exists to protect, and it is what makes the
  wildcard ban a rule rather than a habit.
- **A foreign origin gets nothing added** — not a refusal, not a different
  status. The route answers as it always did and the browser withholds the
  body, so the middleware cannot be asked which origins a backend knows.

Two mechanics worth knowing before touching it. The middleware is composed
OUTSIDE the rate limiter: a preflight carries no credential and does no
work, and a throttled preflight fails in a way nothing on the page can
explain. And the two `/attachments/` patterns are METHOD-QUALIFIED, so the
mux answers 405 to an OPTIONS request and a browser reads that as a
refused preflight — each therefore registers its own `OPTIONS` pattern
(`AttachmentDownloadPreflightPath`, `AttachmentUploadPreflightPath`), with
its own `internal/surfaces` row. `/ws` is deliberately absent from all of
this: a WebSocket handshake is not subject to CORS, and `OriginAllowed` is
the whole admission there.

**The device proof binds (method, PATH) and must keep doing so.**
`internal/identity/deviceproof.go`'s `boundTo` compares `r.URL.Path`, never
an absolute URL, which is what lets a shell's cross-origin request sign
byte-identically to a browser's same-origin one. Widening that comparison
to an origin would fork the client into two exchanges that agree until one
of them moves.

**Peer locality is `loopback.PeerAddress(r.RemoteAddr)`**, captured before the
upgrade and reused for the host-tooling receiver refusal, the host-presence
half of the step-up proof,
error-text redaction, permessage-deflate selection, asset cache headers, the
local-channel cookie plant on `/bootstrap.json`, and the manifest's `Remote`
field. `internal/app`'s `bindingAdmitsPeer` calls the same predicate on the
presentation side. It reads the kernel-reported
peer address, never a header, fails closed on an empty or unparseable one, and
carries the same same-host-proxy caveat as above.

Both predicates live in `internal/loopback` alongside the two endpoint-URL
classifiers. They are deliberately different rules and the package doc says why;
do not swap one for another because the names look interchangeable.

## Attachment bytes do not ride the socket

`attachmentroutes.go` (wave 6b). Two routes, both reached by a ticket and
nothing else:

| route | ticket subject | answers |
|---|---|---|
| `GET /attachments/{threadID}/{attachmentID}` | that exact pair | the bytes, via `http.ServeContent` (Range and conditional handling come free) |
| `PUT /attachments/upload` | thread + filename + content type + exact size | the created row as JSON |

**Why they exist.** Every attachment byte used to ride base64 inside one WS RPC
frame, so a 10 MiB image became a ~13.4 MB frame on the socket the live event
stream shares — the one thing the spec forbids outright ("large bodies never
block the event socket").

**No session cookie is required, and that is not a gap.** The ticket IS the
admission, exactly as the WS ticket is on the upgrade. It is minted only by a
bound method the per-call scope gate already authorized
(`MintAttachmentDownloadTicket` / `MintAttachmentUploadTicket`, `internal/app`),
it is spent by the first presentation, and it lives 30 seconds. Because nothing
here is authorized by an ambient cookie, the Origin allow-list is deliberately
NOT applied: it exists to stop a foreign page spending a credential the browser
attaches on its own, and there is none to spend — a foreign page can cause the
request and can neither produce the ticket nor read the answer. The Host guard
IS kept.

**A refusal is 404 in every case** — missing ticket, spent ticket, a path the
ticket does not name, and a seam failure alike. Nothing on the wire
distinguishes them.

**Nothing on the request may widen what the ticket admits.** The download path
is COMPARED against the subject rather than read from. The upload's filename and
content type come from the subject rather than from headers, because a header
would be a caller describing bytes it is in the middle of sending.

**The upload cap is enforced during the read** (`http.MaxBytesReader`), not from
`Content-Length`: a declared length is a claim and a chunked body declares none.
A mismatched `Content-Length` is only an early exit.

**Both routes replace the server's 60s timeouts** with
`AttachmentTransferWindow` (5 minutes) per request, on BOTH halves.
`extendTransferDeadline` sets the read and the write deadline together,
whatever the direction: net/http arms the write deadline at
`now + WriteTimeout` when the request HEADERS finish reading, before the
handler runs, so a slow upload that only extended its read deadline was
killed mid-body by a write deadline armed before its first byte arrived.
The two are independent on `http.NewResponseController` and a direction
that does not need one pays nothing for having it. The 60s default is right for
an RPC and wrong for bytes — 10 MiB inside it demands a sustained ~170 KB/s —
and the same bytes previously rode a socket net/http had already released, so
leaving the defaults would have been a NEW way for a large attachment to fail.
`internal/clientmode` imports that constant for its relay rather than restating
it.

**Resumable upload is deliberately NOT built.** A ticket is spent by the first
request, so a failed upload is retried by minting again. Bodies are at most
10 MiB and the composer compresses images first; resumable transfer belongs to
the phone waves and is a design of its own.

`GetAttachmentThumbnail` stays an RPC. ~10-30 KB is not a large body, and a grid
would pay a mint round trip per tile.

## Computer-to-computer handoff bytes

`thread_transfer.go` owns `/transfers/<operation>/<action>`. The optional
`Config.ThreadTransfers` adapter supplies durable state and authorization. The
adapter is separate from App's bound control methods; no device credential is
exported to another computer. The source presents a single-operation bearer in
Authorization, and the destination rechecks that grant on every request. The
destination additionally compares `X-AO-Transfer-Backend` before mutation;
every reply carries the backend and operation identities for the sender to verify.

Off-loopback calls require TLS. There are no cookies, query credentials or CORS
grants. A separate peer budget bounds grant lookups without consuming pairing or
reconnect budgets. Control bodies are capped at 4 KiB; chunks at 8 MiB with an
exact length, offset and SHA-256. Socket read deadlines interrupt stalled body
reads. The wire DTO/limit/path drift test binds this handler to `transferwire`
and `transferfiles`. Errors use fixed codes; internal details stay server-side.

Both activation and cancellation require the source secret in addition to the
offer grant. Source cancellation intent must be durable before that proof is
sent. This prevents an independent frontend cancel from racing source retirement.
The adapter serializes operations and rechecks durable phases; the transport
never assumes that an HTTP failure means the peer made no change.

## The backend is the phone's update server

`bundleroutes.go` (wave 6g-a), over `internal/bundle`. Two reads, and
between them they are the whole update channel for the one client that
carries a bundle of its own:

| route | answers |
|---|---|
| `GET /bundle/manifest.json` | the SHA-256 manifest: path, digest and size per file, plus the content id and `minShellBuild` |
| `GET /bundle/archive.zip` | exactly those files, deflated, through `http.ServeContent` |

**The credential is the paired SESSION, not the page cookie.** Both run
the check `/bootstrap.json` falls back to — `sessionAdmitsRequest`, which
is a live session credential in `X-AO-Session` plus the device proof its
enrolment bound. Refusing the cookie is a property of the CONSUMER rather
than a hardening choice: the consumer is a page at `ShellOrigin`, which
holds no cookie for this backend and could not be sent one, so admitting
it would only widen the door to something this surface cannot revoke. A
caller naming no session gets the same unfingerprintable 404 an unpaired
remote gets at the manifest — loopback pages included, which is the plain
consequence of the rule rather than an exception carved out for them.

**The tier rule, and where its gate goes.** The spec states that only
OWNER-TIER backends may supply bundles: peer and hub connections never
push executable content, because one misbehaving owner-tier backend can
reach the phone's device key and its other backends' credentials through
an update. Every session this backend can issue today is owner-tier, so
there is nothing to compare and no gate is written. When those tiers
exist the comparison belongs in `bundleSessionAdmits`, beside the session
lookup — one place, not a tier test in two handlers.

**CORS is the shell's, exactly as the attachment routes have it.** Both
patterns are method-qualified, so each registers its own `OPTIONS`
pattern; without it the mux answers 405 to the preflight and the browser
never sends the real request.

**Neither route is rate limited.** Both demand a live paired session, so
there is no request an unadmitted caller can repeat for free, and the one
admitted client asks at most twice per update.

The archive rides `http.ServeContent` so Range comes free (a phone that
lost a 5 MB transfer at 90% resumes it) and `extendTransferDeadline`
gives it the same window the attachment bytes get — it is the largest
body this listener serves. No `Last-Modified` is published: the archive
carries no timestamps by construction, and inventing one would invite
revalidation against a clock rather than against the content id the hello
frame already gave the client.

`Config.Bundle` is nil on a dev-server boot, which leaves both routes
answering 404 and the hello frame silent. A bundle that changes on every
save is not something a phone should stage.

## Per-peer request budgets

`ratelimit.go` gives three routes a token bucket per peer: `/bootstrap.json`,
`/pageurl`, and `/rpc`. **`/healthz` and the SPA assets are never limited** — a
readiness probe is polled by design and one page load is dozens of asset
requests, so limiting either breaks ordinary use to bound work that is already
trivial. `/ws` is not limited either: one upgrade per long-lived connection,
and the budget that matters for it is the ticket exchange that precedes it.
**The two `/attachments/` routes are not limited either**, and the reason is
the same one that lets them run without a cookie: a peer cannot repeat a
request whose admission was spent, and every transfer was authorized one
attachment at a time by a mint that crossed the budgeted socket. A bucket here
would bound the same work twice while adding a way for a busy grid to be
refused.

What this bounds is WORK, not guessing. The launch token is 256 random bits and
scoped tokens are minted per provider process, so no achievable request rate
makes guessing one plausible; what a rate does reach is the backend's own cost,
and on `/pageurl` specifically it evicts tickets other pages are about to
present.

- **The refusal is 429 with `Retry-After`, never the credential channel's 404.**
  The SPA latches terminal `unauthorized` state on a 401/403/404 from the
  manifest and stops its reconnect ladder (§ Credentials and refusal shapes), so
  a 404 here would tell a client that merely burst that its credential is dead.
  Same class of mistake as answering a readiness probe with a 404.
- **Loopback peers are limited on the same terms as everyone else.** Exempting
  them would leave the path unexercised in development and in the e2e suite, so
  its first real run would be its first LAN bind. The budgets are set so a
  reconnect ladder, a Playwright worker, and a scripted CLI never reach them.
- **Separate limiters per surface**, so a burst on one cannot spend another's.
- The table is bounded (`maxTrackedPeers`) and self-cleaning: a bucket that has
  refilled carries nothing a fresh entry would not, so the insert path drops
  idle peers instead of running a sweep goroutine. With the table full of peers
  that are all actively spending, a new peer is REFUSED rather than admitted
  untracked — admitting it would mean not limiting it at all.
- The admitted path allocates nothing: `peerKey` is hand-parsed because
  `net.SplitHostPort` allocates an `*AddrError` on input it cannot read, and the
  bucket is a map VALUE rewritten in place. Two tests pin this
  (`TestRateLimiterAdmitsWithoutAllocating`, `TestPeerKeyDoesNotAllocate`); keep
  them passing rather than treating them as incidental.
- Refusals are logged with per-peer attribution, once per dry spell rather than
  once per request — a flood must not be able to flood the log with its own
  evidence. The capacity refusal has no bucket to mark, so it is throttled by
  time instead.

## Security headers

`wireheaders.go` is the one definition, shared with `internal/clientmode`.
`WriteSecurityHeaders` sends `X-Content-Type-Options: nosniff`,
`X-Frame-Options: DENY`, and `Referrer-Policy: no-referrer`, which keeps the
page ticket out of outbound referers — and, since wave 6b, transfer tickets
too. Cache-Control is deliberately NOT in that set: each route picks its own
policy. The byte routes pick `no-store`, because the URL that fetched them
carried a single-use credential and a shared cache holding the response would
hold an attachment past the one request authorized to read it.
`WriteCrossOriginIsolationHeaders` (COOP, COEP `require-corp`, CORP) is
diagnostic-mode only, gated on `Config.CrossOriginIsolate` from
`diagenv.RendererDiag`: it buys `crossOriginIsolated` and
`measureUserAgentSpecificMemory` at the cost of breaking remote images in chat
markdown, so do not make it default.

## Scoped tokens (the `ao` CLI surface)

`scopedtoken.go` plus `httprpc.go` add a second, strictly narrower credential
class for the `ao` CLI (spec §5, D15). Not a second API: the same dispatcher,
frame types, and method table serve it. The route is `POST /rpc`
(`ScopedRPCPath`) with a bearer token, loopback-only, 404 for non-loopback
peers, and the server's own session token is not honoured on it. Route and
registry mechanics are in
[docs/architecture/transport.md](../../docs/architecture/transport.md).

- **Route.** `POST /rpc` (`ScopedRPCPath`), `Authorization: Bearer <token>`,
  one `ClientFrame` in and one `ServerFrame` out. A CLI process makes one call
  and exits, so it gets a POST rather than a WebSocket with a replay ring.
  Loopback-only, and a non-loopback peer gets a 404 so the route stays
  unfingerprintable. The server's own session token is **not** honoured here —
  this surface can never be wider than the table below, however it is reached.
- **Registry.** The app owns it (`App.aoTokens`, mutated only from the session
  registry in `internal/sessionruntime.Manager`); this package consults it through the narrow
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
  every host-tooling method outside the table — is `method_not_found` for a
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
`CallerScopeFrom`, and only for a method whose scope already keeps it out of
the observe tier — `TestScopedTokenMethodsAreNotObserveTier` pins that, and
`TestScopedTokenMethodsExistInGeneratedTable` catches a name that has drifted
off the receiver. `GrantNotRequired`
(`"*"`) marks a method whose authority is entirely row-level, admitting every
scoped token whatever grants the phase froze. It is not a grant: no workflow may
declare it, `def.KnownGrant` does not know it, and a test pins that in both
directions. Use it only for a method that is part of doing the work rather than
an extra capability, which is the case campaign memory makes.

**One method on this route blocks.** `WorkflowAgentWatchRun` holds its request
until the run it names moves, bounded by the app's `maxWorkflowWatchHold` (25s).
`HTTPReadTimeout` and `HTTPWriteTimeout` (60s each) must stay comfortably above
that hold, or the transport would kill a healthy blocked call and every quiet
minute would read as a dead backend. Keep the ordering
`hold < CLI rpcTimeout (30s) < HTTP write timeout` before changing any of them.

## Additional receivers

`Dispatcher.Register` accepts more than one receiver, and the only one besides
`App` is the harness's `Harness` under `--harness`, registered with
`RegisterOptions{LocalOnly: true}` so the whole receiver is refused for
non-loopback peers — the only locality gate left on the dispatcher. Registering
another one has rules (boot-path gating, name collisions, what a receiver with
no scope annotations means):
[docs/architecture/transport.md](../../docs/architecture/transport.md).

## Event-channel policy registry

`event_channels.go` holds `channelPolicies`, one authored row per channel the
app emits: `{Channel, Audience, Retention, Scope, EntityFiltered, Why}`. It
decides all four per-channel questions: who may receive a channel's frames by
ORIGIN, what GRANT a session needs to receive them, whether a connection may
narrow them to the entities it is looking at, and how deep its replay ring is.
It cannot be generated, because emit sites are spread across several packages
and some build their channel name at runtime.

**Adding a channel is two edits**, an `eventchan.Channel` constant in
`internal/eventchan` and a row here. `event_channels_eventchan_test.go`
AST-parses the constants and fails on either half missing its counterpart. The
newtype stops a channel *variable* reaching an emit site without an explicit
conversion; Go would still assign a bare string literal silently, which the root
package's `TestEmitSitesNameAnEventChannelConstant` catches.

- `Audience`: `AudienceAny` / `AudienceLoopbackOnly` / `AudienceRemoteOnly`.

  **`AudienceLoopbackOnly` says who can CONSUME a frame, not how sensitive
  it is.** It is for frames whose only legitimate consumer is a process on
  this host: the launcher's directives (`power:keepawake`, `browser:host`,
  `updater:install`, `webview:trim`), harness tooling, the desktop
  self-updater's own lifecycle, and the native browser pane. Those rows
  carry `host` on `Scope` as well, and the column survives for them because
  it is a THIRD DOOR the scope gate does not close: `host` stops an off-host
  session ARMING a stream, but once a local pane subscribes the push side
  fans out to every subscriber regardless of who armed it.

  It is **not** a disclosure control for thread or workspace state. Since
  wave 6d1 every off-host connection names a session, so `Scope` is the gate
  for that state, and a channel carrying it is `AudienceAny` with the scope
  its pull RPC carries. Every event about a thread or a workspace must reach,
  in real time, any connected client that has visibility of it — a sidebar
  row or an open pane — the phone and the `--connect` browser included (user
  ruling 2026-09-03). `TestLoopbackOnlyIsForHostDirectivesOnly` holds both
  lists by name in both directions.

  **The lesson, and it is the reason nineteen rows were wrong for three
  months: a `Why` that justifies an audience by citing an RPC's
  REACHABILITY goes stale the moment reachability changes.** Those rows read
  "the resolve RPCs are LocalOnly" and "every MCP RPC is LocalOnly"; wave 6d2
  deleted that table and nothing in this file pointed at them, so a phone
  could call `RegisterQueueItem`, `RespondToApproval`, `GetGitStatus` and
  `OpenTerminal` while every matching push was withheld — stale queue rows,
  no live approval prompt, a terminal with no output. Justify an audience by
  the DATA CLASS and by `Scope` instead. Both survive a reachability change,
  because `Scope` is what reachability is now made of.
- `Scope`: the grant a session-carrying connection must hold. **Pick it by
  finding the RPC that reads the same data** — a push must not be a way
  around the authorization its pull half enforces, so `git:status` is
  `git:operate` because `GetGitStatus` is. `ScopeHost` means host presence
  is the only key. It does not replace `Audience`; a connection subject to
  both is narrowed by both.

  **`ScopeHost` on a channel whose whole audience is remote is a feature that
  only works where it is not needed**, and the `service:update-*` pair is the
  worked example. Both carry a supervised host's update story —
  `service:update-status` while the flow runs, `service:update-outcome` from the
  version that comes back — and the peer they exist for is an owner who is not
  at that machine. `service:update-outcome` shipped in 8h1 as `host`, so no
  session could receive it and the only client that could was the one standing
  beside the box; 8h2 moved both to `access:admin`, which is what
  `GetServiceUpdateStatus` (the read that answers the same fact) carries.
  `TestChannelScopeMatchesItsReadRPC` pins both rows and the reason.
- `Retention`: `RetentionDefault` (full ring) / `RetentionEphemeral`
  (capacity 0) / `RetentionLatestOnly` (capacity 1). Class-level doctrine,
  including the unkeyed membership rule for latest-only, lives on the constants.
- `EntityFiltered`: whether a connection that has named the threads it is
  looking at stops receiving this channel's frames for the others (see
  *Narrowing a connection to what it is looking at* below). The membership
  rule is on the field: HIGH-FREQUENCY PAYLOAD CARRIERS whose only consumers
  render the named thread and whose absence degrades to a slower correct
  path. Low-frequency thread-keyed channels stay wildcard deliberately — the
  bytes saved would not pay for the reasoning every future consumer of that
  channel then owes.
- `Why`: the decision. A `Why` containing `"unreviewed"` marks a row that
  inherited a default rather than one anyone decided, and
  `TestChannelPolicyUnreviewedWorklist` prints any that appear.

A channel with no row gets the fail-closed default
(`unregisteredChannelPolicy`: loopback-only, full ring) and Emit logs it once at
ring creation, so a forgotten registration degrades to "invisible to remote
clients", never to "leaked to remote clients". The two harness-only emit paths
(`HarnessEmit`, `harness.Replayer`) name caller-supplied channels and are
unregistrable by construction; both spell the explicit conversion at the call
site so the escape hatch stays visible.
`TestChannelPolicyPreservesFrozenClassification` freezes every non-default
classification, so changing one of those lists is a behavior change, not a
refactor. Its loopback-only list is now the host-directive residue only, and
`TestLoopbackOnlyIsForHostDirectivesOnly` is its two-sided companion: one list
of state channels that must stay `AudienceAny`, one of host-directive channels
that must stay loopback-only, and a sweep that fails on any third loopback-only
row appearing on neither.

**A write path that persists and answers only its caller is a channel that
was never added, and the registry cannot see the hole.** Every test here
asks whether an existing row is right; none asks whether a row is MISSING,
because the missing half is a Go write path in another package with nothing
to parse. Eleven of them were found by reading write paths rather than rows
(wave 2026-09-03): worktree cut and attach, `OpenTerminal`, keybindings,
chat-bar favorites, review comments, new-thread defaults, discussion
definitions, provider-account listing, backend add/remove/rename, the editor
preference and session import all persisted and returned, and no other
connected client learned of them until reload. So the question belongs at
the write, not here: after a write persists, name the clients that can SEE
what it changed and say how each of them learns. The answers are the
ordinary ones — an existing row channel (a thread or project row moved: use
the `broadcast*Row` chokepoint, never a second emit beside it), an existing
state channel, or a new row here.

**Opening a channel to remote clients decides the RECEIVE side only, and
whatever PRODUCES that channel must be re-checked in the same change.** A
channel whose frames drive UI on every attached client is a steering primitive
the moment a client can also emit it, so the row's audience and the producer's
`//ao:scope` annotation are one decision spread across two files — and nothing
in either file points at the other. Pin the pairing with a test that reads both,
as `TestNotificationChannelsReachRemoteButStayHostProduced` does for
`notification:activated`. Registry lookups stay keyed by plain `string`, because each is reached
by a channel name that came off the wire at least some of the time and the
newtype would assert a registration nobody checked.

`event_visibility.go` filters pushed events per connection origin, deriving its
two sets from those rows at init, so change a row rather than editing them. A
non-loopback connection receives only `remoteVisibleEventChannels` (the
`AudienceAny` and `AudienceRemoteOnly` rows). One thing the rows do not say: a
remote-only channel's producer is ALSO gated on `Server.HasRemoteClient()`, an
atomic count of non-loopback WS connections, so no work happens when only
loopback clients are attached. SSH-tunneled remotes arrive as loopback and are
invisible to that probe, so they keep the RPC path.

`EventBus.RemoteReceiverCount(channel)` is the GRANT-aware sibling of that
probe, for producers whose work is only worth doing when somebody who may
actually receive it is out there. `HasRemoteClient` counts non-loopback
connections; this counts the subset whose session also holds the scope the
channel's row names. `devserver:list` is the case it was written for: the
dev-server scan (`internal/app/app_preview.go`) walks `/proc` and dials
loopback ports every three seconds, and a desktop-only install must never
pay for a list nothing reads. Channel SUBSCRIPTION is deliberately NOT the
signal — an SPA subscriber takes every channel by default, so it would
answer yes for the webview sitting in front of the machine — and neither is
a subscriber with no origin or no active scope filter, both of which mean
no session named its grants.

## Events are entity-keyed

A pushed frame is addressed by the entity it describes: a cwd (`git:status`), a
PR key (`pr:updated`), a thread id, a project. Never by the subscription that
happens to be listening. Subscription ids stay legitimate on the RPC result that
hands out the unsubscribe handle (`GitStatusSubscriptionResult.ID`), a
per-caller lease rather than an address.
`TestWirePayloadsAreEntityKeyedNotSubscriptionKeyed` (repo root) fails on any
struct field that serializes as `subscriptionId`, and on the sibling spellings
of the same idea (`subId`, `streamId`, `handleId`, `watcherId`).

It carries exactly one carve-out, and the shape of it is the rule: a byte-stream
MULTIPLEXER numbers its streams because the stream number IS the identity of a
live socket, with no entity behind it and no shared observation being filtered
down. `webview2host`'s CDP tunnel is that; an event that has merely run out of a
better key is not. The exemption is keyed by file so it cannot widen quietly.

Two panes routinely watch one entity, so subscription-keyed frames force each
pane into a private filtered copy and those copies drift: they disagreed about
whether there was anything to commit for minutes at a time before `git:status`
was re-keyed (audit 2026-08-08). The producer is therefore refcounted per
entity, not per caller. N subscribers on one cwd share one
`gitwatch.Subscription`, one goroutine, and one frame per change; pause and
resume compose across them, and fetch errors ride the payload.

## Narrowing a connection to what it is looking at

`event_entity.go` holds the doctrine and the derived set; `EntityFiltered` on a
policy row is the only place membership is declared. A `watch` frame names the
threads a connection is looking at, and `Subscriber.watches` withholds the
filtered channels for every other thread — in `deliver` AND in `handleReplay`,
because a filter that only applied live would hand the whole backlog back on
the next reconnect.

Four properties the code depends on, each with a test in
`event_entity_test.go` / `conn_watch_test.go`:

- **The key is computed once, at emit.** `App.emitKeyed` derives it before the
  bus sees the payload, so N subscribers cost one extraction rather than N.
  `Event.EntityKey` rides the ring entry, which is what lets replay filter
  without re-parsing.
- **Fail-open.** An empty key is delivered to everyone. A payload
  `eventscope.ThreadIDFromEvent` cannot attribute degrades to the previous,
  wider behavior — never to a silently missing frame.
- **A withheld frame is not a drop.** The check runs BEFORE the gap accounting,
  so withholding never flags the channel and never mints a `gap:true` marker.
  Reversing that order turns narrowing into a resync storm.
- **Watching is not subscribing.** A `watch` frame must not touch channel
  subscription: the SPA never sends `subscribe`, and `ChannelSubscriberCount`
  is what tells the launcher whether a dedicated bridge is attached.

The client side is the other half of the same decision and cannot be inferred
from these rows — a filtered channel needs an exemption in the client's
forward-skip heuristic, and a thread with a consumer needs a source in
`frontend/src/lib/stores/watchedThreads.ts`. Adding a row means auditing every
consumer of that channel for one that reads it for a thread with no surface.

**That audit is the work, and it is not always a no.** `provider:item_event`
had six such consumers and joined the column anyway, by RE-HOMING them onto
wildcard carriers rather than by widening the filter: the sidebar's Failed
badge onto `thread:error_notice` (a channel that exists for this), its Plan
ready badge and the user_text sidebar bump onto `thread:updated` (a `full`
row and an `updatedAt` patch), and the workspace-change lock onto
`provider:background_tasks_changed`. So the question a candidate row asks is
"what does the off-pane consumer actually need, and what is the cheapest
wildcard frame that says it" — never "can we live without the consumer".

## A paused client is leased down, and only a paused one

`lease.go` holds the doctrine. A `lease` frame states the CLIENT's lifecycle —
`active` or `background` — and a backgrounded connection is served less: its
`highlight:seed` frames are withheld, and its `provider:item_event` deltas are
merged to one frame per (thread, item) per 250ms. Nothing else changes, which
is what keeps turn completions, approvals, errors, thread rows and
notifications reaching a phone whose screen is off.

The rule that decides every question about it: **it is the whole-client
NATIVE lifecycle and nothing finer.** The platform paused the app. Not a
pane, not `document.visibilityState`, not focus, not a minimised window — all
of those keep receiving, for the reason the watch set does not read them
either (a surface that stops receiving renders wrongly the moment it is
looked at). Anything proposing a second, softer trigger for this frame is
proposing off-view work shedding under a new name.

Four properties, each with a test in `lease_test.go` / `conn_lease_test.go`:

- **Active until told otherwise, and it survives nothing.** A connection that
  never sends the frame behaves exactly as before it existed; a reconnect
  starts active and the client restates a non-active state after hello, on
  the same socket as its watch set.
- **Withholding precedes gap accounting**, in `Subscriber.deliver` beside the
  origin, grant and watch filters. A seed a paused client was not sent is not
  a frame it lost, so the channel is never flagged and no `gap:true` is
  minted. The channel is also `RetentionEphemeral` and `EntityFiltered`, so
  the advancing seq neither replays nor trips the client's forward-skip
  heuristic.
- **Seq never goes backwards.** A merged frame carries the LAST merged
  frame's seq, and a client drops anything at or below its channel cursor —
  so a late merge is LOST text, not late text. Two rules produce the order:
  any pass-through on the coalesced channel flushes every pending row first
  (not just its own), and a resumed connection flushes before it forwards.
  The first is also what preserves the frontend's contract that a row's
  deltas land ahead of the `meta` or `patch` that re-states it.
- **The withheld list is for cache warmers only.** A channel qualifies when
  its consumers already have a working path without it — `highlight:seed`
  falls back to the highlight RPC it uses for every late-mounting fence
  anyway. A channel whose absence a consumer cannot recover from is dropping
  frames, not leasing down, however cheap it looks.

The merge decodes `provider:item_event` through a LOCAL shape
(`leaseItemFrame`), because transport must not import triage's store types.
`TestLeaseItemFrameMatchesItemStreamEvent` pins the two by their bytes, so a
field rename fails there rather than shipping merged frames a client ignores.

## Wire frames and the gap marker

Ownership refusals use `thread_moved` or `thread_transfer_pending` with an
optional `transfer: {operationId, backendId}`. The dispatcher recognizes the
transport-neutral `ThreadTransferRef` error interface and supplies fixed public
prose; wrapped errors and local paths never cross this boundary. Older clients
can display the message. New clients may route only to an already attached
owner, never enroll or choose a fallback computer from an error frame.

`frame.go` is the frame catalog: `ClientFrame` and `ServerFrame` document every
type, field, and bound (`MaxReplayChannels`, `MaxSubscribeChannels`,
`MaxRPCParams`, `MaxWatchThreads`, `MaxWatchThreadIDBytes`) beside the decoder.
What a gap means to a client is not.

A client may send six frame types: `rpc`, `replay`, `subscribe`, `watch`,
`lease` and `presence`. `TestClientFrameVocabularyIsFrozen` pins the list,
because each one is a contract two codebases hold — a new type also needs a
word in `frontend/src/lib/transport/frames.ts` and a route in `readLoop`, and
the freeze is what makes that a deliberate edit rather than a compile.

Three of them state something about the CLIENT rather than asking for
something, and they are routinely confused because all three mention threads
or attention. They are different questions with different answers:

| frame | states | the backend changes |
|---|---|---|
| `watch` | which threads a surface EXISTS for | which entity-filtered frames this connection is SENT |
| `lease` | whether the OS has paused this whole client | withholds highlight seeds, merges transcript deltas |
| `presence` | whether this screen is being LOOKED AT, and what it shows | whether an OS notification is RAISED, and nothing else |

## The screen presence frame

`presence.go` is the third of those, and its whole doctrine is that it
changes nothing about delivery. `{focused, threads}`, absolute, replacing the
last frame — not a latch — stored on the `Subscriber` behind one atomic
pointer, and read only by `EventBus.LocalScreenPresence`, whose one caller is
`internal/app`'s `screenIsAlreadyLooking`. Same bounds as `watch`
(`MaxWatchThreads`, `MaxWatchThreadIDBytes`), and a frame past them is a
`bad_params` refusal that leaves the previous presence standing: a truncated
set would claim a thread is off screen when it is not.

Four properties, each with a test in `presence_test.go` /
`conn_presence_test.go`:

- **It is not off-view work shedding, and must never become it.** The only
  outcome it can produce is a toast that is not raised. Nothing may read
  `Subscriber.presence` in `deliver`, in `handleReplay`, in the gap
  accounting or beside the lease, and
  `TestConnPresenceFrameDoesNotNarrowDelivery` is the tripwire.
- **Unattended until stated.** A connection that never sent one — every
  client predating the frame — is not a screen, so the frame is additive and
  the behaviour before it existed is "every notification is raised".
- **Only the LOCAL screen counts.** `LocalScreenPresence` ORs over
  subscribers on a LOOPBACK origin, because the backend interrupts the
  machine it runs on: a phone the owner is staring at must not silence the
  desktop in front of them. The WSL deployment is included by that rule
  rather than excepted from it — WSL2 forwards the distro's `127.0.0.1:<port>`
  to the Windows host's localhost, so the launcher's WebView2 arrives on the
  loopback interface and `loopback.PeerAddress` answers true, the same fact
  `handleWS` already admits the launcher's own sockets on.
- **It dies with the socket.** No teardown to remember: a closed laptop lid
  stops being a screen because its subscriber left the bus.

`gap:true` means "your replay seq fell outside the in-memory ring, re-fetch
through the list endpoints". It is a resync instruction rather than a late
event: a cursor can fall outside the ring at either end, and an above-head
cursor produces a marker whose seq is LOWER than the client's own. Clients must
honour it before their own dedup check and reset the channel cursor to the
marker's seq in both directions (`wsClient.handleEventEntry`). Both ends, the
retention interactions, and the client-side forward-skip detection are in
[docs/architecture/transport.md](../../docs/architecture/transport.md).

**A channel with NO RING answers a non-zero cursor with a marker at seq 0,
and silence is not an option there.** Rings are created lazily at the first
`Emit`, so "no ring" after a backend restart means the cursor belongs to the
previous process's sequence space — and every frame the new process is about
to send would be dropped by the client's dedup check until the new sequence
overtook a cursor that could be arbitrarily far ahead. `Replay` skipped such a
channel entirely, which is silence that reads as "nothing was missed" and
costs the channel for the rest of the session. Seq 0 is below every seq this
bus can mint (`Emit` pre-increments), so the marker resets the cursor and the
next live frame passes. A ZERO cursor still gets nothing: it asks for nothing.
The general rule for anything answering a replay: a cursor this process cannot
place is answered with a reset, never with silence.

## Code generation

`methodgen/` emits TWO files from ONE scan: `methods_gen.go`, the
`MethodMeta{Name, ID, Scope, Route, StepUp}` table, and
`frontend/src/lib/transport/methodRoutes.ts`, the client's copy of the Route
column. Run `make methodgen` and commit both; `TestMethodsGen_InSync`
bytes-diffs a fresh run against each committed output, so a new exported `App`
method without a regeneration fails CI. One command for both halves is the
point — a second command to run is a second command to forget, and the failure
would land on whoever next touched a file they had no reason to think was
stale.

It reads the scope vocabulary out of `scopes.go` and the route vocabulary out
of `routes.go` by AST rather than restating either, for the reason it parses
`internalmethods.go` the same way: this tool cannot import the package it
generates into, or a broken `methods_gen.go` would stop the very run that fixes
it. That makes both files generator INPUTS — see the manifest paragraph below.

That gate is only as good as its cache key, and for a long time it was not
good at all. `go test` keys a cached result on the files the TEST PROCESS
opens; this test opens none of `internal/app`, because it shells out to
`methodgen`. A cached PASS therefore stood over source the test never
looked at, and two newly exported `App` methods reached a green six-gate
run undeclared (2026-08-30). The generator now writes an input manifest
(`-inputs`) and the test opens every path in it — **files for their
content, and their directories for the entry list**, since only the second
notices a method declared in a file that did not exist on the cached run.
If `methodgen` ever grows an input, add it to `writeInputManifest` in the
same change, or the gate silently stops covering it.

A `receiverSpecs` entry's `Package` and `TypeName` are the FQN labels a method
hashes under, not facts about where the code lives, so a service promoted into
`internal/<pkg>` keeps `{Package: "main", TypeName: "App"}` and its IDs never
move (`docs/architecture/root-decomposition.md` § Wire compatibility). Adding a
spec widens the production allow-list and gives every method on it a scope
row the per-call gate reads, which is why `Harness` is deliberately not one:
receiver-level `LocalOnly` says no grant reaches host tooling at all.

### The Route column

Scope answers "may this caller do it". **Route answers "on which machine"**
(`routes.go`; `docs/specs/remote-access.md` §10, "Routing an RPC"). The two are
independent: a call routed to the thread's backend is still gated there by its
own scope, and `home` carries no authority of its own.

**Nothing on this side routes anything.** One connection is one backend, and
every RPC that arrives on it is for the backend that answered it. The column
exists for the CLIENT, which holds one `WSClient` per attached backend and has
to pick one before it writes a frame. It is declared here anyway, beside the
scope it ships with, so a method's two classifications are one decision on one
screen — and it crosses to the client as generated code, never as a second
hand-kept list.

| route | the call belongs to |
|---|---|
| `thread` | the backend that owns the thread named by the first non-context parameter |
| `project` | the backend that owns the project named by the first non-context parameter |
| `workspace` | the backend that owns the project named INSIDE the first non-context parameter, a `gitapp.WorkspaceRef` |
| `home` | the backend that served the page |
| `selected` | the backend the composer is pointed at |
| `all` | every attached backend, answers merged by the client |

**Three routes are inferred, and it is not a naming convention that makes them
so.** Thread ids and project ids are minted unique across BACKENDS
(`internal/entityid`), so a client holding one names its owner with no other
help; every other id in the tree is unique within one database. So `methodgen`
reads `thread` off a first parameter named `threadID` and `project` off
`projectID` (case-insensitively, because the same parameter is spelled
`threadID` in one file and `threadId` in another, and a route that changed with
the spelling would be a routing decision made by a typo). The third is read off
a TYPE rather than a name: a method whose first non-context parameter is a
`gitapp.WorkspaceRef` routes `workspace`, to the backend that owns the
`projectId` inside that ref, and needs no route line: the ref already carries
the only id that can answer the question, and a workspace path on its own means
nothing off the machine that holds it. Everything else **declares
`//ao:route home|selected|all`**. Unrouted fails the run listing
every offender, the same fail-closed shape as unscoped: a method nobody routed
is one a multi-backend client answers from whichever socket happened to be
first, which is a wrong answer that looks like a right one.

An explicit directive OVERRIDES the inference, and the browser companion is
what it is for: `BrowserCompanionPaneAttach(threadID, ...)` names a thread and
is still `home`, because the pane is a native view on the page's own machine
whatever thread it is showing.

**`home` is not a synonym for "host-scoped".** It is the honest answer for a
method that acts on the backend serving the page — its settings, its access
admin, its provider accounts, its in-app updater, its `ui_state` bucket — and
it is also where the methods keyed by an id that is neither a thread nor a
project land today (terminal ids, workflow item ids, subscription ids,
attachment ids). Those are NOT settled: the client resolves such an id through
its entity index, and the route column has no word for that. `home` is the
fail-closed placeholder, and the list is in wave 7a's report; a method that
grows a thread-id parameter should take the inference instead.

The service-update trio (`GetServiceUpdateStatus`, `ListServiceReleases`,
`RequestServiceUpdate`) is the deliberate exception, and it is `selected`
rather than `home` because of what it names: a MACHINE, and specifically one
of several a client may have attached. Routed `home` from a desktop with three
backends attached, "install this release" would land on whichever one served
the page, which is not the one the person was looking at. The in-app updater
above it stays `home` because it updates the process serving the page and
nothing else can.

## Per-connection policy and keepalive

`connProfile` (conn.go) captures transport policy at upgrade time and is
immutable for the connection's lifetime. Every connection coalesces events
(`DefaultCoalesceWindow` 16ms, `DefaultCoalesceMaxEvents` 50) into one
`type:"batch"` frame, loopback included, because the receiving webview pays per
message; single-event windows fall through to `type:"event"`. Non-loopback
connections additionally get `permessage-deflate` with context takeover, about
1.5 MB per connection, since bytes are free on a local pipe and CPU is not.

`connProfile.client` (clientidentity.go) is the calling screen's identity,
parsed once from the upgrade URL's `did` / `conn` query parameters and reachable
from any bound method as `transport.ClientFromContext(ctx)`. A bound method that
takes a leading `ctx context.Context` reads it there; the generated TS bindings
STRIP that parameter, so adding one changes no wire signature and no call site.

- `DeviceID` (`did`) is durable per browser profile and SHARED by that profile's
  tabs. It is for attribution — "edited on <device>" — and must never be used to
  suppress a client's own echo: two tabs of one browser would each suppress the
  other's writes and sit on stale state.
- `ConnectionID` (`conn`) is minted per page load and is the echo-suppression
  key.
- Both are empty for an in-process call, a background saga, or a test. That is a
  normal answer meaning "no screen behind this write", not an error, and the
  frames such a write produces are applied by every client.

Identity rides the upgrade URL rather than a post-connect handshake frame
because it must be readable before the FIRST RPC on the connection: a write
issued in a pre-handshake window would broadcast unattributed and echo back into
the surface that made it.

The keepalive loop (conn.go `keepalive`) defaults to a 10s cadence and a 10s
pong timeout, both overridable through `Config.KeepaliveInterval` and
`Config.KeepalivePongTimeout`, which are test knobs, not production tuning.

- A client-visible `{type:"ping"}` frame every interval. The SPA's stale-socket
  watchdog (`wsClient.ts STALE_TRAFFIC_THRESHOLD_MS`) is three heartbeat
  periods, so keep the two constants in ratio when changing either. It makes no
  silence verdict while `document.hidden`: browser engines may throttle both
  its interval and WebSocket message delivery then, and visibility resume gives
  the connection a fresh threshold before judging it again.
- Every third tick also round-trips a protocol ping, and a missed pong convicts
  only when the reader was parked in `ws.Read` with no recent frame (`inRead`
  plus `lastReadAt`).
- Every wire write goes through `writeRaw` under `writeTimeout` (30s), so a peer
  that stops draining cannot wedge the event pump while holding `writeMu`.
- Every close logs one line, graceful closes included, with peer address,
  duration, and `closeReason`. That duration is what the client's reconnect
  ladder judges itself on, so keep it in the line. A connection torn down by a
  revocation reports `session revoked` rather than the context cancel's
  `server shutdown`, because at the error alone the two are identical and a
  revocation would otherwise be invisible in the log exactly when somebody is
  checking whether it took effect.

## Live-session registry and revocation teardown

`SessionConns` (sessionconns.go) maps a durable session id to the WebSockets
currently carrying it. It exists because a revoked row only stops the NEXT
call: a socket that is mid-stream keeps receiving events until something
closes it, and `CloseSession` is that something.

- `Config.SessionForRequest` resolves a request's session BEFORE the upgrade
  and may refuse it. Refusal is `http.NotFound`, the same unfingerprintable
  shape a bad launch credential gets — see § Credentials and refusal shapes.
  The app supplies it from `internal/identity`; a request carrying no session
  credential still proceeds and names none, which is every launch-credential
  client — and the peer rule above then admits such a connection only from
  this machine. It does not REPLACE the launch credential on `/ws`: both are
  still demanded of the clients that always presented both.
- **A `?ticket=` on the upgrade takes precedence over the hook**, and is spent
  whether or not the upgrade then succeeds; that is what single use means. The
  ticket NAMES a session, it does not authorize one: the subject is re-checked
  through `Config.SessionLive` before it is believed, so a ticket for a session
  revoked during the seconds it was in flight cannot resurrect it — **and
  through `Config.SessionAdmitsPeer`, which is the BINDING-CLASS half every
  other presentation path gets for free inside `SessionForRequest`.** The
  ticket arm is the one path that does not go through that hook, so it was the
  one place a `loopback-only` session admitted a peer that is not on this
  machine: a ticket is minted by a request that presented the credential and
  spent by whoever holds the URL, and the mint says nothing about where. The
  rule for any FUTURE arm that resolves a session by id rather than from the
  request: it owes both questions, because neither is implied by the other.
- **`Config.SessionLive` answers a CONJUNCTION**, and this package does not
  and cannot see the second half of it: a session is live only while its own
  row and its DEVICE's row are both unrevoked
  (`docs/specs/remote-access.md` §2). Every consult here — the ticket
  re-check, `watchSession`'s interval, the per-RPC scope read — inherits it by
  going through that one hook. Nothing on this side may cache the answer;
  re-asking is the whole mechanism.
- **An established connection re-validates on an interval and caps its own
  lifetime** (`connHandler.watchSession`), armed only for a connection that
  names a session. The interval covers the two ways a session stops that
  revocation's synchronous teardown does not reach: it EXPIRED, or something
  outside this process revoked it. The lifetime cap forces a periodic
  re-ticket, and **loopback connections are exempt from it** — the local page's
  session is re-minted at boot and travels no network, so capping it would cost
  the webview a visible reconnect and buy nothing. `resolveWatchWindows` holds
  every default and exemption in one function precisely so none of that is
  invisible at the call site.
- `connHandler.closeCause` names why a server-side teardown happened
  (`revoked` / `session no longer live` / `connection lifetime reached`). All
  three cancel the connection context, which at the terminal error alone is
  indistinguishable from a server shutdown — and that is exactly when somebody
  is checking whether a revocation took effect. A new cause is a constant and a
  case, never a fourth atomic three call sites have to remember to read.
- **The attach is followed by ONE more liveness re-check, and that ordering is
  the mechanism.** The upgrade read this session's liveness before the socket
  joined the registry, so a revocation landing in between iterated a registry
  this connection was not in yet: `CloseSession` returned having closed every
  socket except the one that was still arriving. `runConnHandler` therefore
  asks `sessionLive` once more immediately AFTER `sessionConns.attach`
  succeeds, and closes with `closeCauseSessionEnded` when the answer is no.
  Re-checking before the attach narrows the window by an instruction and
  closes nothing; without it the socket streamed under a dead credential until
  `watchSession` next ticked, which for the local page is a minute.
- Registration rides `ConnState.RegisterCleanup`, the same LIFO pass every
  other per-connection resource uses. Do NOT add a parallel teardown: a second
  path could disagree with the first about when a connection ended, and the
  registry would keep closers for sockets that are already gone. When
  registration reports the connection already closing, `runConnHandler` undoes
  the attach itself, because nothing will run the cleanup list again.
- `closeForRevocation` is three steps and the order is the mechanism: close the
  event subscriber (delivery stops at the instant `CloseSession` returns, not
  whenever the read loop notices), cancel the connection context (pump and
  keepalive stop), then `CloseNow` the socket (the parked reader unblocks and
  the ordinary teardown runs). Every step is idempotent, because it races the
  connection's own close.
- `CloseSession` runs its callbacks OUTSIDE the registry lock. A teardown that
  blocked while holding it would stall every other attach and detach in the
  process.
- The registry keeps **no tombstone** for a revoked session, so a later
  connection on that id attaches normally. Refusing it is the database row's
  job (`SessionForRequest` → `identity.Sessions.Live`); a second source of that
  truth would be one that could disagree.
- A connection that names no session attaches nothing. Empty session ids must
  never share a slot — that is every loopback launch-credential connection,
  and one revocation would close all of them.

## Conventions specific to this package

- Wire-bound errors carry only generic prose (`"internal error"`,
  `"bad parameter"`). Full text plus a correlation id is logged server-side, and
  internal panics and file paths must never reach the wire.
  A missing database row (`sql.ErrNoRows`, wrapped or bare) is `not_found`
  with fixed user-facing prose on every origin. It is distinct from an absent
  RPC (`method_not_found`); never make a missing conversation disable a feature.
  `dispatcher_notfound_test.go` pins this classification and redaction.
- A full subscriber buffer drops the incoming event, and the loss is
  ANNOUNCED (`Subscriber.deliver`): the channel is flagged per subscriber, the
  next event that fits on it arrives `gap:true` (re-encoded per subscriber),
  and other flagged channels get standalone `{gap:true, data:null}` markers
  flushed ahead of any later delivery. Client-side seq-skip detection alone
  needed a later same-channel delivery to fire, which a flood starves — a
  subagent fan-out storm left a pane's timeline truncated for 30-40s with the
  connection healthy (incident 2026-08-29). Latest-only channels stay
  unannounced on purpose: the next frame supersedes the lost one.
- `Server.Start` returns when the listener is bound. The HTTP serve goroutine
  surfaces async failure through `Server.ServeErr() <-chan error`.

## References

- Root `AGENTS.md` § "Permanent invariants" for the transport-boundary rule.
- [docs/architecture/transport.md](../../docs/architecture/transport.md) for
  port pinning, gap-marker semantics, coalescing, keepalive rationale, and the
  scoped-token route mechanics.
- `docs/architecture/data-flow.md` for how triage events reach the bus.
- `frontend/bindings/agent-overflow/app.ts` for the generated bindings the wire
  format must keep working, and `frontend/src/lib/transport/` for the wsClient
  and `@wailsio/runtime` shim on the other side of this wire.

`commands.remote.v1` advertises the bounded remote-command RPC contract. Peer
clients verify this flag, protocol and backend identity on the authenticated
WebSocket before dispatch, and never replay mutations automatically. The
session-scoped CLI methods require remote-commands for workflow phases.
`agent-computers:changed` is a latest-only, terminal:operate invalidation of the
selected source computer's opt-in table; it contains no peer list or secrets.
PairAgentComputer is selected-computer access:admin plus step-up and still
requires the destination's ordinary owner-confirmed pairing.


`Config.WaitForActivation` gates the entire HTTP handler during supervisor
trials, including credential, transfer, bundle and WebSocket routes. Only
`/healthz` bypasses it. Wait on the request context and abort disconnected
requests without an auth-shaped response; older clients treated HTTP 503 as
revocation. The gate is supplied by App's existing activation owner. Do not
replace it with a bootstrap-only check: paired clients can mint tickets and
rotate credentials without fetching bootstrap first.
