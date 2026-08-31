# transport/

HTTP+WebSocket wire between the Svelte frontend (the Wails-embedded webview,
`agent-overflow --connect`, or a remote browser) and the Go backend. Mechanism
walkthroughs live in
[docs/architecture/transport.md](../../docs/architecture/transport.md).

## What this package owns

The HTTP listener (embedded SPA, `/bootstrap.json`, `/healthz`, the `/ws`
upgrade, and
`POST /rpc` for the `ao` CLI), the JSON wire frame, token authentication, the
per-connection authorization policy, per-peer request budgets on the credential
surfaces, reflection-based RPC dispatch, a per-channel bounded ring for event
replay on reconnect, and the live-session registry that lets a revocation reach
connections that are already open. Method
IDs are FNV-1a 32-bit of `<package>.<typeName>.<methodName>`, matching Wails'
`internal/hash.Fnv`, so the generated TypeScript bindings keep working. The ring
is in-memory only: a network jitter buffer, not a history store (root
`AGENTS.md` principle 3).

Not owned here: the receiver (the dispatcher takes an `any` and reflects), TLS
(local binds are plain `ws://`, and public exposure goes behind Tailscale Serve,
an SSH tunnel, or a reverse proxy), and where the listen port comes from
(`Config.Port` is injected, never read from a file here).

## Every new App method is also a wire RPC, so annotate it

Adding an exported method to `App` puts it on the wire, so every one carries a
`//ao:scope <name>` directive in its doc comment naming the capability it
exercises — the same comment-directive form as `//wails:ignore`. `methodgen`
**fails the run** listing every unannotated method, and again for a name the
vocabulary does not declare. There is no default and no silence: the generator
is the gate, and it runs before a method reaches the wire at all.

`scopes.go` is the vocabulary — the scope names a session can be granted
(`docs/specs/remote-access.md` §5) plus `host` for a call with no remote form —
and the tier each resolves to: **observe** (`threads:read`, `files:read`,
`settings:read`), **execute**, **host**. It restates `internal/identity`'s
grantable names rather than importing them, since this package stays store-free;
`internal/app` imports both and `TestScopeVocabularyMatchesIdentity` fails in
either direction.

`//ao:stepup` marks the calls §4 requires a fresh per-call proof for: minting a
pairing link, network bind / exposure changes, provider custom-env writes, MCP
config writes, the WSL distro preference, and worktree-setup recipe writes
(stored argv that runs unattended on every worktree cut).
`TestStepUpMethodsAreTheSpecSet` pins that list, because a dropped directive
turns a mandatory proof into an ambient standing grant and nothing else in the
tree would notice.

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
method-error text for a non-loopback peer, and host presence is what a step-up
proof resolves to this phase.

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
- **Step-up goes through `stepUpProven`**, one function whose doc comment
  carries §4's argument. This phase the proof is host presence; phase 5 swaps
  the proof there and no call site moves.
- **Two typed refusals**, following the `ErrCodeGrantRequired` precedent:
  `scope_required` carries the missing scope in `FrameError.Scope` (a FIELD,
  because prose does not survive the wire for a non-loopback caller and a
  client explaining a disabled surface has to branch on something stable), and
  `step_up_required` is its own code because no grant can satisfy it.

**A method's annotation is the FLOOR, not the whole answer.** Authority that
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

It carries `protocolVersion`, `capabilities`, `backendId`, and
`serverTimeMs`.

- **Nothing gates on the version.** Features negotiate through capability
  flags: a client asks "does this backend have X" and degrades on the
  answer instead of inferring from a number (spec §9). `ProtocolVersion`
  moves only for a change that alters what an EXISTING frame or field
  means; adding a frame type, field, or channel is additive and does not
  move it. Additive-only is what makes the swap window — an old bundle
  live against a just-updated backend — safe.
- **`serverCapabilities` is frozen by a test**, and the frozen list spells
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
- `serverTimeMs` is sampled per accept, not cached at boot: the field
  exists so a client can measure its own skew, and a cached value would
  be wrong by the process uptime.

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
  stub dialing its upstream.
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

**There is ONE ticket mechanism** (`ticket.go`), and both users share it. A
`ticketBook` mints a CSPRNG token, hands it out over a channel that is already
authenticated, and lets the FIRST presentation spend it — with a constant-time
compare, an eviction rule, and a bound. The page ticket and the WebSocket ticket
differ in exactly two parameters:

| | subject | deadline |
|---|---|---|
| page ticket (`Credential.tickets`) | none — a launch has one page credential, so the ticket only decides who receives it | none — a URL ticket is produced for a person to open, and a launcher's fixed `?t=` URL must still work an hour later |
| WS ticket (`Server.wsTickets`) | the session id it names | `wsTicketTTL` (30s) — a client mints one immediately before it dials |

Do not add a third implementation. A new single-use token is a new book with
those two parameters set; building it separately means a second constant-time
compare and a second place for "single use" to be got subtly wrong. Because a
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
host-only; session lifetime, matching a per-launch token; and Secure only under
real TLS, since a Secure cookie on a plain loopback bind would never be stored.
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
and its fixture is the one way this package produces a non-loopback peer: the
same `*http.Server` served over a second listener whose accepted connections
report a LAN address, which is exactly what net/http copies into
`Request.RemoteAddr`.

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

`authroutes.go` holds three POSTs (spec §4), the only routes on this mux a
client reaches WITHOUT the launch credential — because they are how a client
that has never met this backend gets one.

| route | what the caller presents | registered when |
|---|---|---|
| `/auth/pair` | a single-use pairing token, plus the thumbprint of the key the device generated first | `Config.AuthEndpoints != nil` |
| `/auth/token` | a rotating refresh secret in the body, its device key in `X-AO-Device-Key` | `Config.AuthEndpoints != nil` |
| `/auth/ticket` | the session credential it already holds | `Config.SessionForRequest != nil` |

Rules that hold across all three:

- **This package owns the wire, not the decision.** `AuthEndpoints` is a
  two-method interface declared here and satisfied by an app-side adapter over
  `internal/identity` — the same direction and the same reason as
  `ScopedTokens`. The DTOs (`PairingRedemption`, `SessionRenewal`,
  `TokenGrant`) are dumb: nothing here validates a token, interprets a reason
  code, or knows what a session row is.
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
- **A proof never comes from the body.** `SessionRenewal.KeyThumbprint` is read
  from `DeviceKeyHeader` and the JSON tag is `-`. A proof a caller may write
  into the same document it is proving something about is not a proof.
- **Origin runs before anything else**, for the reason it does on the bootstrap
  exchange: these routes hand out credentials, and a request another origin
  initiated must never be answered with one.
- **One budget for all three** (`authRateLimit`), because they are alternative
  ways for the same peer to ask this backend for a credential.
- `DeviceKeyHeader` carries a THUMBPRINT this phase, not a signature. It proves
  the client knows which key the device enrolled, not that it holds the private
  half; the spec's end state is a per-request DPoP proof, which needs a signing
  scheme the wire does not have yet. The header is named for the KEY so phase 5
  replaces the value and keeps every call site.

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

## Origin allow-list and peer locality

**`OriginAllowed` (credential.go) gates `/ws`, `/bootstrap.json` and
`/pageurl`, ahead of the credential check.** A request with no `Origin` header is
a client that is not a browser (or a same-origin GET, which browsers send without
one) and passes to the credential and peer rules. A request *with* an `Origin`
passes only when it names the authority this request was addressed to — scheme
from the TLS state, authority from the `Host` header, so the answer stays true
across a rebind, a port change, and every spelling of loopback — or matches a
pattern the LAN bind adds. `internal/network.OriginPatterns` produces those
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

Whether that list is empty is also this package's LAN switch for the
`loopbackHostGuard` on `/bootstrap.json`, `/ws`, and `/rpc`: 404s any Host
header that is not a loopback name (`loopback.HostHeader` accepts only
`127.0.0.1`, `localhost`, and `::1`, and refuses every DNS name — including one
that resolves to 127.0.0.1, which is the case it exists for).

**Peer locality is `loopback.PeerAddress(r.RemoteAddr)`**, captured before the
upgrade and reused for the host-tooling receiver refusal, the step-up proof,
error-text redaction, permessage-deflate selection, asset cache headers, the
local-channel cookie plant on `/bootstrap.json`, and the manifest's `Remote`
field. `internal/app`'s `bindingAdmitsPeer` calls the same predicate on the
presentation side. It reads the kernel-reported
peer address, never a header, fails closed on an empty or unparseable one, and
carries the same same-host-proxy caveat as above.

Both predicates live in `internal/loopback` alongside the two endpoint-URL
classifiers. They are deliberately different rules and the package doc says why;
do not swap one for another because the names look interchangeable.

## Per-peer request budgets

`ratelimit.go` gives three routes a token bucket per peer: `/bootstrap.json`,
`/pageurl`, and `/rpc`. **`/healthz` and the SPA assets are never limited** — a
readiness probe is polled by design and one page load is dozens of asset
requests, so limiting either breaks ordinary use to bound work that is already
trivial. `/ws` is not limited either: one upgrade per long-lived connection,
and the budget that matters for it is the ticket exchange that precedes it.

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
page ticket out of outbound referers. Cache-Control is deliberately NOT in
that set: each route picks its own policy.
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
app emits: `{Channel, Audience, Retention, Scope, Why}`. It decides all three
per-channel questions: who may receive a channel's frames by ORIGIN, what
GRANT a session needs to receive them, and how deep its replay ring is.
It cannot be generated, because emit sites are spread across several packages
and some build their channel name at runtime.

**Adding a channel is two edits**, an `eventchan.Channel` constant in
`internal/eventchan` and a row here. `event_channels_eventchan_test.go`
AST-parses the constants and fails on either half missing its counterpart. The
newtype stops a channel *variable* reaching an emit site without an explicit
conversion; Go would still assign a bare string literal silently, which the root
package's `TestEmitSitesNameAnEventChannelConstant` catches.

- `Audience`: `AudienceAny` / `AudienceLoopbackOnly` / `AudienceRemoteOnly`.
- `Scope`: the grant a session-carrying connection must hold. **Pick it by
  finding the RPC that reads the same data** — a push must not be a way
  around the authorization its pull half enforces, so `git:status` is
  `git:operate` because `GetGitStatus` is. `ScopeHost` means host presence
  is the only key. It does not replace `Audience`; a connection subject to
  both is narrowed by both.
- `Retention`: `RetentionDefault` (full ring) / `RetentionEphemeral`
  (capacity 0) / `RetentionLatestOnly` (capacity 1). Class-level doctrine,
  including the unkeyed membership rule for latest-only, lives on the constants.
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
refactor.

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

## Events are entity-keyed

A pushed frame is addressed by the entity it describes: a cwd (`git:status`), a
PR key (`pr:updated`), a thread id, a project. Never by the subscription that
happens to be listening. Subscription ids stay legitimate on the RPC result that
hands out the unsubscribe handle (`GitStatusSubscriptionResult.ID`), a
per-caller lease rather than an address.
`TestWirePayloadsAreEntityKeyedNotSubscriptionKeyed` (repo root) fails on any
struct field that serializes as `subscriptionId`.

Two panes routinely watch one entity, so subscription-keyed frames force each
pane into a private filtered copy and those copies drift: they disagreed about
whether there was anything to commit for minutes at a time before `git:status`
was re-keyed (audit 2026-08-08). The producer is therefore refcounted per
entity, not per caller. N subscribers on one cwd share one
`gitwatch.Subscription`, one goroutine, and one frame per change; pause and
resume compose across them, and fetch errors ride the payload.

## Wire frames and the gap marker

`frame.go` is the frame catalog: `ClientFrame` and `ServerFrame` document every
type, field, and bound (`MaxReplayChannels`, `MaxSubscribeChannels`,
`MaxRPCParams`) beside the decoder. What a gap means to a client is not.

`gap:true` means "your replay seq fell outside the in-memory ring, re-fetch
through the list endpoints". It is a resync instruction rather than a late
event: a cursor can fall outside the ring at either end, and an above-head
cursor produces a marker whose seq is LOWER than the client's own. Clients must
honour it before their own dedup check and reset the channel cursor to the
marker's seq in both directions (`wsClient.handleEventEntry`). Both ends, the
retention interactions, and the client-side forward-skip detection are in
[docs/architecture/transport.md](../../docs/architecture/transport.md).

## Code generation

`methodgen/` emits `methods_gen.go`, the `MethodMeta{Name, ID, Scope, StepUp}`
table. Run `go run ./internal/transport/methodgen` and commit the result;
`TestMethodsGen_InSync` bytes-diffs a fresh run against the committed output, so
a new exported `App` method without a regeneration fails CI.

It reads the scope vocabulary out of `scopes.go` by AST rather than restating
it, for the reason it parses `internalmethods.go` the same way: this tool cannot
import the package it generates into, or a broken `methods_gen.go` would stop
the very run that fixes it. That makes `scopes.go` a generator INPUT — see the
manifest paragraph below.

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
  revoked during the seconds it was in flight cannot resurrect it.
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
