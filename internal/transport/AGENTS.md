# transport/

HTTP+WebSocket wire between the Svelte frontend (the Wails-embedded webview,
`agent-overflow --connect`, or a remote browser) and the Go backend. Mechanism
walkthroughs live in
[docs/architecture/transport.md](../../docs/architecture/transport.md).

## What this package owns

The HTTP listener (embedded SPA, `/bootstrap.json`, the `/ws` upgrade, and
`POST /rpc` for the `ao` CLI), the JSON wire frame, token authentication, the
per-connection authorization policy, reflection-based RPC dispatch, and a
per-channel bounded ring for event replay on reconnect. Method IDs are FNV-1a
32-bit of `<package>.<typeName>.<methodName>`, matching Wails'
`internal/hash.Fnv`, so the generated TypeScript bindings keep working. The ring
is in-memory only: a network jitter buffer, not a history store (root
`AGENTS.md` principle 3).

Not owned here: the receiver (the dispatcher takes an `any` and reflects), TLS
(local binds are plain `ws://`, and public exposure goes behind Tailscale Serve,
an SSH tunnel, or a reverse proxy), and where the listen port comes from
(`Config.Port` is injected, never read from a file here).

## Every new App method is also a wire RPC, so classify it

Adding an exported method to `App` puts it on the wire. If it touches the local
filesystem, external processes, provider sessions, settings, credentials, or
attachments, add its name to `localOnlyCategories` **with its category** in the
same change.

`internalmethods.go` holds the two filter sets the dispatcher consults:

- `InternalServiceMethods`: Wails framework hooks and `//wails:ignore` methods.
  Never registered, as defense in depth beside the codegen filter.
- `localOnlyCategories`: the privileged surface, one row per method, each tagged
  with the `LocalOnlyCategory` that put it there. `LocalOnlyMethods` (the
  `map[string]bool` the dispatcher and every gate read) is derived from it, so
  the two cannot disagree; `LocalOnlyCategoryOf` answers the reason.
  `Dispatcher.ResolveForOrigin` refuses these from non-loopback peers with the
  same `method_not_found` shape an unregistered method returns, so the
  privileged surface stays unenumerable from the LAN.

`LocalOnlyCategory` is a closed set of ten: local execution, session control,
settings mutation, attachment payload, local-FS bookkeeping, credential and
account enumeration, WSL inventory, MCP state, desktop host control, session
import. `localonlycategory_test.go` parses the constant block out of the source
and fails on a constant with no name row, a name row with no constant, an entry
tagged with an undeclared value, an untagged entry (the zero value is not a
category), and a declared category that classifies nothing. Adding a category is
a deliberate act with a doc comment, not a new number in a comment.

The classification list is the source of truth and method bodies do not re-check
origin. `methods_gen_test.go` fails on a generated method nobody classified
(`TestGeneratedMethods_AllClassified`) and on a classified name that no longer
exists. A reverse proxy on the same host makes remote peers appear loopback and
defeats this locality, so proxy from a different host instead.

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
consuming the page ticket and writing the cookie.

**A page URL carries a one-time ticket (`?t=`), never the session token.**
Tickets are minted per page URL produced — `Server.AppURL` at boot,
`MintPageTicket` for the LAN share URL, the `PageURLPath` route (`/pageurl`) for
every later navigation — and the first `/bootstrap.json` presenting one
exchanges it for the cookie. The SPA then strips it from the URL, and a reload
rides the cookie. Outstanding tickets are bounded (`maxOutstandingTickets`,
oldest evicted), so the newest URL a user just copied is always live. Because a
ticket is spent by the page it was minted for, anything that navigates more than
once asks `/pageurl` for a fresh URL rather than reusing the boot one: the WSL
launcher's reload keybinding, `ao-harness open`/`info`/`attach`/`up`, and the
e2e `HarnessApp.open()`. That route is itself credentialled, and the two
binaries that call it without linking this package restate its path behind a
drift-guard test (`internal/wsllauncher`, `internal/harnessclient`).

Cookie attributes are argued where they are set (`pageCookie`): HttpOnly always,
so page script cannot read the credential back; SameSite=Strict; `Path=/`;
host-only; session lifetime, matching a per-launch token; and Secure only under
real TLS, since a Secure cookie on a plain loopback bind would never be stored.
The name carries the listen port because cookies are scoped by host and path but
**not** by port — two backends on one host (the app and a harness instance, an
app and a `--connect` stub) would otherwise overwrite each other's.

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
upgrade and reused for `LocalOnlyMethods`, permessage-deflate selection, asset
cache headers, and the manifest's `Remote` field. It reads the kernel-reported
peer address, never a header, fails closed on an empty or unparseable one, and
carries the same same-host-proxy caveat as above.

Both predicates live in `internal/loopback` alongside the two endpoint-URL
classifiers. They are deliberately different rules and the package doc says why;
do not swap one for another because the names look interchangeable.

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
`CallerScopeFrom`, and add it to `LocalOnlyMethods` too. `GrantNotRequired`
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
non-loopback peers. Registering another one has rules (boot-path gating, name
collisions, receiver-level versus per-method classification):
[docs/architecture/transport.md](../../docs/architecture/transport.md).

## Event-channel policy registry

`event_channels.go` holds `channelPolicies`, one authored row per channel the
app emits: `{Channel, Audience, Retention, Why}`. It decides both per-channel
questions, who may receive a channel's frames and how deep its replay ring is.
It cannot be generated, because emit sites are spread across several packages
and some build their channel name at runtime.

**Adding a channel is two edits**, an `eventchan.Channel` constant in
`internal/eventchan` and a row here. `event_channels_eventchan_test.go`
AST-parses the constants and fails on either half missing its counterpart. The
newtype stops a channel *variable* reaching an emit site without an explicit
conversion; Go would still assign a bare string literal silently, which the root
package's `TestEmitSitesNameAnEventChannelConstant` catches.

- `Audience`: `AudienceAny` / `AudienceLoopbackOnly` / `AudienceRemoteOnly`.
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
refactor. Registry lookups stay keyed by plain `string`, because each is reached
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

`methodgen/` emits `methods_gen.go`, the static name to FNV-ID table. Run
`go run ./internal/transport/methodgen` and commit the result;
`TestMethodsGen_InSync` bytes-diffs a fresh run against the committed output, so
a new exported `App` method without a regeneration fails CI.

A `receiverSpecs` entry's `Package` and `TypeName` are the FQN labels a method
hashes under, not facts about where the code lives, so a service promoted into
`internal/<pkg>` keeps `{Package: "main", TypeName: "App"}` and its IDs never
move (`docs/architecture/root-decomposition.md` § Wire compatibility). Adding a
spec widens both the production allow-list and the LAN-safety classification
gate in `methods_gen_test.go`, which is why `Harness` is deliberately not one.

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
  ladder judges itself on, so keep it in the line.

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
