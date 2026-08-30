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
  thereafter. No tailnet is required; tailnet/tunnel exist for
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
| public: Funnel, Cloudflare tunnel | full **minus the step-up set**, and `public` binding required |

### Effective scopes

```
effective = granted(device) ∩ ceiling(principal tier) ∩ ceiling(network path)
```

Resolved **once**, at session establishment, into a precomputed set
carried on the connection. Every surface (WS RPC, HTTP RPC, event
push, attachments, snapshots, design files) authorizes from that one
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
accountability**, not constraining capability the machine already
granted. Design effort goes to the gate; the boundaries doc lists what
is deliberately out of scope.

## 3. Identity model

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

### Sessions

- **Access tokens are short-lived** (minutes–hours), always
  key-bound for `device-bound`/`public` classes.
- **Refresh is rotating with reuse detection**: each renewal issues a
  new refresh secret and invalidates its predecessor; replay of a spent
  refresh forks the family, **auto-revokes the whole family**, and
  alerts the owner. This is how a leaked credential is detected; a copy
  cannot renew indefinitely alongside the real device.
- Refresh binds to the device key on every listener. A bare bearer
  token on its own cannot self-renew.
- Browser class: short TTL, non-renewable without passkey re-auth where
  passkeys are available.

Every authentication failure carries a **closed, typed reason code**
end-to-end (missing/malformed proof, key mismatch, time window,
invalid signature, …), with signature checked *before* the time
window so a forged proof is never misreported as clock skew, and one
client-side presentation module mapping codes to actionable hints
("check automatic date & time on both devices" for skew — the
dominant real cause). Adapted from t3code's DPoP-failure rework.

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

A per-call fresh passkey (or host-presence) proof, never an ambient
standing scope, is required for: minting pairing links, network bind /
exposure changes, provider custom-env writes, MCP config writes, WSL
distro preference, and remote update triggering (§7). Optional step-up
is theater; these are the calls that re-key the system or re-route every
prompt.

### Local clients

The embedded webview drops `?t=`: at boot the backend mints an implicit
`loopback-only` device session delivered over the existing fd/stdout
bootstrap. The WSL launcher **forwards that credential** rather than
relying on apparent loopback origin. With topology no longer
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
trusts the client's capability object**. Every RPC re-checks
server-side; hello-frame flags are compat hints, never authorization.

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
  `settings.Service` + `atomicfile`.
- **User and device tiers live in the `ui_state` table**, which already
  exists for exactly this shape (and already migrated pane layout out of
  settings). User tier = `user:<id>` scope; device tier = `device:<id>`
  scope, with typed validation over the same store. Device rows cascade
  on device deletion. Revoking a device drops its state for free.
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

Multi-machine convenience without a sync engine: host- and user-tier
settings are per-machine by design (divergence is a feature), but the
settings UI shows which machine is being edited and offers "apply to
all / selected machines" on eligible keys — the client fans the same
write out per backend; no cross-backend settings replication exists.

The **key→tier taxonomy lands in phase 3**, with the scope table:
device-tier writes ride a valid session (they touch only `device:self`),
user-tier writes need `settings:write`, host-tier needs step-up. Phase 4
is then pure storage migration with no scope churn.

## 7. Transport, reachability, TLS

### Multi-listener, one session store

Loopback (webview, CLI), optional LAN bind, optional tsnet listener,
optional tunnel-fronted. Sessions are valid across listeners **subject
to their binding class** (§2). Local clients never hairpin through the
tailnet, and a soft listener cannot launder a strong credential into a
weaker presentation.

Cross-origin defense is explicit: strict Host allow-list (canonical
domain + known loopback names), Origin / `Sec-Fetch-Site` checks on
`/ws` and every auth endpoint, DNS-rebinding rejection.

Listener and endpoint-advertisement init is **per-listener isolated**:
one integration failing to start (a broken `tailscale` binary on
PATH, a dead tunnel) degrades that listener only and surfaces its
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
the same way, no tailnet involved. Tailscale/Funnel are for
*off-network* reach only, never a LAN prerequisite. The one
platform-imposed limit (constraint 6): plain-HTTP LAN **browsers** are
bearer-only: no passkeys, no service workers. The desktop app, CLI,
and native phone app are unaffected; they hold device keys, are not
subject to browser secure-context rules, and get encrypted TLS with no
domain at all via cert pinning anchored in the pairing payload (see
TLS below). Wanting passkeys in a LAN browser is the one thing that
requires the DNS-01 owned-domain path: real HTTPS on a private
address, still no tunnel. It is an optional upgrade, never a
dependency.

### TLS (in-app termination)

Two supported paths; others are documented escape hatches, not built:

1. **Owned domain + DNS-01**. DNS record → LAN IP (public DNS may hold
   private addresses), Let's Encrypt via DNS-01, backend renews. Real
   HTTPS on a LAN-only path, valid passkey RP ID, no tunnel.
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

Escape hatches: private CA (mkcert-style, manual trust), cloudflared
subprocess with an owned domain. The chosen HTTPS name is the backend's
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
  over LAN, tailnet, and tunnel alike; the dev server needs no
  `--host` flag and never binds beyond loopback.
- **Reachable ports are an allowlist, never arbitrary**: ports the
  dev-server scanner attributed to this thread's sessions, plus ports
  the user adds explicitly. A localhost proxy that forwards anywhere
  reaches every host-local service on the box; this one forwards only
  to declared dev servers. Gateway access requires an execute-tier scope.
- **The gateway is its own origin**, the same posture `/design/`
  acquires in phase 0 (today `/design/` is a route on the SPA's own
  mux, host, and port — that is the defect, not the model): proxied
  content is agent/app-authored and never shares the SPA origin; the
  session credential is never visible to it — access rides a
  short-lived ticket bound to the gateway origin.
- Detection reuses t3code's proven shape, server-side: enumerate
  loopback listeners (`lsof`/PowerShell), publish only candidates
  whose bounded 1s probe returns HTML or a redirect, cache probe
  results (~15s), poll (~3s) only while something subscribes,
  attribute PIDs to the owning thread's sessions.

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
launcher) with a stated headless credential-storage posture. Keychains
frequently cannot unlock without a login session, so the signing key,
provider credentials, and tsnet state need a defined at-rest strategy
for unattended boot.

**Session lifecycle on an unattended host.** Today `ArchiveThread` is
a DB flag only — it never closes the thread's provider session, and
the idle reaper deliberately skips sessions with running background
tool calls. Locally, app shutdown eventually reaps what that leaves
(provider processes are group-killed); on a headless host nothing
ever does, so an archived thread's dev server and its ~hundreds-of-MB
provider process burn forever. Required (t3code fixed the same class,
#5774): archive stops the session — the group kill cascades to dev
servers and monitors — with a stop-time re-check that the thread was
not re-engaged in the gap. The reaper's keep-alive-while-working
choice stays (killing quiet-but-working sessions is rejected
doctrine); what an unattended host adds is **visibility and control,
not timeouts**: a running-background-work inventory (which thread,
what, since when) with per-task and per-thread stop controls from any
attached client. The per-item data already exists — `store.Item`
carries thread, tool, summary, status, parent, and timestamps — but
every entry point today is thread-scoped, so this needs one new
cross-thread bound method. It must union the same three sources
`ListLiveBackgroundTasks` does (the store query, live Codex subagent
launches, and the triage layer's in-memory Codex unified-exec tasks,
which exist in no table), because a query written against SQLite
alone silently under-reports. The tray's 2-second completed-sibling
retention is a live-tray tuning value, not an inventory history;
the inventory reports what is running now.

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
- **Device attribution** on persisted mutations (which device did it).
  A trivial column now, required later for audit and shared-thread
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
  future-dialect fixture.
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
  and re-stamps). Obligations still open: purge on sign-out and
  device revocation — today **no code path ever deletes a replica
  database**, and a backendId change orphans the old
  `ao-replica-<oldId>` database on the origin forever, outside the
  per-database caps — and the resume ladder becomes replay-ring → windowed
  replica diff → full snapshot, in that order. At rest: the phone
  replica is encrypted with a key held in native secure storage
  outside the webview (biometric-gateable); browser profiles cannot
  do this. Revocation is not remote wipe. Cutting a device's access
  does not un-disclose what its replica already held (boundaries
  doc).
- **Reconnect discipline** (two t3code patterns adopted; unbuilt
  today, phase 1). Current behavior is the opposite of the target:
  on socket close `wsClient` rejects every in-flight RPC with one
  shared `DisconnectedError('socket closed')` — no suspension queue,
  no per-call cause, and no app-layer retry wrapper anywhere, so a
  reconnect that takes 200ms still surfaces as a failed call. Target:
  every in-flight query/RPC derives from one canonical
  connection-state observable — transient states (connecting,
  backoff) *suspend* pending work so it re-runs on the next
  "connected", while terminal states fail it with the preserved
  underlying cause, never a generic message. And retry-on-terminal is
  only ever a small explicit allowlist scoped to a known transient
  window (e.g. an authentication refusal in the seconds after a
  server update restart), not a blanket policy. On a flaky link this
  is the difference between an app that pauses and one that throws.
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
  phone/unattached path). This needs an audience change, not just a
  preferences UI: `NotificationSend` and `NotificationActivated` are
  loopback-only channels today, so an attached LAN browser receives
  neither. `NotificationSend`'s retained (non-ephemeral) retention
  stays — the Windows launcher replays it by cursor after reconnect.
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

Decide the **seams** in phase 1, not a speculative store rewrite:

- Document and enforce global uniqueness of thread/project ids (already
  UUIDs) so most stores need no re-keying.
- `bindings.ts` routes RPCs through a resolvable transport handle
  rather than importing a singleton.
- Event fan-out carries connection origin (backend UUID).
- The IndexedDB thread replica keys its stores by backend UUID so two
  backends' threads can never collide in one browser profile.

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
backend holds one read-only, `public`-class, key-bound session against
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
- Reachability: Tailscale node sharing (cross-tailnet, no merge),
  Funnel/Cloudflare URL, or LAN.

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

Complete coverage has to be structural, not a promise. The current
counter-example: `/design/` serves agent-written files from the SPA
origin with **no token, no response headers, no per-thread check, and
symlinks unresolved**. That is an entire HTTP surface sitting outside
the authorization model, found only because it was audited (see the
boundaries doc's findings, and §16 phase 0).

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
  tickets, attachments, snapshots, design files, health/version.
- **Event channels**: required scope per channel, resolved into the
  connection's precomputed visible set.
- **Listeners**: loopback, LAN, tsnet, tunnel, plus the auxiliary
  loopback servers (browser MCP, design MCP, harness control,
  claudetui gateway + hook relay, pprof, the `--connect` client stub)
  and the **implicit** ones our own child processes open — chromedp
  gives every managed Chrome a loopback DevTools port, which no
  inventory named until this audit. Each declares what capability it
  carries and how it authenticates, not merely that it holds no
  session credential: the browser MCP endpoint carries page
  evaluation and workspace file reads behind an unguessable path
  alone, which is a larger grant than "no session credential"
  suggests. A listener whose credential is weaker than the surface it
  gates is the pattern the enumeration exists to make visible. The
  starting inventory is 12 listeners across 6 packages, verified
  2026-08-30.
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
  tunnel subprocess and the TLS listener.
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
5. cloudflared is subprocess-only; tsnet needs a control plane
   (Tailscale account or self-hosted Headscale).
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
   enabled. Every item below was re-verified against this tree on
   2026-08-30. Two entries left the list that day: the markdown
   renderer's relative-href branch (fixed at the render layer, both
   render paths verified) and a correction to `frontend/CLAUDE.md`
   (the claim it was going to correct now lives in a code comment and
   is true against current code). Two more turned out already shipped
   and are recorded in §7 and §9 rather than here: the persisted
   stable port and the backend-keyed replica.

   - **`/design/` sits outside the authorization model.** It is a
     route on the SPA's own mux, host, and port, with no token, no
     per-thread check, directory listings on (so a GET of `/design/`
     enumerates every thread id on the install), no response headers,
     and `http.Dir`'s symlink following intact — an unauthenticated
     read of anything the app process can read, available to any
     loopback peer. Fix: its own origin, a per-thread capability
     token, containment via `os.OpenRoot` (the primitive
     `internal/safecopy` already uses), listings off, and the
     `WriteSecurityHeaders` block the SPA route already gets.
     Without a domain, "its own origin" means its own loopback
     listener on its own pinned port, and every internal consumer of
     the design URL moves with it (the screenshot capture path builds
     that URL today). One interaction to get right, because it makes
     the Origin allow-list load-bearing rather than defense in depth:
     **cookies are scoped by host, not by port**, so a document on
     the design origin still has the boot cookie attached to
     requests it makes to the SPA origin — including a WS upgrade,
     which CORS does not cover. The upgrade must therefore refuse
     any `Origin` outside the allow-list, and the design origin is
     never in it.
   - **Anchor-navigation guard.** `handleExternalLinkClick` returns
     *without* `preventDefault` when `safeExternalURL` yields null, so
     a non-`http(s)` href performs its default navigation. The
     markdown renderer can no longer emit such an anchor, but this is
     app-wide policy that every other component inherits, and today
     it is neither fail-closed nor documented as intentional.
   - **`PRStep.svelte`** binds two forge-derived hrefs with no
     `safeExternalURL`, while the two sibling consumers of the same
     field (`PrBadge.svelte`, `GitActionsControl.svelte`) both
     validate. Both layers of defense are absent at once here.
   - **A baseline CSP**, strict in production and relaxed in dev.
     There is no CSP anywhere in the product today. The Vite dev
     server needs inline styles regardless of HMR, so the split is
     not an HMR concession; disabling HMR is an independent
     preference.
   - **The boot credential moves out of script reach.** Bootstrap
     exchanges the one-time `?t=` URL token for an HttpOnly cookie,
     strips the token from the URL, and the WS upgrade authenticates
     via cookie plus the §7 Origin allow-list, deleting the
     `sessionStorage['ao:bootstrap-token']` copy and
     `window.__AO_BOOTSTRAP__`. This is the same channel that carries
     session credentials from phase 2 on, not a stopgap.
   - **The `--connect` client stub hands out that same credential.**
     Its loopback listener serves the injected `__AO_BOOTSTRAP__`,
     upstream token included, on an unauthenticated `GET /`. Same
     credential shape, so it is fixed in the same change.
   - **The two MCP endpoints authenticate on an unguessable path
     alone** — no `Origin`/`Sec-Fetch-Site` rejection and no
     loopback-peer re-check (the claudetui gateway already does the
     peer check; copy it). Lazy-starting these listeners is not
     available: the URL rides provider argv at spawn, so it must
     exist before any tool is called. The two checks are the fix.
   - **Managed-browser navigation policy.** `Manager.Open` accepts
     loopback URLs, which is right for dev servers and wrong for the
     app's own ports: as written, a page in the in-app browser is a
     loopback peer of the app, and an agent can reach any thread's
     `/design/` workdir through it, around `browser_open_file`'s
     workspace containment. Deny our own transport and auxiliary
     ports; leave dev-server ports alone.
   - **The two Chrome launchers disagree on sandbox posture.**
     `internal/screenshot` disables the OS sandbox while rendering
     agent-authored HTML; `internal/browser` explicitly refuses the
     same flag and documents why failing to launch is the better
     outcome. Same class of content, so align on the stricter
     posture.
   - **Tests**: a `//`-leading link href through `ChatMarkdown` (the
     `//`-versus-schemeless discrimination exists in both render
     paths and is pinned in neither for links), the delegate's
     behavior on a non-`http` anchor, and a link-level differential
     between `staticHtml.ts` and `Link.svelte` — `markdown/AGENTS.md`
     already names that silent fork as a known hazard.
   - **The §13 surface enumeration + CI gate**, seeded with the
     verified inventory: 12 listeners including the implicit Chrome
     DevTools ports, the transport's routes, and content origins. The
     RPC-method and event-channel columns join in phase 3 when the
     scope table generates.
   - **Doc drift inside the classification table.**
     `internalmethods.go` describes six categories; the map carries
     nine and 269 entries. Anything citing "six" is stale.

1. **Sync sweep + seams.** Archive-closes-session fix (§7 — a
   standing leak today, acute once hosts are unattended). Emits,
   channels, gap entries, race handling,
   device attribution column, thread branch/remote/head recording,
   backend UUID, hello frame, multi-backend seams (§10). Reconnect
   discipline lands here (§9): it is client-transport work that every
   later surface depends on, and today's blanket
   fail-every-in-flight-RPC is what a flaky link would surface. So
   does the replica's missing lifecycle — nothing deletes a replica
   database today, and a backend-id change orphans the old one on the
   origin permanently — and §9's forward-tolerance obligation with
   its future-dialect fixture.
2. **Identity core.** Genuinely N-user from the start, with no implicit
   single owner anywhere in queries, session checks, or audit
   attribution (hub deployments depend on it; §11). Schema
   (users/devices/sessions/audit), pairing
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

1. Whether `access:admin` exists as a standing remote scope at all, or
   whether every admin action requires step-up.
2. Push distribution posture (§9). Direct for personal builds is fine;
   the distributed answer must be chosen before public release.
3. How much of the payload-sensitivity machinery (§11) is built at
   team-time vs. designed-only now.
4. Whether draft "edited on <device>" and presence-aware routing survive
   at all (marked cuttable).
5. Whether the public-path ceiling (§2) should exclude anything beyond
   the step-up set, e.g. whether `terminal:operate` over a public
   tunnel is acceptable given it is already key-bound and TLS-wrapped.
6. Hub-thread operability (§11): whether shared-workspace threads on a
   team server are operable by members via workspace roles (personal
   backends stay read-only + fork regardless), and who may answer
   approvals on a hub thread: any member holding the scope, the
   thread starter, or a role gate.

Settled in review: approvals are never gated on the owner's own devices;
terminal access is not withheld from native clients by device class;
scope narrowing is per-device and opt-in, never imposed; the boot
credential rides an HttpOnly cookie with an Origin-checked WS upgrade
from phase 0 rather than deferring that shape to the session work; the
CSP is strict in production and relaxed in dev, with HMR removal an
independent preference rather than a CSP prerequisite; the surface
enumeration gate lands in phase 0, not phase 3.
