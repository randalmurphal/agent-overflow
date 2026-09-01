# internal/app/

This package is the importable application shell. It owns `App`, all Wails-bound
facades and wire DTOs, service composition/lifecycle, explicit cross-owner
transactions, harness adapters, and application integration tests. Domain and
provider behavior still belongs in the narrower `internal/` packages mapped by
the parent guide.

## Wails compatibility

Root `service.go` declares a named `main.App` that embeds `*app.App`. Wails v3
and Go reflection include the promoted method set while retaining the wrapper's
`main.App.<Method>` identity. Do not replace the named wrapper with a type alias,
register `*app.App` directly, or change the explicit transport registration
labels: any of those changes every method ID.

`internal/transport/methodgen` scans this directory but hashes methods under
`main.App`. Every bound method here carries an `//ao:scope <name>` directive in
its doc comment (plus `//ao:stepup` where the spec requires a per-call proof);
the generator fails the run without one, and that scope is the whole of what
`transport.AuthorizeSessionMethod` compares a caller's grants against — there
is no second, hand-listed reachability table.

It also carries a ROUTE, which answers a different question: not "may this
caller do it" but "on which machine" (`docs/specs/remote-access.md` §10). A
method whose first non-context parameter is named `threadID` or `projectID`
needs no directive — those two ids are unique across backends
(`internal/entityid`), so the generator infers `thread` / `project` from the
signature. Every other method declares `//ao:route home|selected|all`, and the
generator fails the run without it. The vocabulary and what each value means
are in `internal/transport/AGENTS.md` § The Route column; the client is the
only consumer, through the generated mirror at
`frontend/src/lib/transport/methodRoutes.ts`.

A bound method addition/removal must annotate BOTH, regenerate Wails bindings
with `-ts`, run `make methodgen`,
and verify all existing `$Call.ByID` values remain stable unless a wire migration
was explicitly approved.

App-owned generated models live in
`frontend/bindings/agent-overflow/internal/app/models.ts`. Never edit generated
bindings by hand.

## Bootstrap boundary

Executable-only inputs cross through `bootstrap.go`: build version, data-dir
override, harness/soak isolation, provider control environment, notification
adapters, updater configuration, backend identity, and window geometry. Keep
these as narrow package functions rather than exported `App` methods—an exported
method is a candidate wire RPC unless explicitly ignored.

The complete mocked-provider isolation set belongs in `ConfigureIsolation`.
Tests must never spawn real Claude/Codex binaries or touch real provider homes;
use the existing guarded fixtures and mock scripts.

## The session core meets the wire here

`app_identity.go` is where `internal/identity` and `internal/transport`
meet, because neither may import the other. It holds the one
`*identity.Sessions`, satisfies the five hooks the transport declares
(`SessionForRequest`, `SessionLive`, `SessionScopes`, `StepUpProof`,
`PageSessionCredential`), and implements `transport.AuthEndpoints`.

Everything in that file is adaptation. **No policy decision belongs
there** — a decision made in the adapter is one the session core could not
enforce for a caller that reached it another way; put it in
`internal/identity` and call it from here.

- `initIdentity` runs from `Start` after the store opens, and is
  **deliberately not fatal**. The launch credential still authorizes every
  request, so an App whose identity core failed serves the local page
  exactly as it did before this existed; what it loses is attribution and
  revocation. Refusing to boot would turn a credential-table problem into
  "the app does not start". The loss is confined to this machine on its
  own: no session resolves, so the transport's peer rule refuses every
  off-host `/ws` upgrade rather than admitting one nothing could revoke.
- Every accessor answers honestly for an App that never called `Start`.
  Test fixtures build one directly, and nil `identityState` means
  "identity is not wired" — a state, not a fault. The one asymmetry is
  deliberate: a request presenting NO session credential proceeds naming
  none, while one presenting a credential nothing can judge is REFUSED,
  because proceeding would name a session this process cannot revoke.
- `bindingAdmitsPeer` is the ONE comparison of a session's binding class
  against the peer presenting it, and this file is the only place in the
  tree that can host it: `internal/identity` never sees a request and
  `internal/transport` may not import the vocabulary. Every presentation
  path runs through `SessionForRequest`, so `/ws`, the manifest fallback
  and `/auth/ticket` inherit it. A `loopback-only` credential presented
  off-host resolves NO SESSION rather than refusing the request — the
  full argument is in `internal/identity/AGENTS.md` § Binding class.
- `AuthEndpoints(a *App)` is a bootstrap-boundary function returning an
  unexported adapter type, **not** a set of exported `App` methods. An
  exported method on `App` is promoted onto `main.App` and becomes a wire
  RPC by construction (see § Bootstrap boundary); redeeming a pairing link
  over the RPC wire would let a caller who already holds a session enroll
  another device, which is the one thing the HTTP route's shape exists to
  constrain. Anything the transport calls through an interface belongs on
  a type of its own for the same reason.
- **The passkey seam is split by who the caller is, and that split is the
  point.** SIGN-IN goes through this adapter, because its caller holds
  nothing; REGISTERING a credential and PROVING step-up are bound methods,
  because their callers already hold a session and registration must never
  be reachable without one — it is what a later sign-in trusts.
  `app_passkey.go` holds the two facts neither `internal/identity` nor
  `internal/transport` can know: what this backend answers to
  (`passkeyRelyingParty`, from the canonical domain plus the live
  listener's port, re-read per ceremony because the domain is a live
  setting) and `StepUpProof`, the hook the per-RPC gate spends a token
  through.
- The local page credential is cached and re-issued within
  `localReissueMargin` of expiry, rather than on a timer. The manifest is
  refetched on every reconnect, so the moment a fresh credential is needed
  is also the moment somebody asks for it — and a bootstrap fetch must not
  write to the database on the ordinary path.

## Argument-dependent authorization

`app_authz.go` holds the rechecks a method's `//ao:scope` annotation cannot
express, because the annotation classifies a method NAME and these authorities
are decided by what the call CARRIES (`docs/specs/remote-access.md` §16 phase 3:
"the annotation is the FLOOR").

- `requireAutonomy` — running in `auto` / `auto-accept-edits` / `full-access`
  needs `threads:autonomy`. **It judges the EFFECTIVE mode, never the literal
  argument.** §5 draws the boundary by outcome, so an omitted argument is not a
  free pass: `provider.DefaultRuntimeMode` is full-access, and a create that
  selects nothing lands there. The two resolution points:
  - **Create paths** hand the recheck down as `threadapp`'s
    `AuthorizeRuntimeMode` hook, called on the resolved mode after defaults
    apply and before the thread persists. A hook rather than a mode the caller
    resolved itself — re-deriving the resolution rules outside `threadapp`
    would be a second copy that silently disagrees the day one changes.
  - **Drive paths** (send, steer, queue) use `requireAutonomyForThread`:
    the override if one was selected, else the target thread's CURRENT mode.
    Sending into a full-access thread commits the agent to acting without
    approval gates just as surely as selecting it does. The thread read happens
    only when there is a session to judge and no override was given.

  `UpdateThreadRuntimeMode` and `UpdateNewThreadDefaults` always carry an
  explicit argument, so they judge it directly.
- `requireSettingsTier` — `UpdateSettings` carries all three of §6's tiers, so
  it is decided per patch key: device rides any valid session, user needs
  `settings:write`, host needs a step-up proof.
- `requireStepUp` — the host tier's rule, and the one helper that must NOT
  re-derive its answer. Two proofs satisfy step-up (host presence, or a
  passkey assertion), and the transport resolves both ONCE per call because
  resolving the second SPENDS a single-use token. So this reads
  `transport.StepUpProvenFromContext` — the gate's own answer — rather than
  asking again and finding the token gone.

Three rules hold for every helper here:

- **A call with no session context is ADMITTED.** That is every in-process
  caller (a background saga, a workflow phase, a test) and every
  launch-credential connection. Narrowing them would break the app for callers
  the wave was never about.
- **One helper set, never a copy per method.** A seventh mode-selecting method
  gets written by somebody who greps for how the sixth did it, and a per-method
  copy is how one of them ends up with a subtly different rule.
- **A refusal must not be confusable with a lookup failure.** Where the recheck
  needs a store read to resolve the effective mode, an unreadable thread or
  project PASSES — the method's own lookup answers a step later with something
  true, rather than telling a caller it lacks a scope when the real problem is
  a bad id.

Reaching the connection principal needs a leading `ctx context.Context` on the
bound method. That parameter is **stripped from the generated TS bindings**, so
adding one changes no wire signature and no method ID — but regenerate both
`methodgen` and the Wails bindings anyway, since the doc comment travels.

## The answer is narrowed too, not only the call

The rechecks above decide whether a call HAPPENS. `app_network.go`'s
`networkSettingsForCaller` / `networkSettingsForCallerWithLAN` decide what one
that happened may CARRY BACK, from the same per-call proof.

- **Why there is a second axis at all.** `GetNetworkSettings` is
  `access:admin`, because managing how a backend is exposed is what a paired
  admin device is for — a `host` annotation refused every one of them and left
  Settings → Network reachable from nowhere but the machine. But two fields of
  its answer are host-only whatever the grant: this launch's token, and the
  ticket-bearing share URLs. Widening the CALL and narrowing the ANSWER is one
  change, not a compromise between two.
- **One helper pair, and every method returning a `network.Settings` goes
  through it.** Including the two that are still `host`-scoped, where the gate
  already refused the off-host caller. One rule wherever the shape leaves the
  process beats a per-method judgement about whether some other check happened
  to cover it — and `SetNetworkSettings` is why: it is step-up reachable from a
  paired device, and its RETURN carried the launch token there until this
  existed. Two callers deliberately keep the full builder and say so at their
  call sites (`pairingPageURL`, `ServeEndpoints`).
- **The withholding itself lives in `internal/network`**, not here, and it
  works by never minting rather than by blanking — see that package's guide.

## Settings answer per caller

`GetSettings` and `UpdateSettings` are still one method each on one service,
but the DEVICE tier is resolved from the connection: `uiStateScreen`
(`app_uistate.go`) derives the caller's `ui_state` bucket exactly as
`uiStateScope` does — it is the same function — plus the CLASS of screen
behind it, and `settings.Service.For(bucket, class)` is the service seen from
there (`internal/settings/residency.go`). Two screens on one backend read two
font sizes and one shared set of confirmations, and a paired phone that never
touched `lowPowerMode` reads it on.

- **`settingsCaller(ctx)` is the ONLY way this package builds a per-connection
  settings view.** Both halves come from one derivation, so a bucket can never
  be paired with another screen's class — the failure that would look exactly
  like a phone whose owner changed the setting. A second derivation site is
  the thing to refuse in review.
- **A connection with no bucket is not an error here**, which is the one
  difference from `uiStateScope`. `GetUIState` with no bucket has nothing to
  answer with; settings always have an answer — the class defaults — and a
  background saga asking for settings must get them. A session the core
  REFUSES still errors, because that refusal is about the credential.
- **Only a paired device carries a class of its own.** A local page-channel
  screen and a sessionless caller both resolve as `desktop`, whose class row
  is empty — the local channel's device row describes the CHANNEL, which
  several distinct screens share, so its class could not describe any one of
  them, and the channel is loopback-bound so no phone reaches that branch.
  `settingsDeviceClass` converts the row's string and answers `desktop` for an
  unreadable one.
- **A backend-initiated device write attributes to the caller when there is
  one.** `recentWorkspaces` is written from thread creation, so the create
  paths carry `SettingsBucket` and `SettingsClass` down to `threadapp` as a
  pair; `callerSettingsScreen` is the non-failing variant they use, because
  losing the attribution is not worth failing the create. A genuinely
  caller-less write lands on the backend machine's own screen rather than
  being dropped.
- **A writer that reaches `ui_state` around the settings service owes it
  `InvalidateTierCache`.** There is exactly one — the harness reset's
  `ClearUIState` — and `harnessHost.ClearUIState` makes the call.
- **Backend code that acts on THIS machine's screen reads
  `settings.Service.BackendScreen()`, not `For("", …)`.** A bucket-less caller
  answers class DEFAULTS, which for a screen-facing preference means silently
  ignoring what the user set on the very screen being acted on. There is one
  such caller — the OS-notification gate — and a key with no screen behind it
  does not belong in the device tier at all.

## Notifications map from the event funnel

`app_notification_mapping.go` taps `emit` itself, not the six places that
announce a notification-worthy moment. `internal/notify` owns WHAT a moment
says and when it is taken back (pure, testable without an App); this file owns
WHERE each one is observed and how the sentence is finished.

- **Tap the funnel, not the emitters.** Turn completion is announced from the
  triage router, approvals from three places, provider status from a discovery
  service, sign-in from an account manager. Subscribing to the one funnel every
  Go→frontend event already crosses closes the class: a seventh emitter of
  `provider:approval` is mapped the day it is written.
- **Project synchronously, dispatch on the queue.** The tap type-asserts and
  reads the two or three fields the mapping needs before enqueuing, so the queue
  never retains a whole wire payload. The queue is a `serialqueue.Queue` rather
  than a bare `go` because ORDER is the retraction contract: a retract that
  overtook its own send would strand the notification forever.
- **`notifyOS` is the one preference gate**, and that placement is the point: a
  send that reaches a presenter without passing through it does not exist, so no
  sender can ship having forgotten to ask. Checking at each call site is a class
  of bug, not a bug.
- **A retraction is never gated.** The gate answers "may I interrupt you", and
  withdrawing something already on screen is the opposite. Gating it would let a
  toggle flipped mid-flight strand the very alerts it was meant to stop.
- **A notification body carries no content.** Titles are the thread's own,
  clipped; bodies are fixed phrases from `internal/notify`. The provider's
  stderr tail, the failed turn's error message and the approval's command line
  are all deliberately left behind in the tap.

## The device-access surface

`app_access.go` is the settings pane's half of the same core: which
devices hold credentials on this backend, the pairing calls that add one,
and the revocations that take one away. Adaptation on the same terms as
`app_identity.go` — no policy decision lives there — but it owns the wire
shape (flat DTOs, millisecond epochs, additive-only) and two refusals that
are facts about this process rather than about a row:

- **The local page channel is not a device a person may revoke.** That row
  is what the embedded webview, the WSL relay, and the `--connect` stub
  all present; revoking it signs the host's own window out. Both
  revocations refuse it, `RevokeAccessSession` by resolving the session's
  device first.
- **The overview shows the state that should not exist.** A session
  standing on a REVOKED device is carried, marked
  `SurvivedRevocation` — `store.RevokeDevice` moves both rows in one
  transaction, so reaching it means something wrote around that, and
  filtering it away with the ordinary revoked history is how a device
  that kept access goes unnoticed twice. `AccessSession.Scopes` rides
  along verbatim for the same "carry the fact, not a verdict" reason:
  what "view only" MEANS is one definition, and it lives on the page
  that already applies it to itself.
- **A revoke reports what it DID, not that it succeeded.**
  `RevokeAccessDevice` answers a `DeviceRevocationResult` — whether the
  device row moved, how many sessions ended, how many sockets closed —
  because "revoked, 2 sessions ended" and "already revoked, nothing was
  live" are different answers and the person who just lost a phone needs
  to be told which one they got. Reporting success uniformly is how a
  device that kept access went unnoticed (spec §2, incident 2026-08-31).
  Re-revoking is deliberately still allowed and still re-sweeps; the
  surface is where that becomes visible, not where it is decided.
- **`backend-peer` is not a class this surface mints.** Enrolling another
  backend is the federation flow with its own trust decisions; admitting
  one here would give it the posture of an owner's own device.
- **How much a link grants is chosen at mint, and only there.**
  `MintDevicePairing` takes an access level — `full` or `view-only`
  (`identity.PairingAccess`) — and the grant set it resolves to is the
  session's for that session's whole life. An EMPTY level is full: the
  parameter was appended to a call that already existed, so naming none
  asks for what the surface always did, and an unrecognized one is
  refused rather than widened. Re-narrowing a paired device is a fresh
  link, not an edit to a row.

Every method carries `//ao:scope access:admin`, which is what keeps the
set together: one annotation, so the surface moves as a unit. Two of them
add `//ao:stepup`, and they are the two that ISSUE: minting a pairing
link, and BEGINNING a passkey registration. A standing grant must not
make either — a session that could mint could enroll its way around its
own revocation, and a session that could register a credential could do
the same thing with a different kind of key. The FINISH deliberately
carries no second proof; `app_passkey.go`'s doc comment argues why one
would guard nothing and break a remote registration. The rest
answer a device the owner granted `access:admin`, which is what makes
revoking a lost phone from the other phone possible.
Minting ISSUES a credential, revoking withdraws
every credential a device holds, restoring re-admits a revoked
device's KEY to pairing without moving any credential (the revoked-key
redemption refusal names it as the remedy), and forgetting DELETES a
revoked device's row — refusing an un-revoked one, because revoking is
what ends access and this only removes the row it emptied
(`internal/identity/AGENTS.md` has both consequences: the key becomes
free to enroll again, and the audit rows stay); the overview read goes with them because
it carries the device map, the connection counts, the audit log, and a
pending pairing's verification number — which is only a check if the
owner is the only party comparing it.

`app_passkey.go`'s four `access:admin` methods join the same set on the
same terms, with one difference worth saying out loud rather than
inferring: **removing a passkey ends no session.** A session a passkey
signed in is an ordinary session on an ordinary device row, and it ends
the way every other one does — by revoking the device. `DeletePasskey`
carries no `//ao:stepup` for the argument every subtraction here makes
(it issues nothing, and the phone you can still reach must be able to
remove the credential on the one you cannot), and the UI copy has to say
what it DOES rather than imply a revocation. `ListPasskeys` carries
credentials that have OUTLIVED their domain — `Usable: false` where the
stored relying party no longer matches the current one — because an
authenticator still offers those, so hiding them would leave a person
unable to remove something they can still see.

Two shapes are worth knowing before editing:

- **`pairingDeadline` is not the link row's `expires_at`.** Once a device
  redeems, the deadline is the owner's confirmation window
  (`identity.PairingConfirmWindow`), because redemption mints the session
  under that window. A link filtered on its own five-minute expiry would
  drop the confirm affordance five minutes into a live ten-minute
  exchange. `pairingState` orders on the same rule: settled first, then
  redeemed BEFORE expired.
- **A pairing URL carries a page ticket and no `cid`.** The ticket is what
  lets a device holding no credential load the page at all; `cid` is this
  install's durable UI-state identity, and stamping it on a link for
  somebody else's phone would point that phone at this machine's bucket.
  The payload rides the URL FRAGMENT, which is never sent to a server.

## The other machines this installation drives

`app_backends.go` is the mirror image of the device-access surface: that
one is "who may reach THIS backend", this one is "which other backends do
I reach". Four methods — `ListBackends`, `AddBackend`, `RemoveBackend`,
`RenameBackend` — all `//ao:scope host` and `//ao:route home`, all thin
adapters over `internal/attachedbackends`.

`host` scope is the whole access rule and needs no second one: host
presence is the only key that opens a host method, and no session grant
does. It matters more here than almost anywhere on the wire — a caller
that could attach a backend could point this installation at a machine of
its choosing, and a caller that could list them learns every computer this
person works on. `home` route because they act on THIS backend's own
profile directory; asked of an attached backend they would answer about
that machine's attachments, which is a real thing to want one day and not
what this surface is.

**`AddBackend` returns before the pairing admits anything, and that is not
a shortcut.** The confirmation window is ten minutes
(`deviceclient.AwaitActivation`), longer than any timeout between here and
the page, so the call answers the verification number a person has to
compare and the wait runs on its own goroutine, reporting on the
`backend:attach` channel. The same split the terminal ceremony makes
between printing the number and waiting for it. The wait runs on `appCtx`,
so a shutdown mid-wait ends it rather than leaving a pinned TLS transport
open for ten minutes.

A boot with no resolvable config root has no manager, and all four answer
that as a plain refusal rather than panicking. The transport is handed a
nil interface in that case (`attachedBackendsSeam` in `main.go` — a typed
nil in an interface is not nil), so the carried routes are absent rather
than serving 404s from an empty set.

## The canonical domain's certificate

`app_domaincert.go` is the one place that decides WHERE this backend's
domain certificate comes from and WHEN it is obtained. `internal/acmecert`
knows how to order one and `internal/transport` knows how to present one;
neither knows about settings, a schedule, or the other's existence, and
this file is the seam.

- **One goroutine, one loop, one status.** `startDomainCertificateLoop`
  runs `reconcileDomainCertificate` and sleeps on the wait it returns.
  Boot, `SetNetworkSettings`, and `RenewCanonicalDomainCert` all KICK it
  and read the status it left behind; none of them does certificate work
  inline. That is not tidiness — a DNS-01 exchange waits on record
  propagation and outlives any RPC timeout the frontend has (60s), and a
  renewal three months from now has no call to attach to at all. The
  screen polls `GetNetworkSettings` while `tls.renewing` is set.
- **The branch order IS the policy**, and it is the settings file's
  precedence made executable: no domain clears the slot; an external
  cert/key pair is loaded and nothing is ordered; a hook orders and
  renews; anything else clears. The fourth branch is a real deployment,
  not a fallthrough — a domain with neither hook nor pair is somebody
  else's proxy terminating TLS in front of us.
- **A failure keeps the certificate that is serving.** Backoff runs
  between `domainCertRetryFloor` and `domainCertRetryCeiling` because a
  broken DNS hook is usually still broken in five minutes and a CA counts
  failed orders against a rate limit. The error is verbatim, names its
  stage, and is cleared by the next success — user-facing state, not a
  log line (root `AGENTS.md` principle 5).
- **The external pair is re-read only when its bytes could have changed**,
  keyed on a `(size, modtime)` stamp. Deliberately not a filesystem watch:
  an outside tool renews monthly at most, and a watch costs a descriptor
  plus a wake-up per unrelated write in that directory.
- **`RenewCanonicalDomainCert` carries `//ao:scope host` and NO
  `//ao:stepup`.** It takes no argument and changes no configuration — it
  re-runs what the daily timer would have run anyway, against settings
  that were themselves written through the step-up-gated
  `SetNetworkSettings`. Demanding a second proof would gate the RETRY of
  an act that was already proved.
- **Tests reach no network and no real CA.** The order flow is driven
  through `acmecert`'s narrow `CA` interface; what is live-only is a real
  Let's Encrypt issuance, and the "Check certificate now" button is how
  that gets checked.

## The tailnet node

`app_tailnet.go` is the same seam one layer over: `internal/tailnet` knows
how to be a node and `internal/transport` knows how to serve a listener,
and neither knows about settings. It follows the domain-certificate loop's
shape deliberately — one goroutine, one `reconcileTailnet` returning its
own next wait, boot and `SetNetworkSettings` KICKing rather than working
inline — because bring-up waits on a person approving the machine and
outlives every RPC timeout there is. The screen polls `GetNetworkSettings`
while the feature is enabled and the node is not yet Running.

- **The loop's wake-ups include the NODE's own event channel**, re-read
  every pass rather than captured once. A closed node's channel is closed,
  and a closed channel selected on forever is a spin.
- **Listeners are attached only while the node is Running**, and the
  numeric port is the SAME one the main bind uses. One port for one
  backend keeps the share URL, the cookie name and the origin rule
  deriving from the same authority they always did.
- **`SetAuxiliaryHosts` is set after a listener exists and cleared when
  one goes away**, so the Host guard never admits a name nothing serves.
- **HTTPS is attempted only when the node reports certificate domains**,
  which is the tailnet admin panel's answer and not ours to substitute
  for. No domains means cleartext over WireGuard, and the status says so
  rather than failing.
- **A tailnet failure is that listener's failure and nothing else's.** It
  lands in `network.TailnetStatus.LastError` verbatim, backs off between
  `tailnetRetryFloor` and `tailnetRetryCeiling`, and never reaches
  `Server.ServeErr` — the spec's rule that one integration failing
  degrades only its own path.
- **`ForgetTailnetNode` carries `//ao:scope host` and NO `//ao:stepup`.**
  Every act it can perform is a DELETION of local state, it refuses while
  the feature is enabled so it cannot race the reconciler, and turning the
  feature off went through the step-up-gated `SetNetworkSettings` first —
  the same argument `RenewCanonicalDomainCert` makes.
- **Tests reach no control plane and no network.** `internal/tailnet`'s
  rig runs an in-process control server; what is live-only is a real
  Tailscale sign-in, a real `ts.net` certificate, and DERP-relayed reach
  between two machines.

## The activation gate

`app_activation.go` is the backend half of `internal/supervise`: a boot that is
a supervisor TRIAL must prove it works before anything commits to it, which
means booting FULLY — store, migrations, transport bind, ready — and answering
RPCs, while taking no action of its own.

- **One gate, one waiter.** `Start` hands the whole unattended set to
  `activation.Run` as a single function (`startUnattendedWork`). Not a flag per
  subsystem: a second boolean is how the eleventh subsystem gets added without
  one.
- **The zero value is OPEN**, so every boot that is not a trial never touches
  this file. `Run` then calls its function inline, in `Start`'s own goroutine,
  in the same order as before the gate existed — a startup failure is still a
  boot failure and nothing about the ordinary boot moved.
- **Membership rule for the parked set**: *if this ran and the update were
  rolled back, would restoring the database undo it?* A rollback restores the
  SQLite triple and nothing else, so anything that touches the world outside it
  waits — background git fetch, provider status and account probes, rate-limit
  probes, the idle-session reaper, the retention sweep (which deletes attachment
  FILES), the ACME reconciler, the tailnet node, and workflow autoresume plus the
  scheduler. Serving RPCs while parked is CORRECT; that is what "prepared" means.
- **A git READ is not an action.** `backfillProjectIdentity` runs unparked, in
  its own goroutine, because its whole effect is two TEXT columns in SQLite —
  inside the snapshot boundary — and `git remote get-url` / `git rev-list`
  change nothing. The rule is about what a rollback cannot undo, not about
  whether a subprocess was spawned.
- **`startWorkflowAutomation` is split out of `initWorkflowEngine`** for that
  reason: the engine must exist for RPCs to answer, and only the autoresume
  sweep and the scheduler are unattended. `startWorkflowEngineForTest` calls
  both, because a fixture with an engine and no scheduler is a state no boot
  produces.
- **`SetServiceUpdateRequester` and `ParkUnattendedWork` are bootstrap-boundary
  FUNCTIONS**, and `serviceUpdateRequest` is unexported, for the reason every
  input in `bootstrap.go` is one: an exported method on `App` is a wire RPC by
  construction. A caller that could park this backend's unattended work over the
  wire is a denial of service with a friendly name, and the update trigger gets
  its step-up-gated bound method in the wave that adds it — not a wave early.

## Tests

Application tests stay beside the shell. `main_test.go` changes their working
directory to the repository root because whole-repository AST contracts and
committed fixtures historically use root-relative paths. New tests should still
prefer `t.TempDir()` and explicit paths rather than adding more cwd dependence.

Use package-local tests for private transaction invariants. Put behavior tests
in the narrower owning package whenever the behavior does not require App-level
composition.
