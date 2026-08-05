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
- Teammates' *backends* peer with ours (federation) holding read-only
  scoped sessions over shared workspaces, and can fork enrolled threads
  to continue locally. Their devices never touch our machine.
- Reference: t3code's environment auth (pairing → token exchange →
  scoped sessions → DPoP → WS tickets), fully self-hosted. We adopt its
  shape, add a user model and a credential-binding axis it lacks.

## 2. Access model

Today, the host-capable RPCs require **two** independent facts: a valid
token **and** a loopback origin. Scopes alone would collapse that to
one, so every own-device session would carry full host capability from
anywhere. Authorization is therefore a product of three axes:

1. **Scope** — what the principal may ask for (§4).
2. **Binding class** — how strongly the credential is tied to a device.
3. **Step-up** — per-call fresh proof for a small catastrophic set.

### Binding classes

| Class | Minted for | Accepted on |
|---|---|---|
| `loopback-only` | embedded webview, WSL launcher relay, `ao` CLI | loopback listeners only |
| `device-bound` | paired devices with a key (DPoP) or passkey | any listener |
| `public` | sessions used over tunnel/Funnel | any listener; DPoP proof required per request |

Rules:

- **Binding travels with the credential, not the socket.** A session
  ever issued key-bound is never accepted as a plain bearer on *any*
  listener, including loopback. This closes the downgrade where a
  leaked token is replayed on a softer listener.
- **Execute-tier scopes (§5) require `binding ≥ device-bound`.** A
  leaked `loopback-only` webview session therefore carries no remote
  capability at all.
- Publicly-reachable listeners accept only `public`-class presentations.

### Principal tiers

Who the credential belongs to sets a hard ceiling no grant can exceed:

| Tier | Principal | Ceiling |
|---|---|---|
| `host` | embedded webview, local CLI — same process tree | everything, including `scope: host` |
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
| private network — LAN with TLS, tailnet | full (subject to tier) |
| public — Funnel, Cloudflare tunnel | full **minus the step-up set**, and `public` binding required |

### Effective scopes

```
effective = granted(device) ∩ ceiling(principal tier) ∩ ceiling(network path)
```

Resolved **once**, at session establishment, into a precomputed set
carried on the connection. Every surface — WS RPC, HTTP RPC, event
push, attachments, snapshots, design files — authorizes from that one
set (§13). There is no second code path that decides access, so there is
no surface that can drift out of policy.

This is what makes "my phone on my tailnet" and "my phone over a public
tunnel" different privileges without maintaining two device records, and
what makes a teammate's backend structurally incapable of holding an
execute scope even if a grant were mis-issued.

### Scope of impact (stated plainly)

The session rows and the signing key live on the same machine, under the
same user, as the provider processes a session can start. Anything that
reaches execute-tier capability locally can mint its own credentials and
rewrite any on-machine record. This system's value is therefore
**gating what happens before that point, plus off-machine
accountability** — not constraining capability the machine already
granted. Design effort goes to the gate; the boundaries doc lists what
is deliberately out of scope.

## 3. Identity model

Entities in SQLite (authoritative data, not cache — see §12):

- **Backend** — one row, minted at first boot: stable UUID + display
  name. Required by deep links, push routing, multi-backend store
  keying, and fork provenance. Mint in phase 1.
- **User** — an account; first boot creates the owner. Multi-user
  arrives with team sharing; schema assumes plurality from the start.
- **Device** — one client instance (this desktop, this browser profile,
  this phone, a peer backend). Label, class
  (`desktop | browser | phone | cli | backend-peer`), platform,
  created/last-seen, key thumbprint and/or passkey credential.
- **Session** — device → user binding with a scope set, binding class,
  HMAC-signed claims **and** a DB row; both required to verify.
- **Recovery codes** — minted at owner creation, single-use, offline.
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

### Sessions

- **Access tokens are short-lived** (minutes–hours), always
  key-bound for `device-bound`/`public` classes.
- **Refresh is rotating with reuse detection**: each renewal issues a
  new refresh secret and invalidates its predecessor; replay of a spent
  refresh forks the family, **auto-revokes the whole family**, and
  alerts the owner. This is how a leaked credential is detected; a copy
  cannot renew indefinitely alongside the real device.
- Refresh binds to the device key on every listener — a bare bearer
  token on its own cannot self-renew.
- Browser class: short TTL, non-renewable without passkey re-auth where
  passkeys are available.

### WebSocket tickets

The session credential never rides a WS URL. Client POSTs for a ticket
that is **single-use** (consumed on first upgrade), short-lived, and
**key-bound** (redemption requires a DPoP proof). The established
connection re-validates session liveness on an interval and caps its own
lifetime, forcing periodic re-ticket. Per-RPC scope checks still apply
after upgrade.

### Revocation

Revocation is only real if it reaches live connections. A live-session
registry keyed by session id **force-closes** matching WebSockets and
stops their event streams synchronously on revoke; the in-memory session
table is the per-RPC fast path and is invalidated at the same instant.
No RPC authorizes from state cached at upgrade time.

### Passkeys (where a real domain fronts the backend, §7)

`go-webauthn`. RP ID = the backend's canonical domain. Registered from
an already-paired session (pairing bootstraps, passkey hardens). Uses:
new-device sign-in without a code, browser re-auth after short-TTL
expiry, and **mandatory step-up** for the catastrophic set (below).
Cross-device (phone signs for a browser via QR) is native CTAP hybrid.
Fallback is always pairing.

### Step-up (mandatory, not optional)

A per-call fresh passkey (or host-presence) proof — never an ambient
standing scope — is required for: minting pairing links, network bind /
exposure changes, provider custom-env writes, MCP config writes, WSL
distro preference, and remote update triggering (§7). Optional step-up
is theater; these are the calls that re-key the system or re-route every
prompt.

### Local clients

The embedded webview drops `?t=`: at boot the backend mints an implicit
`loopback-only` device session delivered over the existing fd/stdout
bootstrap. The WSL launcher **forwards that credential** rather than
relying on apparent loopback origin — with topology no longer
authorizing by itself, "looks like loopback" must stop being a trust
basis (a same-host relay can otherwise launder remote peers).

## 5. Authorization

### Two enforcement tiers, eight labels

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
| `settings:write` | execute | user/device-tier settings; host-tier and the step-up set are excluded |
| `access:admin` | execute | device list/revoke, audit read; **minting and network changes additionally require step-up** |

Rationale for the splits: answering an approval authorizes host command
execution, and a thread in `full-access` mode needs no approval at all —
so approval-answering and autonomy changes carry exactly the same weight
as `terminal:operate` and should not share a scope with "send a
message". Provider custom-env re-points every turn's traffic, and MCP
config registers a binary the provider will run; neither is an ordinary
settings write.

The scopes are separate *names* because peers, viewers, and the audit
log need to distinguish them — **not** because the owner's own devices
should be gated against each other.

Default profiles:

- **Owner devices — every scope except `scope: host`, on every device
  class.** Approvals are one-tap everywhere: gating them while leaving
  message-send open protects nothing (injected script that can send a
  message can simply instruct the agent), so the gate would cost daily
  friction for no security. Terminal access from a key-bound native
  client is comparable to an SSH session from a phone and is not
  withheld. The meaningful distinction is **native vs browser** — only
  browsers have a script-execution surface — so narrowing is offered
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
Scope, StepUp}` table, and **fails the build on an unannotated method** —
replacing the test-only `wireSafeMethods` completeness gate. `host`
becomes a scope *value*, so the "host-only residue" is just
`scope: host`, not a parallel map. `InternalServiceMethods` stays (never
registered). `ScopedTokenMethods` stays — grants are a genuinely
different axis — with a CI cross-check that every entry exists in the
generated table.

`LocalOnlyMethods` becomes **derived** from the scope table on day one
of phase 3 (privileged scope ⇒ local-only), so only one hand-edited
source exists while clients are migrating; the origin gate is deleted
once every client authenticates.

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

### Frontend capability model

`isViewOnlySession()` (bootstrap boolean) is replaced by a granted-scope
capability object; the ~15 gating files key off the capability they
need. Scope-refusal errors are structured and name the required scope,
so disabled-state tooltips are self-describing. **The server never
trusts the client's capability object** — every RPC re-checks
server-side; hello-frame flags are compat hints, never authorization.

## 6. Per-device and per-user state

### Fix the identity hole

`GetUIState`/`SetUIState`/`DeleteUIState` currently take a
caller-supplied `clientID` — a spoofable bearer string. They stop taking
it; the backend derives scope from the authenticated session's device.

### One mechanism, three tiers

- **Host tier stays in `settings.json`**: it configures the backend
  before identity or the DB matter (network bind, port, provider
  binaries + custom env, retention, observability, WSL preference) and
  must be hand-editable when the UI is unreachable. Keeps
  `settings.Service` + `atomicfile`.
- **User and device tiers live in the `ui_state` table**, which already
  exists for exactly this shape (and already migrated pane layout out of
  settings). User tier = `user:<id>` scope; device tier = `device:<id>`
  scope, with typed validation over the same store. Device rows cascade
  on device deletion — revoking a device drops its state for free.
- Device tier (defaults per device class; phone ships `lowPowerMode`
  on): `lowPowerMode`, theme, fonts + `fontSize`, `paneDensity`,
  `activityRunWindowRows`, `activityRunDefault`, `streamingEnabled`,
  `diffWordWrap`, `collapseDiffPreviews`, `timestampFormat`,
  `editor.preference`, `backgroundGitFetch`, `projectSortMode`,
  `usagePeriod`, `recentWorkspaces`, `remoteEndpoints` (also ends
  today's credential fan-out), window geometry (ends the single global
  slot two desktops fight over).
- User tier: confirmations, commit-message style, textgen routing,
  hidden models, default thread env mode, worktree branch prefix,
  auto-compact thresholds, GitLab hosts.

The **key→tier taxonomy lands in phase 3**, with the scope table:
device-tier writes ride a valid session (they touch only `device:self`),
user-tier writes need `settings:write`, host-tier needs step-up. Phase 4
is then pure storage migration with no scope churn.

## 7. Transport, reachability, TLS

### Multi-listener, one session store

Loopback (webview, CLI), optional LAN bind, optional tsnet listener,
optional tunnel-fronted. Sessions are valid across listeners **subject
to their binding class** (§2) — local clients never hairpin through the
tailnet, and a soft listener cannot launder a strong credential into a
weaker presentation.

Cross-origin defense is explicit: strict Host allow-list (canonical
domain + known loopback names), Origin / `Sec-Fetch-Site` checks on
`/ws` and every auth endpoint, DNS-rebinding rejection.

### Stable endpoint

Port becomes a setting (pick-random-once-then-persist default,
user-fixable). Durable sessions remove re-pairing after restarts; origin
stability keeps browser storage attached.

### TLS (in-app termination)

Two supported paths; others are documented escape hatches, not built:

1. **Owned domain + DNS-01** — DNS record → LAN IP (public DNS may hold
   private addresses), Let's Encrypt via DNS-01, backend renews. Real
   HTTPS on a LAN-only path, valid passkey RP ID, no tunnel.
2. **tsnet cert** — LE cert for the node's `*.ts.net` name, MagicDNS
   resolution, direct peer connections.

Escape hatches: private CA (mkcert-style, manual trust), cloudflared
subprocess with an owned domain. The chosen HTTPS name is the backend's
**canonical domain**: passkey RP ID, related-origins anchor (max 5), and
the phone app's dial target.

### Anywhere access

tsnet embedded (BSD-3, userspace, works in WSL2 without TUN) with
Funnel for public reach; cloudflared subprocess as the alternative.
Public listeners: `public`-class sessions only, step-up set unreachable
without fresh proof, pairing/token/ticket endpoints rate-limited with
limits keyed by **token/account with a global counter across listeners**
(per-IP fails behind a tunnel, where every request shares one source
address; derive real client IP from our own validated forwarded header).

### Headless serve mode and remote update

`agent-overflow serve` runs windowless; the desktop app attaches.
Service install (systemd user unit / launchd / Windows via the WSL
launcher) with a stated headless credential-storage posture — keychains
frequently cannot unlock without a login session, so the signing key,
provider credentials, and tsnet state need a defined at-rest strategy
for unattended boot.

Update is a genuine availability requirement once the machine is
unattended, and a supply-chain risk if remotely triggerable. Resolution:
download/apply remain `scope: host`; a **remote trigger** exists but
requires step-up **plus** artifact signature verification, and runs
behind a healthcheck-and-auto-rollback watchdog that preserves listener
config and the session store. A bad update must never lock the owner
out of a machine they cannot physically reach.

### Provider re-authentication while remote

Provider OAuth redirects to `localhost` **on the host** — unreachable
from a phone — yet provider logins die at inconvenient times (see the
2026-08-03 credential-death incident chain). Without a remote path, one
token rotation bricks the backend until the owner is physically present.
Required: provider auth state is a first-class remote-visible signal
with a push event, and re-auth is completable remotely — the backend
surfaces the authorize URL to the authenticated remote client and
proxies its own loopback callback (or relays the paste-code/setup-token
flow). If any provider makes this impossible, that limitation is
documented explicitly rather than discovered in the field.

## 8. State sync completeness

Prerequisite sweep, valuable standalone:

- Emit on every persisted mutation: the ~12 thread-row RPCs
  (create/delete/archive/pin/read/model/effort/fastMode/contextWindow/
  branch/workspace), `settings:updated` (with tier + keys),
  `project:*`. Frontend replaces local-only applies (`syncThread`,
  `*Local`) with event-driven convergence; initiators may still apply
  optimistically.
- `draft:updated` with initiator echo-suppression; last-write-wins plus
  an "edited on <device>" affordance (cuttable polish).
- Wire `GetQueueState` as the fresh-attach bootstrap.
- Races: backend is single-writer; losers get typed already-handled
  responses; state-change events flip other devices to "answered on
  <device>" live.
- **Device attribution** on persisted mutations (which device did it) —
  a trivial column now, required later for audit and shared-thread
  provenance.
- Gap-recovery switch gains an entry per new channel.
- Threads begin recording **branch / remote / head** so future forks are
  possible for threads created before team sharing exists.

## 9. Wire evolution, phone, notifications

- **Hello frame lands in phase 1** (not later): server states protocol
  version, capability flags, and **server time** (phones behind captive
  portals drift, and silent DPoP skew failures are undebuggable).
  Additive-only discipline on frames and channels. An HTTP
  `/healthz`-with-version endpoint doubles as the update watchdog probe
  and the pre-WS compatibility check.
- **Ticket primitive generalizes beyond WS** — short-lived signed URLs
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
  interested scopes with TTL; backend skips unleased work — generalizes
  `HasRemoteClient`), `afterSeq`-with-snapshot-fallback resume.
- **Push**: senders run in the backend, outbound-only. Constraint to
  resolve before shipping to anyone but the owner — APNs/FCM require the
  *app vendor's* signing key, which cannot ship inside distributed
  self-hosted binaries. Personal builds can send directly; distribution
  requires either a blind relay (payload encrypted end-to-end, gateway
  cannot read it), UnifiedPush, or PWA Web Push (VAPID keys are
  genuinely per-backend). Decide before the phone app ships publicly.
- **Notification semantics**: event→push mapping (turn complete,
  approval needed, error, provider signed out), redaction policy
  (payloads transit Apple/Google — titles and command text are
  sensitive), collapse/retract on handled-elsewhere (retraction rather
  than presence-guessing: presence heuristics are wrong whenever the
  desktop is attached but unattended), and a deep-link scheme carrying
  backend UUID + thread id.
- **Approval policy**: pending approvals need a TTL / abandon policy so
  a turn does not hang forever holding a workspace when no device
  answers; approving from a notification is not allowed (app-open, and
  `approvals:respond` scope, and optionally biometric).

## 10. Multi-backend clients

Decide the **seams** in phase 1, not a speculative store rewrite:

- Document and enforce global uniqueness of thread/project ids (already
  UUIDs) so most stores need no re-keying.
- `bindings.ts` routes RPCs through a resolvable transport handle
  rather than importing a singleton.
- Event fan-out carries connection origin (backend UUID).

The genuinely collision-prone singletons (git status by path, provider
accounts/usage, settings, sysstat) get keyed when multi-backend UI
lands. `--connect` becomes "add/attach endpoint", and the sidebar groups
projects under backend sections.

## 11. Team sharing (federation)

Chosen topology: **peer backends, not teammate devices.** A teammate's
backend holds one read-only, `public`-class, key-bound session against
ours, scoped to shared workspaces, and re-projects to its own devices.
Reviewed alternative (teammates' browsers connecting directly) is
simpler but puts N of their devices and credentials against our machine;
one revocable peer principal per teammate is the better boundary and is
what the owner wants.

- **Shared workspace** is the ACL unit; enrollment is the grant.
- **Read-only + fork.** No forwarded operations, no approval proxying,
  no peer-triggered spawns.
- **Payload contents need classification.** Thread payloads routinely
  contain `.env` contents, tokens echoed into logs, absolute host paths,
  and diffs of private config. "Read-only" does not mean "low
  sensitivity", and a peer is an *automated* client pulling in bulk. So:
  terminal-frame and file-content payloads are withheld or redacted by
  default with explicit opt-in to full fidelity; peer bulk reads are
  rate-limited and audited with per-peer attribution; the UI states
  plainly that enrollment is a **one-way disclosure** — un-enrolling
  does not un-share what was already pulled.
- **Fork** (designed at team-time, prepared now): the transfer is
  session file(s) + our thread data + git state (a bundle, since thread
  branches often exist only on the owner's machine and working-tree
  state is uncommitted). Guaranteed layer is context-seeded continuation
  that works regardless of provider version; native resume from
  transferred provider session files is the enhancement when versions
  match. Deliberately *not* synthesizing resume files from SQLite rows —
  that would require the store to become a full-fidelity event store
  (violating principle 3) and is untestable under the no-real-provider
  invariant.
- Reachability: Tailscale node sharing (cross-tailnet, no merge),
  Funnel/Cloudflare URL, or LAN.

## 12. Consequences for existing principles

- **SQLite stops being purely a cache.** Identity (users, devices,
  sessions, audit) is authoritative — losing the DB costs identity, not
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

Complete coverage has to be structural, not a promise. The current
counter-example: `/design/` serves agent-written files from the SPA
origin with **no token, no response headers, no per-thread check, and
symlinks unresolved** — an entire HTTP surface sitting outside the
authorization model, found only because it was audited (see the
boundaries doc's findings, and §16 phase 0).

Every externally-reachable surface is enumerated in one place with four
declared properties: **listener** (which port/origin), **principal tiers
admitted**, **required scope**, **content-type posture**. The
enumeration is code, not prose, and a CI gate fails the build when a
route, event channel, or listener exists without an entry — the same
fail-closed pattern the method table uses. The enumeration and gate
land with phase 0 covering HTTP routes, listeners, and content origins,
so every later phase builds against the gate; the RPC-method and
event-channel classes join in phase 3 when the scope table generates.

Classes to enumerate:

- **RPC methods** — generated from `//ao:scope` annotations (§5).
- **HTTP routes** — bootstrap, WS upgrade, scoped RPC, auth/token,
  tickets, attachments, snapshots, design files, health/version.
- **Event channels** — required scope per channel, resolved into the
  connection's precomputed visible set.
- **Listeners** — loopback, LAN, tsnet, tunnel, plus the auxiliary
  loopback servers (design MCP, harness control, claudetui gateway,
  pprof) which must each declare that they carry no session credential.
- **Content origins** — anything serving bytes an agent or user
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
  one in-memory session-table lookup. **No SQLite query per RPC** — the
  in-memory table is authoritative for live checks, SQLite is durable
  backing, and revocation writes both synchronously (§4).
- **Signature work is bounded to establishment.** ES256 DPoP
  verification (~tens of µs) happens per HTTP request and per WS
  upgrade — never per frame. Per-frame proofs are explicitly rejected.
- **The DPoP replay guard is an in-memory TTL map** bounded by the
  proof freshness window with a hard size cap — not a file per proof
  (which would burn an inode and a syscall per request and need its own
  GC). Restart clears it; the window is short enough that this is
  acceptable and is documented rather than papered over.
- **Audit records privileged and auth events, not reads.** Auditing
  every RPC would write thousands of entries during streaming. Appends
  are buffered with periodic fsync and bounded rotation — never
  fsync-per-entry.
- **Draft sync is gated on there being another client.** With a single
  attached client, debounced draft events are pure waste; the existing
  `HasRemoteClient`-style gate generalizes to "more than one session
  attached". Same rule for any other convergence-only channel.
- **tsnet is opt-in and lazily initialized.** An embedded userspace
  WireGuard stack costs memory and keeps DERP connections alive; a user
  who never enables remote access must not pay for it. Same for the
  tunnel subprocess and the TLS listener.
- **CSP and security headers are constant strings** set from a
  prebuilt header block — no per-request construction.
- **Symlink-safe file serving** costs a few extra syscalls per *open*
  (not per byte), on a path served rarely.
- **Snapshot and attachment transfers ride HTTP**, not the WS, so large
  bodies never block the event socket or inflate the replay ring.

Phase 6's phone work (subscription narrowing, buffered deltas, scope
leases) is a net *reduction* in wire and CPU cost, not an addition.

## 15. Hard constraints

1. WebAuthn RP ID must be a registrable domain — no IPs, no `.local`;
   secure context required (localhost is dev-only). Pairing codes are
   the universal fallback.
2. One passkey ↔ one RP ID; related origins capped at 5.
3. iOS associated domains are baked at build/sign time — native-app
   passkeys only under a vendor-controlled (wildcardable) domain.
4. APNs requires the Apple Developer Program ($99/yr); FCM requires a
   free Firebase project; iOS Web Push requires home-screen install and
   is unavailable to EU-mode PWAs. Vendor push keys cannot ship in
   self-hosted binaries (§9).
5. cloudflared is subprocess-only; tsnet needs a control plane
   (Tailscale account or self-hosted Headscale).
6. Plain-HTTP LAN browsers lose WebAuthn, service workers, clipboard —
   **and non-extractable WebCrypto**, so they are bearer-only. There is
   no LAN-HTTP DPoP path.
7. A sleeping machine is unreachable; wake-on-LAN is out of scope. The
   app may offer a keep-awake-while-sessions-live inhibitor.

## 16. Phases

0. **Open content-isolation defects.** Independent of everything else
   and reachable today, in the desktop webview, with no remote feature
   enabled: the vendored streamdown `Link.svelte` relative branch
   (root-relative and protocol-relative hrefs render as live anchors,
   bypassing `transformUrl`), an anchor-navigation guard, `/design/`
   hardening (origin/content-type posture, response headers, symlink
   containment via `os.OpenRoot` as `internal/safecopy` already does,
   per-thread scoping, no directory listing), and a baseline CSP —
   strict in production, relaxed in dev (the Vite dev server injects
   inline styles regardless of HMR, so the split is not an HMR
   concession; disabling HMR is an independent preference). The boot
   credential moves out of script reach entirely: bootstrap exchanges
   the one-time `?t=` URL token for an HttpOnly cookie, strips the
   token from the URL, and the WS upgrade authenticates via cookie
   plus the §7 Origin allow-list — deleting the `sessionStorage` copy
   and `window.__AO_BOOTSTRAP__`. This is the same channel that
   carries session credentials from phase 2 on, not a stopgap. Also:
   `safeExternalURL` on the two unvalidated `PRStep.svelte` hrefs,
   either using or dropping the unused `dompurify` dependency, tests
   for `/`- and `//`-leading hrefs, a correction to the false claim in
   `frontend/CLAUDE.md`, and the §13 surface enumeration + CI gate
   seeded with HTTP routes, listeners, and content origins (the
   RPC-method and event-channel columns join in phase 3 when the scope
   table generates).

1. **Sync sweep + seams.** Emits, channels, gap entries, race handling,
   device attribution column, thread branch/remote/head recording,
   backend UUID, hello frame, multi-backend seams (§10).
2. **Identity core.** Schema (users/devices/sessions/audit), pairing
   with proof-of-possession + verification number, token exchange,
   rotating refresh with reuse detection, generalized ticket primitive
   (WS + HTTP), revocation with live teardown, recovery codes, device
   management UI, rate limiting, webview/WSL credential forwarding,
   ui_state device binding.
3. **Authorization.** Annotation-driven generated method table, scope
   tiers + binding enforcement + step-up set, event visibility, settings
   key→tier taxonomy, capability-driven frontend, `LocalOnlyMethods`
   derived then deleted.
4. **Settings storage.** Host JSON / user+device in `ui_state`,
   migrations, per-class defaults.
5. **Serve mode, endpoint, TLS, tsnet, passkeys, remote update with
   rollback, provider remote re-auth.** DPoP mandatory here (the token
   endpoint accepts thumbprints from phase 2 so nothing reworks).
6. **Phone preparation.** Subscription narrowing, buffered deltas, scope
   leases, reduced snapshots, attachment flows, push senders +
   notification semantics + deep links.
7. **Multi-backend UI.** Keying the collision-prone singletons, sidebar
   sections.
8. **Team sharing.** Shared workspaces, peer sessions, payload
   sensitivity tiers, fork pipeline.

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
  scope-lease transitions (off→on, TTL lapse mid-turn, visibility flap
  — transitions, not just states), later a two-backend fixture.
- **Playwright**: pairing UX end-to-end, second browser context as a
  second device, capability-gated UI.
- Every refusal path gets a test.

## 18. Decisions still open

1. Whether `access:admin` exists as a standing remote scope at all, or
   whether every admin action requires step-up.
2. Push distribution posture (§9) — direct for personal builds is fine;
   the distributed answer must be chosen before public release.
3. How much of the payload-sensitivity machinery (§11) is built at
   team-time vs. designed-only now.
4. Whether draft "edited on <device>" and presence-aware routing survive
   at all (marked cuttable).
5. Whether the public-path ceiling (§2) should exclude anything beyond
   the step-up set — e.g. whether `terminal:operate` over a public
   tunnel is acceptable given it is already key-bound and TLS-wrapped.

Settled in review: approvals are never gated on the owner's own devices;
terminal access is not withheld from native clients by device class;
scope narrowing is per-device and opt-in, never imposed; the boot
credential rides an HttpOnly cookie with an Origin-checked WS upgrade
from phase 0 rather than deferring that shape to the session work; the
CSP is strict in production and relaxed in dev, with HMR removal an
independent preference rather than a CSP prerequisite; the surface
enumeration gate lands in phase 0, not phase 3.
