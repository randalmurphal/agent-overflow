# Remote Access & Multi-Device

Status: draft, reviewed (robustness / architecture / completeness)
2026-08-04. Owner: Randy. Companion doc:
[remote-access-boundaries.md](./remote-access-boundaries.md).

Goal: any of the owner's devices (desktop, browser, future phone app) can
watch and operate their backend from anywhere; later, teammates watch
shared threads read-only and fork them onto their own machines. This
replaces topology-based trust (`LocalOnlyMethods` refusing non-loopback
peers) with authenticated principals, scopes, and credential binding.

Owner constraints: no paid/SaaS identity dependency; build for the phone
app and team sharing from the start rather than retrofitting; some state
is per-device by design; prefer over-doing it to under-doing it.

## 1. End state

- Backend runs as a long-lived service; the desktop window is one client
  among many.
- One client may attach to several backends; the sidebar groups projects
  under backend sections.
- An always-on personal machine (home server) is a first-class case:
  every other device pairs once over LAN and attaches automatically
  thereafter. No tailnet is required; the tailnet exists for
  off-network reach only.
- Teammates' *backends* peer with ours (federation) holding read-only
  scoped sessions over shared workspaces, and can fork enrolled threads
  to continue locally. Their devices never touch our machine.
- A team server is the same binary in `serve` mode on shared
  infrastructure: workflows + MCP + external triggers supply the
  dispatch-style automation, members attach as principals, and hubs
  peer with other hubs over the same federation protocol as laptops.
- Reference: t3code's environment auth (pairing → token exchange →
  scoped sessions → DPoP → WS tickets), fully self-hosted. We adopt its
  shape, add a user model and a credential-binding axis it lacks.

## 2. Access model

Today, the host-capable RPCs require **two** independent facts: a valid
token **and** a loopback origin. Scopes alone would collapse that to
one, so every own-device session would carry full host capability from
anywhere. Authorization is therefore a product of three axes:

1. **Scope**: what the principal may ask for (§4).
2. **Binding class**: how strongly the credential is tied to a device.
3. **Step-up**: per-call fresh proof for a small catastrophic set.

### Binding classes

| Class | Minted for | Accepted on |
|---|---|---|
| `loopback-only` | embedded webview, WSL launcher relay, `ao` CLI | loopback listeners only |
| `device-bound` | paired devices with a key (DPoP) or passkey | any listener |

Rules:

- **Binding travels with the credential, not the socket.** A session
  ever issued key-bound is never accepted as a plain bearer on *any*
  listener, including loopback. This closes the downgrade where a
  leaked token is replayed on a softer listener.
- **Execute-tier scopes (§5) require `binding ≥ device-bound`.** A
  leaked `loopback-only` webview session therefore carries no remote
  capability at all.

### Principal tiers

Who the credential belongs to sets a hard ceiling no grant can exceed:

| Tier | Principal | Ceiling |
|---|---|---|
| `host` | embedded webview, local CLI (same process tree) | everything, including `scope: host` |
| `owner-device` | the owner's paired devices (desktop, browser, phone) | everything except `scope: host` |
| `automation` | `ao` CLI scoped tokens, workflow phases | the existing frozen grant table, unchanged |
| `peer` | a teammate's backend | observe tier only, restricted to enrolled shared workspaces |
| `viewer` | a shared link | a named subset of observe tier |

### Network-path ceilings

The path a connection arrives on sets a second ceiling, so the same
device is not equally privileged everywhere:

| Path | Ceiling |
|---|---|
| loopback / same host | full (subject to tier) |
| private network: LAN with TLS, tailnet | full (subject to tier) |

These are the only paths. The backend never listens on a publicly
reachable endpoint: tunnel/Funnel exposure was cut entirely (ruled
2026-08-31, §18), so every connection is a device the owner enrolled,
arriving over a network the owner controls.

### Effective scopes

```
effective = granted(device) ∩ ceiling(principal tier) ∩ ceiling(network path)
```

Resolved **once**, at session establishment, into a precomputed set
carried on the connection. Every surface (WS RPC, HTTP RPC, event
push, attachments, and snapshots) authorizes from that one
set (§13). There is no second code path that decides access, so there is
no surface that can drift out of policy.

This is what makes the host's own window and a paired phone different
privileges without maintaining two device records, and what makes a
teammate's backend structurally incapable of holding an execute scope
even if a grant were mis-issued.

### Scope of impact (stated plainly)

The session rows and the signing key live on the same machine, under the
same user, as the provider processes a session can start. Anything that
reaches execute-tier capability locally can mint its own credentials and
rewrite any on-machine record. This system's value is therefore
**gating what happens before that point, plus off-machine
accountability**, not constraining capability the machine already
granted. Design effort goes to the gate; the boundaries doc lists what
is deliberately out of scope.

## 3. Identity model

LANDED 2026-08-31 (wave 5a): migration v75 holds all five entity
families below (the backend row predates it, store v55), accessors in
`internal/store/identity.go`, vocabulary and cross-checks in
`internal/identity`. See §16 phase 2 for the landed/open split.

Entities in SQLite are authoritative data, not cache (see §12):

- **Backend**. One row, minted at first boot: stable UUID + display
  name. Required by deep links, push routing, multi-backend store
  keying, and fork provenance. Mint in phase 1.
- **User**. An account; first boot creates the owner. Multi-user
  arrives with team sharing; schema assumes plurality from the start.
- **Device**. One client instance (this desktop, this browser profile,
  this phone, a peer backend). Label, class
  (`desktop | browser | phone | cli | backend-peer`), platform,
  created/last-seen, key thumbprint and/or passkey credential.
- **Session**. Device → user binding with a scope set, binding class,
  HMAC-signed claims **and** a DB row; both required to verify.
- **Recovery codes**. Minted at owner creation, single-use, offline.
  Cover "new phone, dead laptop, away from home". SSH-to-host CLI mint
  is the last-resort path; document both.

The `ao` CLI's scoped tokens (`internal/transport/scopedtoken.go`)
remain a separate, narrower credential class, unchanged.

## 4. Authentication

### Pairing (universal; works where passkeys cannot)

1. Owner requests a pairing link from an authenticated admin surface
   (desktop settings, or CLI on the host).
2. The **new device generates a keypair first** and presents its
   thumbprint during redemption. Proof-of-possession is universal, not
   peer-only: an intercepted or photographed link is useless without the
   device key.
3. The link is single-use, 5-minute TTL, CSPRNG, token in the URL
   fragment; consumption is an atomic compare-and-set.
4. The minting surface displays a **short verification number** derived
   from the device key; the owner confirms it matches on the new device
   before the session activates. Defeats silent-race interception.
5. Pairing links may carry a scope subset (viewer links, peer
   invitations).
6. Native clients receive the backend's cert fingerprint inside the
   pairing payload (§7) and run the redemption exchange over that
   pinned TLS channel from the first byte. This is required whenever
   the payload form can carry it (QR, link). The **typed-code path** (a
   laptop with no camera enters the short code by hand) cannot carry
   a fingerprint, so its redemption is trust-on-first-use: safe for
   the same reason browser redemptions are (proof-of-possession plus
   the verification number, never channel secrecy), and the
   redemption response returns the fingerprint over the
   now-authenticated channel, pinned from then on.
7. First-ever pairing needs an address before anything is saved: the
   LAN listener may advertise via mDNS (opt-in, off with the listener)
   so a new device picks "found *home-server*" instead of typing an
   IP. Discovery is convenience only. It grants nothing and changes
   no trust step.

LANDED 2026-08-31 (wave 5b), steps 1-5: `identity` pairing over
migration v76, `/auth/pair` as the one wire route, redemption minting
the real credential pair UNACTIVATED with the confirmation gate inside
`Session.Live`, and the verification number as a backend-derived MAC
over (link id, redeeming key). Wave 5c added the owner-facing surface:
eight `CategoryDeviceAccess` RPCs in `internal/app/app_access.go`
(overview / mint / status / confirm / cancel / revoke-session /
revoke-device / restore-device — restore being the remedy the
revoked-key redemption refusal names) and the redeeming client — `#pair=` fragment parse,
`frontend/src/lib/transport/deviceSession.ts` (thumbprint mint, single-
flight ticket renewal, store-before-use rotation discipline), and the
full-page `PairingScreen`. Step 6's fingerprint field is reserved in
the payload and the row (phase 5 fills it); step 7 (mDNS) is not
built.

### Sessions

- **Access tokens are short-lived** (minutes–hours), always
  key-bound for the `device-bound` class.
- **Refresh is rotating with reuse detection**: each renewal issues a
  new refresh secret and invalidates its predecessor; replay of a spent
  refresh forks the family, **auto-revokes the whole family**, and
  alerts the owner. This is how a leaked credential is detected; a copy
  cannot renew indefinitely alongside the real device.
- Refresh binds to the device key on every listener. A bare bearer
  token on its own cannot self-renew.
- Browser class: short TTL, non-renewable without passkey re-auth where
  passkeys are available.

LANDED 2026-08-31 (wave 5b): rotating refresh in
`internal/identity/refresh.go` over `refresh_secrets` (v76). The family
key IS the session id — a renewal extends the session row rather than
minting a new one, so revoke-the-family is exactly revoke-the-session
and every open socket keys on one durable id. Reuse spends the whole
chain FIRST, then revokes. Windows live in `policy.go` (browser
15m/12h, native 1h/30d). The device-key binding is a thumbprint
presented on `X-AO-Device-Key` until phase 5's DPoP proof replaces the
value; browser passkey re-auth gating is phase 5.

Every authentication failure carries a **closed, typed reason code**
end-to-end (missing/malformed proof, key mismatch, time window,
invalid signature, …), with signature checked *before* the time
window so a forged proof is never misreported as clock skew, and one
client-side presentation module mapping codes to actionable hints
("check automatic date & time on both devices" for skew — the
dominant real cause). Adapted from t3code's DPoP-failure rework.
LANDED 2026-08-31 (wave 5a): `internal/identity/reason.go` (the
ordering enforced structurally — `withinWindow` is a method on the
type only the MAC check constructs), `transport.AuthFailure`, and
`authReason.ts`, with gate tests pinning the three sets together.

### WebSocket tickets

The session credential never rides a WS URL. Client POSTs for a ticket
that is **single-use** (consumed on first upgrade), short-lived, and
**key-bound** (redemption requires a DPoP proof). The established
connection re-validates session liveness on an interval and caps its own
lifetime, forcing periodic re-ticket. Per-RPC scope checks still apply
after upgrade.

LANDED 2026-08-31 (wave 5b): one `ticketBook`
(`internal/transport/ticket.go`) behind both the page ticket and the
session-named `/auth/ticket` (30s TTL, spent on the upgrade whether or
not it succeeds, subject re-checked against `SessionLive` so a ticket
cannot resurrect a session revoked in flight). Key-binding rides
`/auth/ticket`'s use of the same `SessionForRequest` hook, which runs
`CheckDeviceProof`; the DPoP proof itself is phase 5. The interval
re-check (60s default) and the 12h non-loopback lifetime cap live in
`conn.go`'s `watchSession`, with the loopback exemption argued at
`resolveWatchWindows`. `/ws` still ALSO admits launch-credential
clients naming no session; migrating them off is phase 3.

### Revocation

Revocation is only real if it reaches live connections. A live-session
registry keyed by session id **force-closes** matching WebSockets and
stops their event streams synchronously on revoke; the in-memory session
table is the per-RPC fast path and is invalidated at the same instant.
No RPC authorizes from state cached at upgrade time.

LANDED 2026-08-31 (wave 5a): `transport.SessionConns` +
`identity.Sessions.RevokeSession`/`RevokeDevice` (device revocation
flips the device and its live sessions in one store transaction), the
generation counter that keeps a slow-path read racing a revoke from
re-caching a dead row, and the `session revoked` close-log attribution.
The revocation RPC and its UI wait for the device-management surface.

### Passkeys (where a real domain fronts the backend, §7)

`go-webauthn`. RP ID = the backend's canonical domain. Registered from
an already-paired session (pairing bootstraps, passkey hardens). Uses:
new-device sign-in without a code, browser re-auth after short-TTL
expiry, and **mandatory step-up** for the catastrophic set (below).
Cross-device (phone signs for a browser via QR) is native CTAP hybrid.
Fallback is always pairing.

### Step-up (mandatory, not optional)

A per-call fresh passkey (or host-presence) proof, never an ambient
standing scope, is required for: minting pairing links, network bind /
exposure changes, provider custom-env writes, MCP config writes, WSL
distro preference, worktree-setup recipe writes (stored argv that runs
unattended with the user's environment on every worktree cut — the
same class as an MCP config write), and remote update triggering (§7).
Optional step-up is theater; these are the calls that re-key the
system, re-route every prompt, or register something the host will
execute.

### Local clients

The embedded webview drops `?t=`: at boot the backend mints an implicit
`loopback-only` device session delivered over the existing fd/stdout
bootstrap. The WSL launcher **forwards that credential** rather than
relying on apparent loopback origin. With topology no longer
authorizing by itself, "looks like loopback" must stop being a trust
basis (a same-host relay can otherwise launder remote peers).

LANDED 2026-08-31 (wave 5b), with one delivery difference: the session
credential rides the existing bootstrap EXCHANGE (an HttpOnly
`ao_session_<port>` cookie planted by `/bootstrap.json`) rather than
the fd/stdout line, because the session core boots after the transport
binds. The page keeps `?t=` — dropping it is the phase-3 migration.
The channel device row (`devices.channel = "local"`, one row per boot
via partial unique index) backs a session re-minted per boot with no
refresh secret. The WSL launcher fetches the cookie over its
authenticated bootstrap exchange and forwards it as a header on every
dial, re-fetching after a refused dial. Wave 5c moved that fetch/cache
into `internal/relaysession` (transport-free so the Windows launcher
can link it; a drift test pins the restated cookie prefix and header
name) and the `--connect` stub now forwards the credential on every
carried upgrade too, marking it stale on any non-101 response.

LANDED 2026-08-31 (wave 6c2): the `?t=` drop itself. Every webview
window loads a BARE page URL (marked `host=webview`) and is handed its
one-time ticket by `ExecJS` injection instead — `internal/pagehost`
holds the marker, the two injected names, and the one rendered script;
`uiwindow.DeliverPageTicket` answers `WindowRuntimeReady` (which the
SPA raises itself, `pageHost.ts`, since it replaces `@wailsio/runtime`)
in all three window hosts: desktop/windowed boots, the Windows
launcher, the `--connect` stub. `/pageurl?host=webview` answers
`{url, ticket}` as separate JSON fields for the launcher. Browsers
keep `?t=` — a URL is the only channel that reaches one. Same
`ticketBook`, same exchange, same cookie; only the delivery moved.

LANDED 2026-08-31 (wave 6d1): a `/ws` upgrade from an off-host peer
must NAME a live session (ticket, `X-AO-Session`, or session cookie —
the carriers `SessionForRequest` already reads); refusal is the
unfingerprintable 404. Loopback peers keep the launch-credential path
BY ADJUDICATION, not as leftover migration: the embedded webview,
`ao-harness`, the e2e rig, the launcher's notification socket, and the
`--connect` stub's carried dial are the host's own processes
presenting the boot secret, which is authentication of the host
itself. The SPA decides the unpaired-networked condition BEFORE
dialing (`manifest.remote` AND non-loopback origin AND no paired
session) and latches a second terminal state, `pairing-required`,
phrased by `connectionRefusal.ts`. Residual, pulled into the
origin-gate deletion as a prerequisite: the bootstrap exchange still
plants the local-channel session cookie without comparing peer
locality against the session's `loopback-only` binding class, so a
share-URL page can name that session on the wire even while the UI
refuses to dial — the UI is stricter than the wire until the binding
class is enforced at presentation.

## 5. Authorization

### Two enforcement tiers, eleven labels

Scope names are the audit vocabulary; the enforced boundary is
**observe vs. execute**, crossed with binding class (§2).

| Scope | Tier | Covers |
|---|---|---|
| `threads:read` | observe | thread/timeline/payload reads, usage |
| `files:read` | observe | diffs, file content, context lines, search |
| `threads:operate` | execute | send/steer/queue/start **in read-only or approval-required modes only** |
| `approvals:respond` | execute | answering tool-use approvals |
| `threads:autonomy` | execute | creating or moving threads into auto / auto-accept-edits / full-access; running workflows & automations |
| `terminal:operate` | execute | PTY create/attach/write/replay, worktree-setup output |
| `git:operate` | execute | git mutations, worktrees, PR surface |
| `attachments:write` | execute | uploads (reads ride payload auth) |
| `settings:read` | observe | settings and preference reads: settings snapshot, keybindings, themes, spinners, chat-bar favorites (added wave 6b — the original ten could not spell a settings read) |
| `settings:write` | execute | user/device-tier settings; host-tier and the step-up set are excluded |
| `access:admin` | execute | device list/revoke, audit read; **minting and network changes additionally require step-up** |

Rationale for the splits: answering an approval authorizes host command
execution, and a thread in `full-access` mode needs no approval at all,
so approval-answering and autonomy changes carry exactly the same weight
as `terminal:operate` and should not share a scope with "send a
message". Provider custom-env re-points every turn's traffic, and MCP
config registers a binary the provider will run; neither is an ordinary
settings write.

The scopes are separate *names* because peers, viewers, and the audit
log need to distinguish them, **not** because the owner's own devices
should be gated against each other.

Default profiles:

- **Owner devices: every scope except `scope: host`, on every device
  class.** Approvals are one-tap everywhere: gating them while leaving
  message-send open protects nothing (injected script that can send a
  message can simply instruct the agent), so the gate would cost daily
  friction for no security. Terminal access from a key-bound native
  client is comparable to an SSH session from a phone and is not
  withheld. The meaningful distinction is **native vs browser**: only
  browsers have a script-execution surface. So narrowing is offered
  per-device (useful for a browser on a shared machine) and never
  imposed by device size.
- **Peer backend**: `threads:read` plus the shared-workspace surface;
  the `peer` tier ceiling makes execute scopes unreachable.
- **Viewer link**: named observe subsets.
- **Automation**: unchanged frozen grants.

### One generated classification table

The four overlapping classifications collapse to one. Bound methods
carry a source annotation (`//ao:scope threads:read`, same mechanism as
`//wails:ignore`); `methodgen` emits a single `MethodMeta{Name, ID,
Scope, StepUp}` table, and **fails the build on an unannotated method**,
replacing the test-only `wireSafeMethods` completeness gate. `host`
becomes a scope *value*, so the "host-only residue" is just
`scope: host`, not a parallel map. `InternalServiceMethods` stays (never
registered). `ScopedTokenMethods` stays (grants are a genuinely
different axis), with a CI cross-check that every entry exists in the
generated table.

`LocalOnlyMethods` becomes **derived** from the scope table on day one
of phase 3 (privileged scope ⇒ local-only), so only one hand-edited
source exists while clients are migrating; the origin gate is deleted
once every client authenticates.

LANDED 2026-08-31 (wave 6a): all 360 bound methods annotated, the
generator gate live, `wireSafeMethods` / `localOnlyCategories` / the
eleven-value category set retired, `LocalOnlyMethods` derived, step-up
set pinned by test. Three facts the annotation pass established, each
a decision the enforcement wave inherits rather than one it may
rediscover:

- **The day-one derivation rule is not reachability-preserving.** 21
  thread/project/discussion bookkeeping mutations (rename, archive,
  pin, read-state, plan/discussion CRUD) are execute-tier by any
  honest reading and were wire-reachable before the table existed. A
  shrink-only `transitionalReachability` override map (43 entries,
  reasons inline) pins today's partition bit-identically; each entry
  is adjudicated and deleted during enforcement, when a session grant
  rather than the origin gate carries the answer.
- **The ten scopes cannot spell a settings or preference read.** Ten
  getters (settings, keybindings, themes, spinners, chat-bar
  favorites, ui_state) sit in `settings:write` only because no
  observe-tier name exists for them. Enforcement either adds
  `settings:read` (observe) or rules that a read through a
  settings-scoped method enforces as observe; until then the override
  map keeps them wire-reachable.
- **`files:read` = observe deliberately reverses the 2026-05 decision
  that locked the diff surface loopback-only.** Twelve workspace-content
  methods stay loopback-only via overrides; unlocking them for
  sessions granted `files:read` is the intent of the scope and lands
  with enforcement as a deliberate reachability change.

ENFORCEMENT LANDED 2026-08-31 (wave 6b): per-RPC scope gate for
session-carrying connections (`transport.AuthorizeSessionMethod`,
grants re-read per call through `Config.SessionScopes`, nothing cached
at upgrade time); typed `scope_required` (missing scope as a wire
FIELD) and `step_up_required` refusal codes following the
`grant_required` precedent; step-up as one `stepUpProven` function
whose proof this phase is host presence and which phase 5 swaps for a
passkey assertion without moving call sites; the argument-dependent
autonomy recheck judging the EFFECTIVE runtime mode (resolved default
at both thread-create paths via a `threadapp.AuthorizeRuntimeMode`
hook, override-else-current-thread-mode on send/steer/queue — an
omitted argument resolving to full-access is an autonomy act);
per-key settings-tier enforcement on `UpdateSettings`; `settings:read`
added (resolving eight overrides, 35 remain). The launch-credential
path is untouched and BOTH gates stay live for session connections
until every client authenticates. Known gap, phase 4: the scope
vocabulary cannot spell "any valid session", so a device-tier-only
settings patch still needs `settings:write` to reach the per-key gate
(stricter than §6, never looser), and `SetUIState`/`DeleteUIState`
keep their overrides for the same reason. Scope refusals are not yet
written to the auth audit log (no transport→identity hook for it).

### Host-only scope (`scope: host`)

Acts on the host desktop or reconfigures the host itself; no remote
form: `OpenInEditor`, `NotificationActivated`, `BrowseDirectory`, WSL
inventory + distro preference, window geometry, UI render-trace and
error-log writes, observability reconfiguration, self-update
download/apply (§7 covers the remote-trigger exception), and any
surviving plaintext credential-retrieval RPC (which should be deleted
outright, not merely restricted).

### Event visibility

Filters become scope-driven: terminal frames require
`terminal:operate`; `provider:account` (billing identity) requires
`access:admin`; approval/queue channels follow their scopes.
Loopback-vs-remote survives only as a transport optimization signal.

LANDED 2026-08-31 (wave 6b): every `channelPolicies` row carries a
`Scope`; a session-scoped connection filters frames through a
per-connection grant mask computed at attach. That once-per-connection
mask is sound because session grants are immutable and revocation
force-closes the socket through the live-connection registry — a
future "narrow a session's grants in place" API must revisit it.
`system:stats` took `threads:read` as its observe floor (push-only
channel with no pull half to derive from).

### Frontend capability model

`isViewOnlySession()` (bootstrap boolean) is replaced by a granted-scope
capability object; the gating files key off the capability they
need — 36 non-test consumers as of 2026-08-30 (24 components, 9
stores, 3 utils), so budget the phase-3 migration for that, not for
the ~15 this section first claimed. Scope-refusal errors are structured and name the required scope,
so disabled-state tooltips are self-describing. **The server never
trusts the client's capability object**. Every RPC re-checks
server-side; hello-frame flags are compat hints, never authorization.

LANDED 2026-08-31 (wave 6c1): `frontend/src/lib/transport/scopes.ts`
answers `hasScope()` from, in precedence order, a paired session's
published grants (which win even on loopback — the upgrade presents
that session), then the local page (an explicit every-scope answer),
else nothing. `host` is answered from presence, never a grant,
mirroring the server gate. The grant list rides the existing
credential-route DTOs (`TokenGrant.Scopes`, always an array — absent
means a backend too old to say and falls back to judging the page,
`[]` means granted nothing). `isViewOnlySession` is deleted; run mode
survives as the process-boot axis only ("whose settings would this
RPC edit"), and the two axes are read together where both apply.
`scopeRefusal.ts` is the one presentation module for
`scope_required` / `step_up_required` (wired through
`userFacingError`), sibling to `authReason.ts`'s credential half; a
Go gate pins the TS scope vocabulary to `transport.Scopes` in order.
Policy note: an unpaired networked page answers "granted nothing"
rather than borrowing the local channel's grants it cannot enumerate
— stricter than the backend would permit, moot once pairing is the
only way onto a networked page.

### Phase 3 closed

LANDED 2026-08-31 (wave 6d2). The local-channel session's
`loopback-only` binding class is enforced at presentation
(`bindingAdmitsPeer` inside the one `SessionForRequest` hook, so
`/ws`, the manifest fallback, and `/auth/ticket` inherit it; a
binding-refused presentation resolves NO session and falls to the
sessionless rules), and `/bootstrap.json` plants the session cookie
only for loopback peers — a LAN share-URL page gets the page cookie
and reaches the pairing prompt with no local channel. On that
foundation the origin gate is DELETED: `LocalOnlyMethods`, the
derivation, all 35 `transitionalReachability` overrides, and the
frozen partition are gone; `AuthorizeSessionMethod` is the one
per-method gate, and receiver-level `RegisterOptions{LocalOnly}` (the
harness) is the only locality rule left on the dispatcher. The 2026-05
diff-surface lock is deliberately open: the twelve workspace-content
methods answer a session granted `files:read` (three ride
`threads:read`), the 21 bookkeeping mutations ride `threads:operate`,
and `reachability_test.go` pins each by name plus the floors (`host`
refused for every off-host session; step-up still demands its proof).
Consequence recorded in `internal/relaysession`: a cross-host
`--connect` stub can no longer borrow the upstream's local channel —
it needs a paired device session.

Posture notes from the deletion, adjudicated: a paired session with
`threads:operate` can run a session import (reads the host's provider
homes) — accepted, that is what pairing a trusted device means. OPEN
product decision: `MintDevicePairing` grants every scope; a per-grant
pairing surface is a later decision, not a phase-3 gap.

## 6. Per-device and per-user state

### Fix the identity hole

`GetUIState`/`SetUIState`/`DeleteUIState` currently take a
caller-supplied `clientID`, a spoofable bearer string. They stop taking
it; the backend derives scope from the authenticated session's device.

### One mechanism, three tiers

- **Host tier stays in `settings.json`**: it configures the backend
  before identity or the DB matter (network bind, port, provider
  binaries + custom env, retention, observability, WSL preference) and
  must be hand-editable when the UI is unreachable. Keeps
  `settings.Service` and its own inline atomic write (`os.CreateTemp`
  → `Sync` → `os.Rename`; the package does not use
  `internal/atomicfile`, despite an earlier draft of this line).
- **User and device tiers live in the `ui_state` table**, which already
  exists for exactly this shape (and already migrated pane layout out of
  settings). User tier = `user:<id>` scope; device tier = `device:<id>`
  scope, with typed validation over the same store. Device rows cascade
  on device deletion. Revoking a device drops its state for free.

  LANDED 2026-08-31 (wave 5c), the device half: the ui_state scope is
  derived from the CONNECTION (the session the upgrade presented →
  `device:<id>`; the client id the RPC parameter used to carry is
  gone). The local page channel deliberately keeps per-screen
  `client:<declared id>` buckets, because every same-host surface
  (embedded webview, WSL relay, `--connect` stub) presents the same
  channel session and one shared bucket would regress multi-screen.
  The user tier and typed validation remain phase-4 work.
- Device tier (defaults per device class; phone ships `lowPowerMode`
  on): `lowPowerMode`, fonts + `fontSize`, `paneDensity`,
  `activityRunWindowRows`, `activityRunDefault`, `streamingEnabled`,
  `diffWordWrap`, `collapseDiffPreviews`, `timestampFormat`,
  `editor.preference`, `backgroundGitFetch`, `projectSortMode`,
  `usagePeriod`, `recentWorkspaces`, plus the six spinner-appearance
  keys the taxonomy wave classified device (display, like fonts and
  motion).

  Adjudicated OUT of this list (2026-08-31, phase-4 design):
  - **Theme** is client-file-resident by design (appearance.json on a
    native client, built-ins + localStorage in a browser) and is NOT a
    ui_state member. The selection is a property of the client
    MACHINE: the `--connect` stub has no DB and a browser has no
    backend bucket of its own, and `theme-system.md` §6.1 already
    landed that argument. `settings.theme` is retired, not relocated.
  - **Window geometry** retiers to HOST: it is the backend machine's
    own window, written by the geometry tracker with no RPC and no
    connection to derive a device from, and the Windows launcher
    already keeps its own per-installation file. The "two desktops"
    fight this line predicted does not exist — a `--connect` window
    persists nothing today.
  - **`remoteEndpoints` retiers to HOST**: the list holds plaintext
    session tokens (its own SECURITY NOTE says so) and the token read
    is already `host`-scoped; a device-tier row would declare a phone
    may edit it. Its storage is redesigned in phase 7 with the
    multi-backend UI, not here.
  - **`backgroundGitFetch` and `editor` retier to USER** (wave 7a
    finding): both are consumed by BACKEND behavior — the unattended
    fetch loop and the editor `OpenInEditor` spawns — and one global
    behavior cannot be driven by a per-screen value. The rule is now
    stated at the tier map: a key backend logic reads is not device
    tier.
- User tier (stored under the reserved `user:default` scope until
  phase 8 introduces real user identities): confirmations,
  commit-message style, textgen routing, hidden models, default thread
  env mode, worktree branch prefix, auto-compact thresholds, GitLab
  hosts, plus the eleven agent-behavior keys the taxonomy wave
  classified user (thinking defaults, prompt overrides, disabled
  tools, cross-session, output style, subagent limits, auto-pin).

Multi-machine convenience without a sync engine: host- and user-tier
settings are per-machine by design (divergence is a feature), but the
settings UI shows which machine is being edited and offers "apply to
all / selected machines" on eligible keys — the client fans the same
write out per backend; no cross-backend settings replication exists.

The **key→tier taxonomy lands in phase 3**, with the scope table:
device-tier writes ride a valid session (they touch only `device:self`),
user-tier writes need `settings:write`, host-tier needs step-up. Phase 4
is then pure storage migration with no scope churn.

LANDED 2026-08-31 (wave 6b), with one deliberate deviation: the
per-key tier gate enforces exactly the three rules above, but the
METHOD floor on `UpdateSettings` is `settings:write`, so a
device-tier-only patch still needs that grant to reach the per-key
gate — the scope vocabulary has no name for "any valid session".
Stricter than this section, never looser; phase 4 either adds a
session-floor value or moves device-tier writes onto their own name.

Phase-4 design decisions (2026-08-31), settled ahead of the storage
waves:

- **One service, tiered storage.** `settings.Service` stays the one
  API and the wire keeps `GetSettings`/`UpdateSettings`; what changes
  is residency. Host tier stays in `settings.json`; user tier is
  backed by `ui_state` under `user:default`; device tier by the
  caller's own bucket (`device:<id>` for a paired session, the
  existing per-screen `client:<id>` for the local page channel).
  `GetSettings` resolves the device slice per CALLER; the settings
  validators keep running on the merged patch regardless of where a
  key lands, and `settings:updated` still announces `{tier, keys}` —
  a device-tier frame prompts each client to re-read and each gets
  its own values.
- **Seeding**: on first boot after migration, the file's user-tier
  values seed `user:default` and its device-tier values seed the
  backend machine's own screen bucket (the embedded webview's
  `client:<id>`); other devices start from device-class defaults.
  Moved keys join `retiredSettingsFieldNames`. Never-overwrite,
  log-and-continue — the pattern `migrateUIStateFromSettings` set.
- **Backend-initiated writers**: `recentWorkspaces` is written on
  thread creation, which is an RPC and therefore has a caller to
  attribute the write to. Window geometry has no caller and stays a
  host-tier file value (adjudication above).
- **The session floor** lands as a scope value meaning "any named
  session" carried by `UpdateSettings` (and the ui_state methods),
  with the per-key tier gate doing all real enforcement: device keys
  pass on session presence, user keys require `settings:write`, host
  keys require step-up. A view-only device changing its own font size
  is the case the floor exists for.
- **One write path per key, closed as a class**: a settings key with
  a dedicated RPC is refused by the generic patch. `network`,
  `claudeCustomEnv`/`codexCustomEnv`, and `remoteEndpoints` already
  are; `workflowPaused` joins (its dedicated RPC enforces
  `threads:autonomy`, and the generic patch demanding host step-up
  for the same act was two answers to one question).

LANDED 2026-08-31 (wave 7a), the storage half: one `settings.Service`,
tiered residency (`internal/settings/residency.go`). The struct and
wire keep every key; the FILE CODEC holds host keys only, the user
tier overlays from `ui_state` `user:default`, and the device tier
overlays per caller through `Service.For(bucket)` (bucket derivation
shared with `uiStateScope`; sessionless in-process callers read device
defaults). Validators run on every write regardless of destination,
and the device overlay re-runs `sanitizeLoadedSettings` on READ, so a
value poked directly into a bucket via `SetUIState` is clamped exactly
like a hand-edited file. Seeding runs once at boot: file values seed
`user:default` and the backend screen's `client:<id>` bucket,
never-overwrite, defaults skipped; moved keys are NOT retired (they
must stay on the wire) — the codec split is the mechanism instead.
`workflowPaused` joined the refused-from-patch class. `settings.json`
writes now happen only when a host key moves. A store-less Service
keeps every tier in the file, which the pre-database boot readers
(bind address, window geometry) depend on. Still open in phase 4: the
session-floor scope value (wave 7b) and device-class defaults (the
seam exists; the class table is phase 6).

## 7. Transport, reachability, TLS

### Multi-listener, one session store

Loopback (webview, CLI), optional LAN bind, optional tsnet listener.
Sessions are valid across listeners **subject
to their binding class** (§2). Local clients never hairpin through the
tailnet, and a soft listener cannot launder a strong credential into a
weaker presentation.

Cross-origin defense is explicit: strict Host allow-list (canonical
domain + known loopback names), Origin / `Sec-Fetch-Site` checks on
`/ws` and every auth endpoint, DNS-rebinding rejection.

Listener and endpoint-advertisement init is **per-listener isolated**:
one integration failing to start (a broken `tailscale` binary on
PATH) degrades that listener only and surfaces its
error — it never takes down the others (t3code shipped exactly this
bug: one spawn defect killed all endpoint advertisement).

### Stable endpoint

The pick-random-once-then-persist half already shipped
(`main_transport_port.go`: `transport-port.json` pin next to
`client-id.json`, engages only when no explicit `--listen` port is
given, falls back to ephemeral and re-pins if the pinned port is
taken, `--reset-transport-port` to clear). What remains is the
**user-fixable setting** surface: today the only controls are CLI
flags. Durable sessions remove re-pairing after restarts; origin
stability keeps browser storage attached.

### LAN access without a tailnet

The always-on home machine is a first-class case, not a degraded one:
enable the LAN listener, pair each device once (QR/code), and durable
rotating sessions plus the stable endpoint make every later attach
automatic. Laptop, phone app, and the machine's own window connect
the same way, no tailnet involved. Tailscale is for
*off-network* reach only, never a LAN prerequisite. The one
platform-imposed limit (constraint 6): plain-HTTP LAN **browsers** are
bearer-only: no passkeys, no service workers. The desktop app, CLI,
and native phone app are unaffected; they hold device keys, are not
subject to browser secure-context rules, and get encrypted TLS with no
domain at all via cert pinning anchored in the pairing payload (see
TLS below). Wanting passkeys in a LAN browser is the one thing that
requires the DNS-01 owned-domain path: real HTTPS on a private
address, nothing publicly reachable. It is an optional upgrade, never a
dependency.

### TLS (in-app termination)

Two supported paths; others are documented escape hatches, not built:

1. **Owned domain + DNS-01**. DNS record → LAN IP (public DNS may hold
   private addresses), Let's Encrypt via DNS-01, backend renews. Real
   HTTPS on a LAN-only path, valid passkey RP ID.
2. **tsnet cert**. LE cert for the node's `*.ts.net` name, MagicDNS
   resolution, direct peer connections.

**Domainless TLS for Go-native clients (pinning via pairing).** The
backend always mints a self-signed cert, and the pairing payload (QR /
code exchange) carries its fingerprint; the desktop attach client and
CLI (Go processes that own their TLS config) pin that exact cert.
Result: encrypted, authenticated TLS on the LAN with no domain, no CA,
and no trust prompts. The pairing ceremony that already establishes
trust also anchors the channel. Rotation rides the session: a paired
client that holds a valid session accepts a signed successor-cert
announcement. The Capacitor phone shell pins too, through its native
WebSocket bridge (§9, constraint 8). Plain browsers are the one class
that cannot pin, so they remain the cleartext-LAN / owned-domain
cases; passkey RP ID still requires the owned-domain path.

Escape hatch: private CA (mkcert-style, manual trust). The chosen HTTPS name is the backend's
**canonical domain**: passkey RP ID, related-origins anchor (max 5), and
the phone app's dial target.

**Termination by someone else's proxy** is the third path people will
try regardless of what we build, so it gets a defined answer rather
than a broken one. Two things break today. `deriveWSURL` derives the
socket scheme from the listener, not the request, so an `https:` page
served through a TLS-terminating proxy is handed a `ws://` URL and the
browser refuses it as mixed content; the fix is honoring a validated
`X-Forwarded-Proto`. And a same-host proxy makes every remote peer
look like loopback, which is exactly the fact `LocalOnlyMethods` reads
as "this is the machine's own window". Both are why the scope table
replaces topology-based trust rather than extending it: with §5's
model, forwarded-header handling is a routing detail. Until phase 3
lands, a reverse proxy in front of the backend is documented as
unsupported, not silently degraded.

### Dev-server preview across machines (the port gateway)

The in-app browser must reach a dev server the agent started on the
thread's host — from any attached UI, over any path. t3code never
built this (its relay case errors with "needs the planned
authenticated preview gateway"; only tailnet-direct works, and only
when the dev server binds beyond loopback). We build the gateway:

- The backend proxies HTTP **and WebSocket upgrades** (HMR) to
  `localhost:<port>` on its own machine; the in-app browser points at
  the gateway when the thread's host is not the local machine. Works
  over LAN and tailnet alike; the dev server needs no
  `--host` flag and never binds beyond loopback.
- **Reachable ports are an allowlist, never arbitrary**: ports the
  dev-server scanner attributed to this thread's sessions, plus ports
  the user adds explicitly. A localhost proxy that forwards anywhere
  reaches every host-local service on the box; this one forwards only
  to declared dev servers. Gateway access requires an execute-tier scope.
- **The gateway is its own origin**: proxied content is
  agent/app-authored and never shares the SPA origin, and the session
  credential is never visible to it — access rides a short-lived
  ticket bound to the gateway origin. Without a domain, "its own
  origin" means its own loopback listener on its own pinned port.
  One interaction to get right, because it makes the Origin
  allow-list load-bearing rather than defense in depth: **cookies are
  scoped by host, not by port**, so a document on the gateway origin
  still has the boot cookie attached to requests it makes to the SPA
  origin — including a WS upgrade, which CORS does not cover. The
  upgrade must therefore refuse any `Origin` outside the allow-list,
  and the gateway origin is never in it. This is the surface that
  reintroduces the hazard `/design/` used to carry, so it inherits
  the posture that route was going to be given rather than starting
  from scratch.
- Detection reuses t3code's proven shape, server-side: enumerate
  loopback listeners (`lsof`/PowerShell), publish only candidates
  whose bounded 1s probe returns HTML or a redirect, cache probe
  results (~15s), poll (~3s) only while something subscribes,
  attribute PIDs to the owning thread's sessions.

### Anywhere access

tsnet embedded (BSD-3, userspace, works in WSL2 without TUN): the
backend joins the owner's tailnet as its own node, and off-network
reach IS the tailnet. There is no public listener and no tunnel
integration — Funnel/cloudflared were cut (ruled 2026-08-31, §18) —
so "anywhere" always means a device the owner enrolled on a network
path the owner controls. Pairing/token/ticket endpoints stay
rate-limited, keyed by **token/account with a global counter across
listeners**.

### Headless serve mode and remote update

`agent-overflow serve` runs windowless; the desktop app attaches.
Service install (systemd user unit / launchd / Windows via the WSL
launcher) with a stated headless credential-storage posture. Keychains
frequently cannot unlock without a login session, so the signing key,
provider credentials, and tsnet state need a defined at-rest strategy
for unattended boot.

**Session lifecycle on an unattended host.** LANDED 2026-08-31
(b809e997). `ArchiveThread` closes the thread's provider session — the
group kill cascades to dev servers and monitors — with a stop-time
re-check against the newest turn's durable `started_at` so an archive
that waited out a send does not kill the session that send just
engaged (`internal/app/app_thread_archive.go`). The reaper's
keep-alive-while-working choice stays (killing quiet-but-working
sessions is rejected doctrine); what an unattended host adds is
**visibility and control, not timeouts**:
`ListRunningBackgroundWork` (wire-safe) reports the cross-thread
inventory, unioning the same three sources `ListLiveBackgroundTasks`
does (the store query, live Codex subagent launches, and the triage
layer's in-memory Codex unified-exec tasks, which exist in no table),
with per-thread unreadability carried in the payload rather than an
error that would discard the rows; `StopThreadBackgroundWork`
(LocalOnly) routes each row through the existing per-kind stop
methods. The tray's 2-second completed-sibling retention is a
live-tray tuning value, not an inventory history; the inventory
reports what is running now.

Update is a genuine availability requirement once the machine is
unattended, and a supply-chain risk if remotely triggerable. Resolution:
download/apply remain `scope: host`; a **remote trigger** exists but
requires step-up **plus** artifact signature verification, and runs
behind a healthcheck-and-auto-rollback watchdog that preserves listener
config and the session store. A bad update must never lock the owner
out of a machine they cannot physically reach.

The watchdog adopts t3code's proven architecture (its
`server-updates` internals doc), which is concrete where "watchdog"
is vague: a separate, stable **supervisor** process owns the launch
state — the running server never mutates its own launch config; the
new version installs into an immutable staged dir and its
compatibility with the installed supervisor is checked *before*
anything is touched; the store is **snapshotted while quiescent**
before migrations; the new version boots fully as a trial — runs
migrations, binds listeners, starts everything — but parks at an
activation gate until it reports prepared within a hard time budget;
only then does the supervisor durably commit. Failure or timeout
restores the snapshot and restarts the old version, with a durable
restore marker so a supervisor crash mid-rollback resumes correctly.
The update carries an id the client correlates through its reconnect,
so "update succeeded" means the new version answered, not that the
old one stopped.

### Provider accounts and remote login

Provider credentials live in each backend's provider homes, so
accounts are a **per-machine fact**: configured per machine (account
dropdown scoped to the machine, usage keyed per backend), and the
composer's target picker shows which account a thread will run and
bill against (§10). All account management works over the wire —
switching the active account *and adding a new login remotely*.

Provider OAuth redirects to `localhost` **on the host**, unreachable
from a phone, yet provider logins die at inconvenient times (see the
2026-08-03 credential-death incident chain). Without a remote path, one
token rotation bricks the backend until the owner is physically present.
Required: provider auth state is a first-class remote-visible signal
with a push event, and login/re-auth is completable remotely: the
backend surfaces the authorize URL to the authenticated remote client
and proxies its own loopback callback (or relays the
paste-code/setup-token flow). If any provider makes this impossible,
that limitation is documented explicitly rather than discovered in the
field.

## 8. State sync completeness

Prerequisite sweep, valuable standalone:

- Emit on every persisted mutation. Thread-row RPCs LANDED
  2026-08-31 (9d48ee7c): every persisted thread-row mutation
  broadcasts `thread:updated` carrying the written row plus an action
  (`full`/`patch`/`listed`/`unlisted`/`deleted`, constants in
  `internal/triage/router.go`); the store's write helper reads the row
  back inside the write transaction and reports no-op writes so
  repeats stay silent; the broadcast row is also the RPC's return
  value, so initiator echo equals optimistic apply
  (`frontend/src/lib/stores/eventsThreadRows.ts` is the applier).
  `settings:updated` and `project:updated`: LANDED 2026-08-31 (wave
  4b). Settings frames carry the tier plus changed KEY NAMES, never
  values (the redaction GetSettings applies must not have a push-side
  bypass; receivers re-read); one frame per tier moved; the write
  chokepoint is `internal/settings/mutate.go` with an AST tripwire, and
  the tier map (`tier.go`) is total by test. Project frames reuse the
  thread action vocabulary through the generic `internal/store/rowwrite.go`
  helper; two tripwires classify every projectapp method and hold
  emit-on-write.
- `draft:updated`: LANDED 2026-08-31 (wave 4b). Emits ride the persist
  (autosave no-ops stay silent via the upsert's change-tested ON
  CONFLICT), the frame names the writer (`transport.ClientIdentity`,
  `did`/`conn` on the WS upgrade URL, readable before the first RPC)
  and never the text; echo suppression keys on the CONNECTION so two
  tabs of one browser do not sit on each other's stale text. The
  channel is loopback-only, matching GetDraft's classification. The
  "edited on <device>" affordance remains cuttable polish.
- Queue: LANDED 2026-08-31 (wave 4b), with the brief's premise
  corrected — `GetThreadLiveState` already bootstrapped queue state on
  every authoritative attach, so no second bootstrap was added. What
  landed: `GetQueueState` is the targeted gap-recovery read for
  `provider:queue_state_changed`, `queue_restored` takes the full pane
  refresh, and the two unrecoverable badge channels say so explicitly.
- Races: LANDED 2026-08-31 (wave 4b). The triage router arbitrates
  concurrent approval/user-input answers on positive evidence and
  releases the claim when a write never reached the provider; losers
  get the typed `already_handled` transport code, which the composer
  treats as answered-elsewhere. Fixing this surfaced a live defect:
  the benign-race filter matched error STRINGS, which dispatcher
  redaction blanks for every non-loopback caller, so remote clients
  saw error banners where the desktop saw nothing. "Answered on
  <device>" live flip is not built (needs the attribution UI).
- **Device attribution**: LANDED 2026-08-31 (wave 4b) as CREATION
  attribution — v73 `threads.created_by_device`, write-once by the
  `import_source` mechanism, empty = the backend created it. Mutation
  audit is a log table and its own decision; a single column
  re-stamped per mutation would destroy provenance without producing
  history.
- Gap-recovery switch gains an entry per new channel: LANDED (wave 4b)
  for settings/project/draft/queue.
- Thread **branch / remote / head** at creation: LANDED 2026-08-31
  (wave 4b) — v74 `created_branch` / `created_remote_url` /
  `created_head_commit`, surfaced as `Thread.Origin`, observed at the
  one moment the answer is true; forks re-observe, workflow threads
  attribute to no device, session import records nothing. Nothing
  renders it yet.

## 9. Wire evolution, phone, notifications

- **Hello frame lands in phase 1** (not later): server states protocol
  version, capability flags, and **server time** (phones behind captive
  portals drift, and silent DPoP skew failures are undebuggable).
  Additive-only discipline on frames and channels. An HTTP
  `/healthz`-with-version endpoint doubles as the update watchdog probe
  and the pre-WS compatibility check. LANDED 2026-08-31 (wave 4a):
  `hello` is written synchronously before the pump goroutines exist, so
  first-frame ordering is a contract; `serverTimeMs` samples at accept,
  the client derives `clockSkewMs` at receipt; capabilities serialize
  `[]`-never-`null` and are frozen by a test (`notifications.remote` is
  the first); `/healthz` answers `{version, backendId}` with no
  credential — both consumers run exactly when none is held — behind the
  Host guard with no CORS read-back, and has its `internal/surfaces`
  row. The client exposes `backendHasCapability()` and deliberately no
  version accessor.
- **Compatibility policy** (what the hello frame enforces): features
  gate on capability flags, never version comparison. A client asks
  "does the server have X", so mismatched pairs degrade instead of
  guessing. Frames and channels evolve additively. With bundle sync
  (below), the six-month support window is *not* a promise to lagging
  everyday clients. They self-update. Its real consumers are old
  native shells pinning old bundles, and federation peers: backends
  on other people's machines, updating on other people's schedules,
  where nobody can push code. Below the window a client gets a typed
  `update-required` refusal at hello, not undefined behavior. The
  swap window itself (an old bundle live against a just-updated
  backend for minutes) requires one-step wire tolerance by
  construction; the shared client is made and kept forward-tolerant
  (unknown events, fields, and frame types ignored), tested with a
  future-dialect fixture. Tolerance LANDED 2026-08-31 (wave 4a):
  unknown input is counted (`noteUnknownInput`, bounded per-kind tally,
  one `console.debug` per kind, never error-level), a `batch` missing
  `events` drops whole rather than dispatching a prefix, and event
  entries are shape-checked before they touch the replay cursor —
  one `undefined`/NaN entry used to cost the session its entire gap
  recovery. Fixtures at both levels: `wsClient.test.ts` (salted
  future-dialect stream, exact counts, silent console) and
  `e2e/tests/transport-forward-tolerance.spec.ts`.
- **The phone app is the same app.** Capacitor shell around the
  existing SPA: same Svelte code, same TS transport client and
  generated bindings, same IndexedDB replica. No Swift/Kotlin
  reimplementation and no second wire schema to drift (native plugins
  cover push, QR pairing scan, secure storage, biometrics).
  Consequences owned now: `CapacitorHttp` request interception stays
  disabled for the transport (it breaks WebSocket paths), and on the
  phone the device key lives in native secure storage
  (Keychain/Keystore, biometric-gateable) with signing done on the
  native side next to the WS bridge, not in webview WebCrypto, which
  remains the browser-class mechanism.
- **Bundle sync: the backend is the phone's update server.** The
  backend already embeds its exactly-matching frontend bundle; the
  shell self-updates its web bundle from the attached backend over the
  authenticated channel (the established live-update pattern,
  self-hosted, with no update SaaS). Semantics: never blocking. Attach
  runs on the current bundle, the new one downloads in the background
  and swaps when ready or at next launch, so an urgent approval is
  never stuck behind a download. Bundles declare a minimum shell
  version (a too-old native shell is the one case that gates on a
  store update); last-known-good is kept with first-boot healthcheck
  and auto-rollback, mirroring the remote-update posture. Trust line:
  bundles are code, so transport trust is not enough. The shell
  verifies every bundle against the **release signing key baked into
  the shell itself**. A backend can only relay genuine signed
  releases, never arbitrary script, so one misbehaving backend cannot
  reach the phone's device keys or its *other* backends' credentials
  through an update. Self-built/dev bundles require an explicit
  per-device "trust dev bundles from this backend" toggle. Only
  owner-tier backends may supply bundles at all; peer and hub
  connections never push executable content and are served by
  capability flags instead. With this, the SPA layer is effectively
  skew-free for the single-backend common case (multi-backend runs
  the newest attached backend's bundle and speaks flags to older
  ones).
- **Code trust per client class, stated plainly.** Browsers and the
  desktop attach client load the SPA *from* the backend they connect
  to. A member using a browser against a team hub executes
  hub-served code, ordinary web trust, and no trust line pretends
  otherwise. The phone shell is the only code-isolated client: its
  bundle comes solely from signed releases via owner-tier backends,
  and it speaks to hubs and peers with data plus capability flags
  only.
- **Phone transport security.** WKWebView cannot accept a self-signed
  cert for WebSocket at all (the auth-challenge hook covers HTTPS
  only; ATS exceptions are ignored for WS), so the webview never
  touches the socket. The shell ships a **native WebSocket bridge**
  (StarScream on iOS / OkHttp on Android) that owns the connection,
  pins the pairing-payload cert fingerprint exactly as the Go clients
  do (§7), and hands frames to the same TS transport client. The
  phone thereby meets the SSH bar: encrypted and pinned on the LAN
  with no domain and no tailnet, trust anchored by the pairing
  ceremony. **No cleartext phone path exists.** Tailnet and owned
  domain remain reachability/browser options, never security
  prerequisites.
- **The client replica is the diff foundation.** The shipped
  IndexedDB thread replica (cold opens paint locally, then
  `SyncThreadWindow` reconciles a windowed diff) is the remote story
  too: over a slow link, attach cost is a diff against the replica,
  not a full load. Backend-UUID keying already shipped (one database
  per backend, `ao-replica-<backendId>`; generation mismatch clears
  and re-stamps). Lifecycle LANDED 2026-08-31 (wave 4c):
  `purgeReplicaDatabases(liveBackendIds)` is the named purge
  primitive sign-out and revocation will call (empty set = drop
  everything), a boot sweep reaps `ao-replica-*` databases no live
  backend claims, deletion is token-sequenced against the session's
  own open machinery, and engines without `indexedDB.databases()`
  report `enumerated: false` honestly. Still open: the sign-out /
  revocation CALLERS (phase 2), and the resume ladder becomes
  replay-ring → windowed replica diff → full snapshot, in that
  order. At rest: the phone
  replica is encrypted with a key held in native secure storage
  outside the webview (biometric-gateable); browser profiles cannot
  do this. Revocation is not remote wipe. Cutting a device's access
  does not un-disclose what its replica already held (boundaries
  doc).
- **Reconnect discipline** (two t3code patterns adopted; **mostly
  built**, the remainder is phase 1). The target shape is already
  in-tree and generic: `stores/transportStatus.svelte.ts`'s
  `onTransportStatusChange` is the one canonical connection-state
  observable, `isTransportClassError` the shared classifier, and
  `entityStore.svelte.ts` wires the transport edge once for every
  entity store — `connected` re-acquires, anything else *suspends*
  rather than grinding a retry curve against a dead socket. Nine
  stores ride it, and its header records that five carried a
  verbatim copy before it moved there. Explicit retry-on-reconnect
  exists too (`editors.svelte.ts`, `prReviewStore`, `gitStatusStore`,
  `threadSwitchLoad`'s `retryHistoryLoad`). **Do not build a second
  suspension mechanism.** The narrower remainder LANDED
  2026-08-31 (wave 4a): `DisconnectedError` carries `closeCode`,
  clamped `closeReason`, `cause`, and `terminal`, and renders the cause
  into `message` — ~150 call sites and the error log read only
  `message`, so a cause on a field alone reaches nobody. Connect-stage
  failures (manifest fetch, thrown constructor) wrap instead of
  re-throwing raw, so `isTransportClassError` classifies them.
  Retry-on-transient-close is `RETRY_ON_TRANSIENT_CLOSE`, an explicit
  allowlist frozen EMPTY by a test with the admission criteria written
  at the seam (idempotent on the backend AND a known transient window,
  e.g. the seconds after an update restart) — never a blanket policy.
  On a flaky link this is the difference between an app that pauses
  and one that throws.
- **Ticket primitive generalizes beyond WS**: short-lived signed URLs
  for attachment upload/download and snapshot fetches, designed once in
  phase 2 rather than bolted on later. Attachments ride authenticated
  HTTP (resumable, ranged, size-capped, client-side image downscaling
  reusing the composer compression posture), never large WS frames.
- **Phone snapshot projection**: a reduced, paginated snapshot with
  payload stubs fetched on demand (principle 4, stated for phones
  explicitly), and server-side highlight spans reused rather than
  recomputed on-device.
- **Phone-era efficiency**: per-thread subscription narrowing (the
  `subscribe` frame exists, unused by the SPA), server-buffered
  assistant deltas, background scope leases (client reports visibility +
  interested scopes with TTL; backend skips unleased work, generalizing
  `HasRemoteClient`), `afterSeq`-with-snapshot-fallback resume.
- **Push**: senders run in the backend, outbound-only. Constraint to
  resolve before shipping to anyone but the owner: APNs/FCM require the
  *app vendor's* signing key, which cannot ship inside distributed
  self-hosted binaries. Personal builds can send directly; distribution
  requires either a blind relay (payload encrypted end-to-end, gateway
  cannot read it), UnifiedPush, or PWA Web Push (VAPID keys are
  genuinely per-backend). Decide before the phone app ships publicly.
- **Notification semantics**: event→push mapping (turn complete,
  approval needed, error, provider signed out), redaction policy
  (payloads transit Apple/Google, and titles and command text are
  sensitive), collapse/retract on handled-elsewhere (retraction rather
  than presence-guessing: presence heuristics are wrong whenever the
  desktop is attached but unattended), and a deep-link scheme carrying
  backend UUID + thread id.
- **Desktop notifications ride the same event mapping.** An attached
  client already receives the *thread* events, so it raises native OS
  notifications for any attached backend — remote behaves exactly as
  native on the box, no push infrastructure involved (push is the
  phone/unattached path). Audience change LANDED 2026-08-31 (wave
  4a): both channels are `AudienceAny`, producing them stays host-only
  (`LocalOnlyMethods`), and a paired test fails if that two-file
  decision comes apart. The SPA's zero-seeded `notification:activated`
  cursor — the cold-launch replay of the channel's whole retained ring —
  is gated on the session being local in both senses, so a fresh remote
  attach is not walked through every activation since boot; the ordinary
  cursor still replays real gaps. `NotificationSend`'s retained
  (non-ephemeral) retention stays — the Windows launcher replays it by
  cursor after reconnect. The preferences UI is still open.
  Notification preferences become a general device-tier setting (per
  event type × per backend); today's always-on notifications fold
  into this and become configurable. Note there are two production
  senders through `notifyOS`, not one: workflow items needing a human
  or failing, and the WSL launcher's "update didn't apply" notice.
  The handled-elsewhere retraction applies to local OS notifications
  the same as to push.
- **Approval policy**: pending approvals need a TTL / abandon policy so
  a turn does not hang forever holding a workspace when no device
  answers; approving from a notification is not allowed (app-open, and
  `approvals:respond` scope, and optionally biometric).

## 10. Multi-backend clients

Decide the **seams** in phase 1, not a speculative store rewrite.
LANDED 2026-08-31 (wave 4c):

- Thread/project id global uniqueness is a stated contract:
  `internal/entityid` mints them (canonical v4 UUIDs, `Valid` pins the
  format), every mint site calls it, and mint-site tests fail a
  short-id regression.
- `bindings.ts` routes RPCs through `resolveTransport()`
  (`frontend/src/lib/transport/handle.ts`) rather than importing the
  `wsClient` singleton; one resolution today, the multi-backend form
  changes only the resolution.
- Event fan-out carries connection origin: every delivered event's
  handler receives `{backendId}` as a second argument, stamped from
  the connection's identity (empty = unknown, never "mine").
- The IndexedDB thread replica keys its **database** by backend UUID
  so two backends' threads can never collide in one browser profile.
  Already shipped (`replica/session.ts`, `ao-replica-${backendId}`) —
  listed here as a seam the multi-backend work must not break, not as
  one to decide. Its lifecycle is not: see §9.

The genuinely collision-prone singletons (git status by path, provider
accounts/usage, settings, sysstat) get keyed when multi-backend UI
lands. `--connect` becomes "add/attach endpoint".

### Unified sidebar: the machine is a property, not a partition

One sidebar, no backend sections. Threads live on backends and appear
in every attached UI; concurrent viewing is ordinary multi-client
sync, so a thread started from one UI shows up natively on the
machine that hosts it and everywhere else attached. The machine
surfaces in exactly three places:

- **Project identity is the repo, not the checkout.** A project entry
  is the repository — matched by primary remote URL, root-commit hash
  when remoteless — and each machine × checkout path is a **target**
  under it. Two clones of the same repo, on one machine or five, are
  simply two targets of one project, exactly as worktrees already are:
  project ≠ workspace generalizes to project ≠ checkout ≠ machine.
  Thread rows carry a target chip only when their project spans more
  than one target. Identity is user-correctable (link/split) when the
  remote-URL match gets it wrong; nothing beyond that match is
  guessed.
- **The composer picks the target.** Sticky last-used per project. An
  unreachable target disables the composer for it and offers the
  reachable alternatives — never silent failover to a different
  machine. The picker shows what the choice implies: machine,
  checkout/branch, and the provider account that runs and bills the
  thread (§7).
- **Reachability is ambient, not modal.** Per-backend status lives in
  the sidebar footer; threads on an unreachable backend dim and stay
  readable from the replica. The full-width transport banner is
  reserved for the visible thread's own backend dropping.

Path links and open-in-editor from a UI that is not on the thread's
host default to copy/preview, with "open on <machine>" as the explicit
secondary. The recommended posture for real remote editing is the
editor's own remote mode over the tailnet (VS Code Remote-SSH against
the host's tailnet name and the like): a per-machine editor command
template lets the local UI open the *local* editor pointed at the
remote checkout (`vscode://vscode-remote/ssh-remote+<host><path>`
deep links — the editor's own SSH does the work). The backend
self-probes before advertising remote-open targets: no `sshd`
listening means no link offered (a clear "no SSH route" beats a
hanging deep link), and offered hosts are ordered
most-reachable-first (tailnet name, then mDNS `.local`). We do not
build a file-open protocol.

## 11. Team sharing (federation)

Chosen topology: **peer backends, not teammate devices.** A teammate's
backend holds one read-only, key-bound peer session (class named at
phase-8 design time) against
ours, scoped to shared workspaces, and re-projects to its own devices.
Reviewed alternative (teammates' browsers connecting directly) is
simpler but puts N of their devices and credentials against our machine;
one revocable peer principal per teammate is the better boundary and is
what the owner wants.

**Hub deployment.** A team server is the same binary in `serve` mode on
shared infrastructure, never on anybody's personal machine. It is the
preferred team topology: enrollment publishes to the hub, members read
from the hub, forks download from the hub, and the author's laptop can
sleep (resolving constraint 7 for the team case). Cross-team access is
hub-to-hub peering; because peering is backend-to-backend and
symmetric, laptop↔hub and hub↔hub are one code path. Dispatch-style
automation on the hub (ticket refinement, reports, sprint prep,
context gathered over time) is workflows + MCP servers + external
triggers, specced in the workflows system when built; what it demands
*here* is N-user identity (§3), attributable audit, ingress routes
entering the §13 inventory, and scope-capped workflow grants. An
externally-authored ticket body is untrusted input reaching an agent
that holds write credentials, so ingress-fed workflows declare bounded
write scopes. Report/context outputs are workflow artifacts committed
to a hub-side git repo plus their threads: versioned and forkable,
no new store.

- **Shared workspace** is the ACL unit; enrollment is the grant.
- **Read-only + fork** on personal backends, unconditionally. No
  forwarded operations, no approval proxying, no peer-triggered
  spawns. Whether *hub* threads are operable by members via workspace
  roles, and who may answer approvals there, is open (§18): the
  "approvals one-tap everywhere" decision was scoped to the owner's
  own devices and does not transfer to shared infrastructure.
- **Payload contents need classification.** Thread payloads routinely
  contain `.env` contents, tokens echoed into logs, absolute host paths,
  and diffs of private config. "Read-only" does not mean "low
  sensitivity", and a peer is an *automated* client pulling in bulk. So:
  terminal-frame and file-content payloads are withheld or redacted by
  default with explicit opt-in to full fidelity; peer bulk reads are
  rate-limited and audited with per-peer attribution; the UI states
  plainly that enrollment is a **one-way disclosure**: un-enrolling
  does not un-share what was already pulled.
- **Fork** (designed at team-time, prepared now): the transfer is
  session file(s) + our thread data + git state (a bundle, since thread
  branches often exist only on the owner's machine and working-tree
  state is uncommitted). Guaranteed layer is context-seeded continuation
  that works regardless of provider version; native resume from
  transferred provider session files is the enhancement when versions
  match. Deliberately *not* synthesizing resume files from SQLite rows.
  That would require the store to become a full-fidelity event store
  (violating principle 3) and is untestable under the no-real-provider
  invariant.
- Reachability: Tailscale node sharing (cross-tailnet, no merge) or
  LAN.

## 12. Consequences for existing principles

- **SQLite stops being purely a cache.** Identity (users, devices,
  sessions, audit) is authoritative. Losing the DB costs identity, not
  just history. Recovery = re-pairing from a host-local admin surface or
  recovery codes. Thread history remains a cache; fork explicitly does
  not change that.
- **Transport boundary unchanged**: everything still flows through
  `internal/transport`; new HTTP surfaces (auth, tickets, attachments,
  snapshots) live there under the same authorization table.
- **Client-side caches**: revoking a device kills access but not the
  history already on it. Client caches are encrypted at rest keyed
  alongside the session credential and cleared on revocation signal.

## 13. Surface inventory

Complete coverage has to be structural, not a promise. The worked
counter-example, now removed: `/design/` served agent-written files
from the SPA origin with **no token, no response headers, no
per-thread check, and symlinks unresolved** — an entire HTTP surface
sitting outside the authorization model, found only because it was
audited, and closed in 2026-08-30's design-mode removal rather than
by the fix that audit prescribed. It is kept here because deletion is
not a mechanism: the same surface would have gone unenumerated for
its whole life, and the dev-server gateway in §7 is the next thing
shaped like it. What follows is what makes the next one visible
without an audit (see the boundaries doc's findings, and §16 phase 0).

Every externally-reachable surface is enumerated in one place with four
declared properties: **listener** (which port/origin), **principal tiers
admitted**, **required scope**, **content-type posture**. The
enumeration is code, not prose, and a CI gate fails the build when a
route, event channel, or listener exists without an entry. This is the
same fail-closed pattern the method table uses. The enumeration and gate
land with phase 0 covering HTTP routes, listeners, and content origins,
so every later phase builds against the gate; the RPC-method and
event-channel classes join in phase 3 when the scope table generates.

Classes to enumerate:

- **RPC methods**: generated from `//ao:scope` annotations (§5).
- **HTTP routes**: bootstrap, WS upgrade, scoped RPC, auth/token,
  tickets, attachments, snapshots, health/version.
- **Event channels**: required scope per channel, resolved into the
  connection's precomputed visible set.
- **Listeners**: loopback, LAN, tsnet, plus the auxiliary
  loopback servers (browser MCP, harness control, claudetui gateway +
  hook relay, pprof, the `--connect` client stub, the dev supervisor)
  and the **implicit** ones our own child processes open — chromedp
  gives every managed Chrome a loopback DevTools port, which no
  inventory named until this audit. Each declares what capability it
  carries and how it authenticates, not merely that it holds no
  session credential: the browser MCP endpoint carries page
  evaluation and workspace file reads behind an unguessable path
  alone, which is a larger grant than "no session credential"
  suggests. A listener whose credential is weaker than the surface it
  gates is the pattern the enumeration exists to make visible. The
  starting inventory is 9 listeners across 7 packages, one of them
  implicit, verified 2026-08-30 against the design-mode-less tree.
- **Content origins**: anything serving bytes an agent or user
  authored declares its origin and content-type posture; agent-authored
  bytes never execute at the SPA origin.

Rules that follow: a new listener declares its binding class and what it
accepts; a new route declares tier + scope + content posture; a new
event channel declares its scope. Unclassified means unbuilt.

## 14. Performance and resource budget

Security work that lands in a hot path will get ripped out later, so the
budget is part of the design.

**Governing rule: authorization is resolved at establishment, never per
frame.**

- **Per-event cost goes *down*.** Channel visibility is a precomputed
  set on the connection (from §2's effective scopes) instead of today's
  per-event map lookup against the loopback-only table. Streaming is the
  hottest path in the app and must not gain work.
- **Per-RPC cost is one map lookup** on the generated scope table plus
  one in-memory session-table lookup. **No SQLite query per RPC**. The
  in-memory table is authoritative for live checks, SQLite is durable
  backing, and revocation writes both synchronously (§4).
- **Signature work is bounded to establishment.** ES256 DPoP
  verification (~tens of µs) happens per HTTP request and per WS
  upgrade, never per frame. Per-frame proofs are explicitly rejected.
- **The DPoP replay guard is an in-memory TTL map** bounded by the
  proof freshness window with a hard size cap, not a file per proof
  (which would burn an inode and a syscall per request and need its own
  GC). Restart clears it; the window is short enough that this is
  acceptable and is documented rather than papered over.
- **Audit records privileged and auth events, not reads.** Auditing
  every RPC would write thousands of entries during streaming. Appends
  are buffered with periodic fsync and bounded rotation, never
  fsync-per-entry.
- **Draft sync is gated on there being another client.** With a single
  attached client, debounced draft events are pure waste; the existing
  `HasRemoteClient`-style gate generalizes to "more than one session
  attached". Same rule for any other convergence-only channel.
- **tsnet is opt-in and lazily initialized.** An embedded userspace
  WireGuard stack costs memory and keeps DERP connections alive; a user
  who never enables remote access must not pay for it. Same for the
  TLS listener.
- **CSP and security headers are constant strings** set from a
  prebuilt header block, with no per-request construction.
- **Symlink-safe file serving** costs a few extra syscalls per *open*
  (not per byte), on a path served rarely.
- **Snapshot and attachment transfers ride HTTP**, not the WS, so large
  bodies never block the event socket or inflate the replay ring.

**Initial wire budgets** — starting targets, revised by measurement,
never by feel; a harness scenario counts actual bytes on the wire and
fails on regression:

- Warm attach to an already-replicated thread: **< 5 KB**.
- Cold attach to a typical thread window: **< 50 KB compressed**;
  heavy payloads stay on-demand and never ride the attach.
- Idle attached thread: **keepalive only** (tens of bytes per 10 s
  tick); an idle *unfocused* thread with subscription narrowing: zero.
- Streaming a turn: **≤ 1.3×** the raw delta bytes after compression
  and framing.
- A backgrounded / unleased client: **zero event traffic** until it
  leases back in.

### Measured baseline, and why the budget is missed today

Measured 2026-08-30 against a real 65,877-item thread, at the size the
cold open actually asks for — `SLICE_AROUND_ITEM_BUDGET = 200`, not the
500 of `ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS`, which is the *retention*
target after paging and was the first figure this section carried. The
200-row window serializes to **330 KB raw, 59 KB compressed**: 1.2×
the budget above, not the 3× first recorded here.

Where it goes matters more than the total. Of 200 rows, 109 are
`tool_call` and they carry **81%** of the bytes. Per-field, across the
whole window: payload metadata 100 KB (38%), item `meta` 63 KB (24%),
`summary` 48 KB (18%, mostly thinking text), preview highlight spans
17 KB (6%), ids and timestamps the rest. Payload *bodies* are
correctly withheld; this is the metadata riding alongside.

Two of those fields are content that **does not paint on first
render**:

- **Full tool arguments** (`meta.input`) are 59 KB of the 63 KB. A
  4.2 KB `Bash` argument object ships so a card can show one command
  line. Three consumers read sub-fields of it — `input.files`
  (`utils/fileChangeRows.ts`), `input.questions`
  (`AskUserQuestionCard.svelte`), `input.tool`
  (`utils/subagentGrouping.ts`) — and nothing reads the whole object.
- **Diff preview text and its highlight spans** are 51 KB, 15% of the
  window. `collapseDiffPreviews` defaults to `true`, so by default the
  patch sits behind a chevron and none of it paints until clicked.

That is ~110 KB of 330 KB raw rendering nothing on arrival. The
correction that follows is therefore *not* a shorter window: the row
count is what makes the timeline look complete, and the reader pages
back through it seamlessly already (auto-load fires 800 px before the
top edge, one page per gesture, with the keyed virtualizer emitting
exact scroll compensation on prepend). Cut the fields that arrive
unrendered and the same 200 rows — or more — fit the budget.

One wrinkle to design around rather than ignore: `collapseDiffPreviews`
is a *client* setting. With it off the diff text does paint on first
load, so that half of the elision is conditioned on the attaching
client's preference, not dropped outright. The projection therefore
belongs where the connection's state is known, not in the store.

**The precedent already exists in-tree, and it is the right shape.**
An earlier version of this section claimed every cap in the wire path
was a count. That was wrong. Two byte budgets already bound the
derived-cache fields, both justified in code by exactly the reasoning
above:

- `persistedCodeSpansMaxBytes = 256 << 10` (`app_highlight_persist.go`)
  bounds the `codeSpans` blob on `items.meta` — *"Meta rides every
  item-list load, so a pathological all-code message must not attach
  megabytes of runs; fences past the budget fall back to the RPC path
  lazily."* It spends a running budget across fences and **skips
  rather than breaks**, so one giant fence cannot starve later small
  ones.
- The same constant caps `preview_spans`
  (`app_highlight_diff_seed.go`) — *"preview_spans rides every item
  list read, so it gets the same retained-bytes guardrail"* — behind
  per-file (256 KB) and aggregate (1 MB) input caps.

Both already satisfy the "elision ships with its recovery route" rule
below: what they skip is fetchable through the highlight RPC. The gap
is not that the pattern is missing, it is that it was applied to the
two fields we authored ourselves and never to the provider-shaped
fields the measurement blames. Count caps that remain count-only:
`SLICE_AROUND_ITEM_BUDGET = 200`, `LOAD_OLDER_ITEM_BUDGET = 200`,
`ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS = 500`,
`inlineDiffPreviewLineCount = 30`, and the 8 MB on-disk tool-output
file cap. A row with thirty very long lines still satisfies all of
them.

Calibration, so the target is not mistaken for a crisis: t3code's own
comment on their page size says it is *"sized so first paint on the
heaviest observed threads stays around 100K gzipped."* Their widely
quoted small figures (a 15.5 KB CI ceiling) come from a fixture whose
point is that 9 MB of retained tool output ships as almost nothing —
it measures their elision, not their window, and we already keep
payload bodies off the wire. On comparable ground, heaviest thread to
heaviest thread, they are at ~100 KB and we are at 59 KB — we are
already ahead of them, and the only thing we are behind is our own
50 KB target, by 9 KB. That target stays where it is: it is more
aggressive than what they achieve, and the elision above clears it
with room to spare.

Rules that follow, and hold for any future payload:

- **Elide unrendered fields before shortening the window.** The row
  count is what makes a reopened thread look complete; the fields are
  where the bytes are. Cutting rows trades visible history for bytes,
  cutting a field nothing paints trades nothing. Reach for the window
  only once the fields are clean.
- **Every count budget still carries a byte budget**, as the backstop
  that field elision cannot provide: a window admits rows until either
  the row count or the encoded byte budget is reached, whichever comes
  first. Count alone bounds reducer churn; bytes alone bounds a small
  number of large rows. Neither substitutes for the other, and neither
  substitutes for not sending the field.
- **One oversized row is always admitted when the page is otherwise
  empty**, or pagination stalls forever on a single item.
- **A budget skips, it does not break.** Spending a running budget
  across items and stopping at the first overage lets one giant item
  starve every later small one, which reads to the user as history
  that thins out for no reason. `buildPersistedCodeSpans` gets this
  right and says so at the `continue`.
- **Byte accounting charges what actually goes on the wire**,
  including any row that appears twice in one payload.
- **Elision ships with its recovery route in the same change.** A
  truncated field carries a typed marker saying so, and the endpoint
  that returns the full value lands with it, never "later". t3code
  cut full MCP results from their payloads (12.2 MB → 546 KB) on the
  stated promise of an on-demand detail endpoint and never built it,
  turning an accepted temporary loss into a permanent one.
- **Truncate at the wire boundary, not in storage.** The persisted
  record stays complete; only the projection shrinks.

### Wire budget enforcement

LANDED 2026-08-31 (1fbb771c) as a Go gate rather than a harness
scenario, deliberately: `internal/app/app_wire_budget_test.go` seeds a
deterministic heavy thread built to the field split above (fixture
entropy corrected until its deflate ratio matches the real thread's
5.6:1), marshals the cold 200-row window into the same
`transport.ServerFrame` the connection writes, and deflates it at the
socket's own level. That puts the gate in `make go-test` on every
commit instead of `make e2e`, with ceilings for both clients (default
193.1 KB raw / 38.4 KB deflated measured, ceilings 216 KB / 44 KB —
under the 50 KB budget with room; previews-on 248.4 / 49.0, ceilings
272 / 54) plus an anti-rot companion that measures the same window
unprojected and fails if the projection's saving disappears. Budgets
live in that test rather than in logs, so a regression fails locally
before it ships.

### What t3code did that we do not need, and what we do

Their largest wins were architectural catch-up we already have.
Their activity rows stored full tool payloads inline, so they built
an allowlist projection to strip them on the way out (12.2 MB → 546
KB for MCP results) and later a second one at ingestion, after
discovering one 65 KB tool result had persisted 238.7 MB across 2,226
streaming updates. Our payload bodies have always lived in a separate
table behind an id, and items persist on completion rather than per
update, so neither problem exists here. We also already have the
partial-window guard they rate as their most valuable idea: an event
for an item outside the loaded window must not be appended at the
end. Ours is cursor-based in both directions and handles negative
item indexes from head-healed prompts.

Deliberately not adopted, three things:

- **Their field-allowlist projection.** It is only as correct as the
  inventory of fields the client reads, and that inventory decayed
  twice in production — once dropping the real status so failed tool
  calls rendered as successful, once matching tool identity on the
  wrong field so the dedupe silently fell back to comparing titles. A
  byte cap on a field we already know is heavy carries no such
  inventory.
- **Summarizing tool output to one line at ingestion, permanently.**
  It is their single largest lever (9 MB of retained output shipping
  as under 16 KB) and it is irreversible by construction: a 900 KB
  result becomes 84 characters and no endpoint returns the rest. Our
  payload table exists precisely so the full value stays fetchable.
- **Buffering assistant text server-side and flushing at block
  boundaries.** Their token streaming is a legacy opt-in that
  defaults off, so a turn's prose crosses as a handful of frames.
  That is a byte win bought with the live-typing feel the reveal
  queue and spinner work are built around. We keep streaming and
  bound its overhead with the ≤1.3× budget instead. Worth noting our
  thread stream already coalesces (16 ms / 50 events) where theirs
  does not — only their sidebar stream has a coalescing window.

Worth taking beyond the byte budgets: dropping rows a snapshot does
not need (they found 47k superseded tool-update rows in one database,
and stale context-window rows were 24–37% of snapshot bytes), applied
to snapshots only and never to live events, since the client folds
live rows itself.

Phase 6's phone work (subscription narrowing, buffered deltas, scope
leases) is a net *reduction* in wire and CPU cost, not an addition.

## 15. Hard constraints

1. WebAuthn RP ID must be a registrable domain, with no IPs and no
   `.local`; secure context required (localhost is dev-only). Pairing
   codes are the universal fallback.
2. One passkey ↔ one RP ID; related origins capped at 5.
3. iOS associated domains are baked at build/sign time: native-app
   passkeys only under a vendor-controlled (wildcardable) domain.
4. APNs requires the Apple Developer Program ($99/yr); FCM requires a
   free Firebase project; iOS Web Push requires home-screen install and
   is unavailable to EU-mode PWAs. Vendor push keys cannot ship in
   self-hosted binaries (§9).
5. tsnet needs a control plane (Tailscale account or self-hosted
   Headscale).
6. Plain-HTTP LAN browsers lose WebAuthn, service workers, clipboard,
   **and non-extractable WebCrypto**, so they are bearer-only. There is
   no LAN-HTTP DPoP path.
7. A sleeping machine is unreachable; wake-on-LAN is out of scope. The
   app may offer a keep-awake-while-sessions-live inhibitor.
8. WKWebView cannot validate self-signed certificates for WebSocket
   connections (HTTPS-only hook; ATS exceptions ignored for WS). So
   in-webview transport never gets domainless TLS. This is why the
   phone shell's socket lives in the native WS bridge (§9); plain
   browsers remain unpinnable.

## 16. Phases

0. **Open content-isolation defects.** Independent of everything else
   and reachable today, in the desktop webview, with no remote feature
   enabled. Re-verified against this tree on 2026-08-30, after design
   mode was removed on main. That removal closed the largest item on
   this list outright — the `/design/` route was the only same-origin
   surface serving agent-authored bytes, and with it went the
   unauthenticated read, the symlink following, the directory
   listings, the second Chrome launcher's sandbox disagreement, and
   half the MCP-endpoint item. Four other entries left the list
   earlier and are recorded elsewhere rather than here: the markdown
   renderer's relative-href branch (fixed at the render layer, both
   paths verified), the persisted stable port and the backend-keyed
   replica (both already shipped, §7 and §9), and a `frontend/CLAUDE.md`
   correction whose claim now lives in a code comment.

   Two items were considered and **deliberately not taken**, so they
   do not come back on a later pass. The click delegate's
   no-`preventDefault` fall-through stays as it is: an agent that
   could plant a hostile anchor already has a shell, so the anchor
   buys it nothing, and the markdown path can no longer emit one
   regardless. `PRStep.svelte`'s two unvalidated forge hrefs stay as
   they are: a forge API returns the same URL the real pull request
   page would link, so validating ours while the real page does not
   is theater. The click delegate is recorded in the boundaries doc
   as observed behavior rather than debt; PRStep is recorded here
   only, because it is a decision about our own component and not a
   property of the boundary.

   - **A baseline CSP.** LANDED 2026-08-31 (2eb5c7dc): every served
     response carries a prebuilt policy (`transport.CSPProduction` /
     `CSPDevServer`), chosen once at server construction from the
     dev-asset-proxy condition so policy and handler cannot disagree,
     and `WriteSecurityHeaders` takes the policy as a typed argument
     so a route cannot ship without naming one. `script-src 'self'`
     with no hash and no nonce — the first-paint theme stamp moved to
     `frontend/public/boot-theme.js` (byte-identical validator), and
     `index.html` holds no inline script or style, held by a test.
     The dev variant relaxes `connect-src` alone (Vite's baked-in
     direct HMR socket fallback); a test fails if the two policies
     differ anywhere else. The e2e page fixture collects
     `securitypolicyviolation` events across the suite. Verified in
     Chromium end to end; the WKWebView leg (macOS) is verified by
     spec reading only and is the first thing to eyeball on a Mac
     boot. The `--connect` stub now serves the exact root via
     `/{$}` and everything else from the bundle file server, because
     the shell answering for `/boot-theme.js` under `nosniff` would
     have silently dropped the theme stamp on that origin.
   - **The boot credential moves out of script reach.** LANDED
     2026-08-31 (24486360): a page URL carries a one-time ticket
     (`?t=`), the first `/bootstrap.json` exchanges it for an
     HttpOnly, SameSite=Strict, port-qualified cookie
     (`ao_page_<port>`), and the SPA strips the ticket from the URL.
     `sessionStorage['ao:bootstrap-token']`, `window.__AO_BOOTSTRAP__`
     and every reader of either are gone. `OriginAllowed` gates `/ws`,
     `/bootstrap.json` and the new `/pageurl` ahead of the credential
     and is load-bearing on loopback too (cookies do not scope by
     port). Consumers that navigate more than once (Windows launcher
     reload, `ao-harness`, the e2e rig) ask the credentialled
     `GET /pageurl` for a fresh URL. One validation function
     (`Credential.Authenticate`), three carriers: cookie, bearer
     header, `?token=` for URL-only WebSocket APIs. This is the same
     channel that carries session credentials from phase 2 on, not a
     stopgap.
   - **The `--connect` client stub hands out that same credential.**
     LANDED 2026-08-31 (same commit): the stub serves the SPA shell
     verbatim on its own origin, issues its own page cookie, and
     carries `/ws` to the upstream through `httputil.ReverseProxy`
     with the upstream token attached server-side, so `validateWsUrl`
     now holds same-origin with no exemptions in every mode. The
     upstream's verdict on the configured token is relayed through the
     stub's own manifest probe (bearer header, refusal maps to 404,
     transient to 503).
   - **The browser MCP endpoint authenticates on an unguessable path
     alone.** LANDED 2026-08-30 (476f428f): every request now clears a
     loopback-peer check off `r.RemoteAddr` (the claudetui gateway's
     precedent), a refusal of any `Origin` header, and an
     `application/json` requirement that forces a preflight where a
     `text/plain` POST would have been a CORS simple request. Both
     real provider clients verified against their header
     construction. The listener still binds eagerly — its URL rides
     provider argv at spawn — and that property is now documented at
     `ensureStarted`.
   - **Tests.** LANDED 2026-08-30 (7897c969): the `//`-leading href
     is pinned through the real `ChatMarkdown` on both render paths,
     and `ChatMarkdown.compactStaticLinkUrls.test.ts` drives a
     20-class href corpus through `staticHtml.ts` and `Link.svelte`
     with per-class test names, so a future edit to one path that
     forgets the other fails the case naming the divergent class.
   - **The §13 surface enumeration + CI gate.** LANDED 2026-08-31
     (7ead32ed): `internal/surfaces` holds the authored rows — 9
     listeners across 7 packages (the tree had not drifted from the
     audit), 17 HTTP routes across all four muxes, 8 content origins,
     each with binding class, credential, posture and a Why — and its
     AST gate scans the Makefile's package roots, failing in both
     directions (unenumerated bind/route, or a row whose file no
     longer binds) with zero exclusions. The RPC-method and
     event-channel columns LANDED 2026-08-31 (wave 6d2) as two
     `Registry` REFERENCE rows — listener, routes, authored-table
     source/symbol, required row fields, and the gate functions that
     read each — with an AST cross-check that fails on a moved symbol,
     an unclassified entry, or a deleted gate, rather than a duplicate
     of the 360-method / 72-channel tables that would only ever drift
     from them. Open repo-hygiene item the sweep surfaced:
     `spike/claude-mitm` is checked in with two live `net.Listen`
     calls against spike-policy step 5; it sits outside the gate's
     package roots.
   - **Doc drift inside the classification table.** LANDED 2026-08-31
     (0114caed): `LocalOnlyCategory` is a closed typed set (ten at
     landing; wave 5c added `CategoryDeviceAccess` as the eleventh),
     each entry in the authored `localOnlyCategories` map carries one,
     and `LocalOnlyMethods` is derived from it — the name set held
     byte-identical through the change, with the wave-2 addition
     (`StopThreadBackgroundWork`) categorized at the merge. Gates pin
     the set closed, the ordinals contiguous, and every entry tagged.
     Sibling landing, same commit series (d7b67946): seven loopback
     predicates consolidated into `internal/loopback` (four named
     predicates; `EndpointAuthority` and `EndpointHostname` provably
     cannot fold and a test pins why).

1. **Sync sweep + seams.** Archive-closes-session fix: LANDED
   2026-08-31 (b809e997, §7). Thread-row emits: LANDED 2026-08-31
   (9d48ee7c, §8). The sync sweep is COMPLETE: settings/project/draft
   emits, gap entries, race arbitration with the typed
   `already_handled` code, the device-attribution column, and thread
   branch/remote/head recording all LANDED 2026-08-31 (wave 4b, §8).
   Hello frame + `/healthz`, per-call cause preservation with the
   frozen-empty retry allowlist, forward tolerance with its
   future-dialect fixtures, and the notification audience change:
   LANDED 2026-08-31 (wave 4a, §9). The suspension mechanism itself
   was already built and was not duplicated. Replica lifecycle and the
   multi-backend seams, backend UUID on the wire included (§10):
   LANDED 2026-08-31 (wave 4c). Byte budgets: LANDED 2026-08-31 (1fbb771c, §14
   "Wire budget enforcement") — `internal/itemwire` projects every
   item path (pagers, `SyncThreadWindow`, live upserts/patches), with
   typed markers, the `GetThreadItemProjectionSource` recovery route,
   a per-window byte backstop, and the counting gate in `make
   go-test`. The window keeps its 200 rows.
2. **Identity core.** Genuinely N-user from the start, with no implicit
   single owner anywhere in queries, session checks, or audit
   attribution (hub deployments depend on it; §11). Schema
   (users/devices/sessions/audit), revocation with live teardown,
   recovery codes, rate limiting, and the typed refusal vocabulary:
   LANDED 2026-08-31 (wave 5a, 7dccc702) — migration v75 (six tables;
   `EnsureOwnerUser` is the one role-resolved read and says so),
   `internal/identity` (HMAC session claims with signature checked
   structurally before the time window, both-halves verification, the
   in-memory per-RPC fast path invalidated synchronously on revoke,
   Crockford-alphabet recovery codes consumed by one CAS statement,
   idempotent `Bootstrap`), the transport live-session registry with
   three-step synchronous teardown behind `Config.SessionForRequest`
   (nil until phase 3 migrates clients), per-peer token buckets on the
   three credential surfaces refusing 429 + `Retry-After`, and
   `auth_failed` + reason on the wire with
   `frontend/src/lib/transport/authReason.ts` as the one hint module,
   pinned against the Go set in both directions.
   Pairing, token exchange, rotating refresh, tickets, and local-client
   sessions: LANDED 2026-08-31 (wave 5b) — migration v76
   (pairing_links, refresh_secrets, devices.channel,
   sessions.activated_at with the confirmation gate INSIDE
   Session.Live), keypair-first redemption with the owner verification
   number derived from the redeeming key, rotating refresh whose
   family key IS the session id (reuse spends the chain then revokes
   the session), one ticketBook behind both the page ticket and the
   30s session-named /ws ticket, per-connection liveness re-check +
   remote lifetime cap, /auth/pair + /auth/token + /auth/ticket on one
   shared tight budget, the implicit loopback page-channel session
   riding the bootstrap exchange as an HttpOnly cookie, WSL launcher
   credential forwarding, and `SessionForRequest`/`SessionLive` wired
   from app boot. Device-access RPC surface, ui_state device binding,
   the shared `relaysession` credential source (now also on the
   `--connect` hop), and the redeeming client: LANDED 2026-08-31 (wave
   5c). The owner-facing devices pane in Settings (list / pair /
   confirm / revoke UI over those RPCs) and the paired client's
   restart-recovery legs: LANDED 2026-08-31 (wave 5c close). Live
   verification surfaced three wire rules now pinned in
   `internal/transport/AGENTS.md` and the frontend transport guide:
   while a paired session is stored it is the ONLY identity the
   upgrade may present (a dial that cannot mint a ticket fails and
   retries instead of proceeding on the page cookie); the manifest
   admits a live durable session when the page-credential exchange
   refuses, without planting the local channel's session cookie; and
   a spent WS ticket naming a live session stands in for the launch
   credential on that upgrade, with the Origin check unconditional.
3. **Authorization.** Annotation-driven generated method table, scope
   tiers + binding enforcement + step-up set, event visibility, settings
   key→tier taxonomy, capability-driven frontend, `LocalOnlyMethods`
   derived then deleted. The table, tier vocabulary, step-up
   annotations, and derived `LocalOnlyMethods` (43 transitional
   overrides): LANDED 2026-08-31 (wave 6a — see §5 for what the pass
   established). Enforcement — the per-RPC scope gate, typed
   refusals, host-presence step-up, the effective-runtime-mode
   autonomy recheck, scope-driven event visibility, settings-tier
   gate, `settings:read` (35 overrides remain): LANDED 2026-08-31
   (wave 6b — §5 has the shape and the two recorded gaps). The
   capability-driven frontend: LANDED 2026-08-31 (wave 6c1 — §5). The
   webview dropping `?t=`: LANDED 2026-08-31 (wave 6c2 — §4 "Local
   clients"). `/ws` onto session credentials: LANDED 2026-08-31 (wave
   6d1 — §4 "Local clients") for every off-host peer; loopback tooling
   keeps the launch credential by adjudication. Origin-gate deletion,
   the binding-class prerequisite, and §13's RPC and event-channel
   registry rows: LANDED 2026-08-31 (wave 6d2 — §5 "Phase 3 closed").
   **Phase 3 is complete.**
4. **Settings storage.** Host JSON / user+device in `ui_state`,
   migrations, per-class defaults.
5. **Serve mode, endpoint, TLS, tsnet, passkeys, remote update with
   rollback, provider remote re-auth.** DPoP mandatory here (the token
   endpoint accepts thumbprints from phase 2 so nothing reworks).
   Includes a headless build target that does not link the webview/GTK
   stack, and the unattended credential-storage posture (§7), both
   prerequisites for server deployments.
6. **Phone preparation.** Subscription narrowing, buffered deltas, scope
   leases, reduced snapshots, attachment flows, push senders +
   notification semantics + deep links. The Capacitor shell itself
   (same SPA + native plugins, §9) is scaffolded here, including the
   native WebSocket bridge (the phone's only transport, §9) and
   bundle sync from the backend with rollback (§9); store builds
   come whenever the app ships.
7. **Multi-backend UI.** Keying the collision-prone singletons; the
   unified sidebar with project targets, composer target picker, and
   ambient reachability (§10); the port gateway's remote wiring in
   the in-app browser (§7).
8. **Team sharing.** Hub-first: team-server deployment, shared
   workspaces with roles, peer sessions, hub-to-hub peering, payload
   sensitivity tiers, fork pipeline. Ingress triggers and per-workflow
   scope grants land alongside as workflows-system work (§11).

Each phase leaves `make check` green.

## 17. Testing

- **Generator gate**: every bound method must declare a scope or the
  build fails (replaces the completeness test).
- **Go integration**: pairing consume-once races, proof-of-possession
  mismatch, token exchange, refresh rotation + reuse detection,
  revocation mid-connection (WS teardown), DPoP replay/skew/downgrade
  attempts, ticket single-use, binding-class enforcement per listener,
  step-up requirement, rate-limit keying.
- **Harness**: multi-client fixtures (two WS clients, fan-out,
  scope-refused calls, reconnect-with-replay, draft echo suppression),
  scope-lease transitions, not just states (off→on, TTL lapse mid-turn,
  visibility flap), later a two-backend fixture.
- **Playwright**: pairing UX end-to-end, second browser context as a
  second device, capability-gated UI.
- Every refusal path gets a test.

## 18. Decisions still open

1. Push distribution posture (§9). Direct for personal builds is fine;
   the distributed answer must be chosen before public release.
2. How much of the payload-sensitivity machinery (§11) is built at
   team-time vs. designed-only now.
3. Whether draft "edited on <device>" and presence-aware routing survive
   at all (marked cuttable).
4. Hub-thread operability (§11): whether shared-workspace threads on a
   team server are operable by members via workspace roles (personal
   backends stay read-only + fork regardless), and who may answer
   approvals on a hub thread: any member holding the scope, the
   thread starter, or a role gate.

Ruled 2026-08-31 (user): `access:admin` exists as a standing remote
scope. Device revoke and rename from a paired device ride the standing
grant behind a confirmation dialog; step-up stays on pairing-grant
minting only, because pairing is where the strong ceremony already
lives (passkey once phase 5 lands) and a device's own unlock covers
casual access. Also ruled, superseding an
earlier same-day ceiling ruling: the public path does not exist. The
reachable paths are loopback, LAN, and the owner's tailnet, all
trusted alike — every granted scope, `terminal:operate` included,
works over the tailnet. Funnel/cloudflared tunnel exposure is cut from
the design entirely (the `public` session class with it): every
connection is an intentional one through an enrolled device, and
sharing rides proper channels (tailnet node sharing, LAN) rather than
a published endpoint. If a no-tailnet share link is ever wanted, it
returns as a phase-8 question against a team hub, not this backend.

Settled in review: approvals are never gated on the owner's own devices;
terminal access is not withheld from native clients by device class;
scope narrowing is per-device and opt-in, never imposed; the boot
credential rides an HttpOnly cookie with an Origin-checked WS upgrade
from phase 0 rather than deferring that shape to the session work; the
CSP is strict in production and relaxed in dev, with HMR removal an
independent preference rather than a CSP prerequisite; the surface
enumeration gate lands in phase 0, not phase 3.
