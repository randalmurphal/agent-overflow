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

Usage HTTP fixtures use `newUsageTestServer`, which exposes only the actual
usage route. Local port discovery can probe test listeners too; unrelated
root requests must not enter mock handlers, alter counters, or fail credential
assertions.

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
  Settings → Remote access reachable from nowhere but the machine. But two fields of
  its answer are host-only whatever the grant: this launch's token, and the
  ticket-bearing share URLs. Widening the CALL and narrowing the ANSWER is one
  change, not a compromise between two.
- **One helper pair, and every method returning a `network.Settings` goes
  through it.** Including the two that are still `host`-scoped, where the gate
  already refused the off-host caller. One rule wherever the shape leaves the
  process beats a per-method judgement about whether some other check happened
  to cover it — and `SetNetworkSettings` is why: it is step-up reachable from a
  paired device, and its RETURN carried the launch token there until this
  existed. `ServeEndpoints` deliberately keeps the full builder; pairing uses
  `network.PairingURL` to mint only the invitation being returned.
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
touched `lowPowerMode` reads the normal-power default; a saved override wins.

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
- **`notifyOS` is the one gate**, and that placement is the point: a
  send that reaches a presenter without passing through it does not exist, so no
  sender can ship having forgotten to ask. Checking at each call site is a class
  of bug, not a bug.
- **TWO QUESTIONS, ONE GATE.** The per-kind toggles answer "is this moment worth
  an interruption" (`notificationKindEnabledIn`, TOTAL over `notify.Kind` with no
  permissive default — a kind this build has no preference for is refused, not
  raised). The ATTENDED-SCREEN rule answers "is this screen already looking"
  (`screenIsAlreadyLooking`), from the one `notifyQuietWhen` picker on the
  backend machine's own screen and the transport's per-connection presence over
  its LOOPBACK connections. The picker has four readings (`settings.NotifyQuiet*`)
  rather than two toggles because the one most people want, quiet about a thread
  they have open while they are in the app and nothing else, is the AND of the
  two facts, and independent toggles can only say OR. Both halves live here for
  one reason: a sender that could reach a presenter without passing them is a
  sender that can forget one.
- **Reading focus here changes NOTHING about delivery.** The only outcome the
  attended-screen half can produce is a toast that is not raised: no client is
  sent fewer frames, no surface renders differently, and no work is skipped
  because something is off-view. Off-view work shedding is a rejected design in
  this codebase, and this is not it — the alternative to a toast is no toast, not
  a stale pane. The thread rule applies only to a `Target` that NAMES a thread; a
  workflow item and the update notice have no pane to be showing.
- **The refusals are typed apart.** `NotificationSuppressed` is "you turned it
  off"; `NotificationScreenAttended` is "you were watching". Neither is a fault,
  so `logNotificationFailure` skips both — but a log line that could not tell
  them apart would be useless for the one question anybody asks here.
- **The phones are NOT subject to the attended-screen half.** A phone in a
  pocket is a different screen from the one the presence describes.
  `pushFanout` runs after `notifyOS` and applies the per-kind gate per phone
  and nothing else.
- **There is exactly ONE bypass, and it is named.** `notifyOSUngated` skips both
  halves for the harness RPC alone (`app_harness.go`), because every gate reads a
  preference or a screen an e2e run cannot see or control — a Playwright page HAS
  focus, so the default `notifyQuietWhen: "focused"` would silence every harness
  notification the moment a spec opened the app.
  `TestOnlyTheHarnessBypassesTheNotificationGate` keeps the caller list at one.
- **A retraction is never gated**, by either half. The gate answers "may I
  interrupt you", and withdrawing something already on screen is the opposite.
  Gating it would let a toggle flipped mid-flight — or somebody walking back to
  their desk — strand the very alerts it was meant to stop.
- **A notification body carries no content.** Titles are the thread's own,
  clipped; bodies are fixed phrases from `internal/notify`. The provider's
  stderr tail, the failed turn's error message and the approval's command line
  are all deliberately left behind in the tap.

### The same mapping wakes the phones

`app_push.go` hangs off `queueNotification`'s job, after `notifyOS` and not
instead of it, so a phone's tray and the desktop's toast are two views of ONE
`notify.Send` — including its withdrawal, which reaches both or neither.
`internal/push` composes what travels; this file decides who gets it.

- **Its own queue.** A send to Google can hang for its whole ten-second
  timeout, and the desktop's next toast must not wait behind it. Serial for
  the same reason the notification queue is: order is the retraction contract.
- **One kind gate, two screens.** `notificationKindEnabledIn` takes a
  `settings.Settings` and answers whether a kind may interrupt THAT screen.
  The desktop asks it of the backend machine's own settings; the fan-out asks
  it of each registered phone's device-tier bucket. Two copies of that switch
  would eventually disagree, and the disagreement is a phone buzzing for
  something its owner turned off.
- **A revoked device is never sent to**, and that is one SQL join rather than a
  filter here: `store.LivePushTokens` answers "registered AND still admitted"
  as one question, so no caller can ask half of it.
- **The message names this backend by the SAME identity every socket frame
  carries** (`notificationBackendID`, the store's `BackendID`). The phone
  composes its tray tag from that and the send id on both paths, so a
  fan-out that stamped anything else — the display name, a per-boot id —
  would make one moment two notifications. `internal/push`'s guide has the
  tag rule.
- **The credential writes are `access:admin` + step-up, not `host`.** Host
  presence is granted to no session, so a host annotation would leave the
  paste reachable from nowhere but the machine's own window — and the
  machine that most needs the key is the serve host nobody sits at. The
  step-up is the per-call proof the other credential-shaped admin writes
  carry.
- **No credential is the resting state.** A nil `push.Sender` is the whole of
  what a friend's backend does differently: it records registrations and sends
  nothing. The designed next step (the home backend as a wake relay) is a
  different `Sender`, not a different fan-out.
- **`ErrTokenGone` is the only error with a reaction** — drop that row, say
  nothing, let the phone re-register. Everything else becomes the standing
  `lastError` the owner reads and one log line per kind.
- **The harness swaps the LAST hop and nothing above it.**
  `InstallHarnessPushSender` (`app_push_harness.go`) puts a recorder in the
  `push.Sender` seam at harness boot, so `make e2e` proves the real
  composition and `HarnessPushSent` reads back the real payload. It is a
  PACKAGE-LEVEL function rather than a method on `*App`, and structurally
  so: every exported method on `*App` becomes a wire RPC, and a way to
  replace the push sender is not something any session should reach. It
  also refuses to displace a configured credential, so it is safe to call
  unconditionally.

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
- **A recorded failure ends when its CAUSE does.** The two arms that
  configure no certificate source (no domain, and a domain with neither
  hook nor pair) call `clearDomainCertFailure` beside their clearing
  publish. It lives at the arms and not inside `publishDomainCertificate`
  because what makes the failure stale is the CONFIGURATION: every failure
  path records and returns without publishing, so nothing on the publish
  path could ever have cleared one, and a user who reacted to the error by
  clearing the field kept reading it under a screen no longer trying to
  serve anything. Any state whose only writer is a failure path needs an
  owner for the moment the thing it was about stops existing.
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
  Cleared only when NOTHING is left answering: HTTPS failing while
  cleartext keeps accepting is a degraded tailnet, not an unreachable one.
- **An attach is identified by its own `tailnetSlot`, created before the
  serve.** The transport reports an auxiliary listener's terminal error
  from its own accept goroutine, which can run before `ServeAuxiliary`
  returns the handle. Matching that report against the FIELD instead
  found it empty, dropped the report, and left a node that is up with
  nothing listening on it and no kick to re-attach. The slot exists first,
  the report marks it, and `adoptAuxListener` refuses to record a listener
  whose accept loop already ended. The general shape: when a callback can
  fire before the call that installs it returns, the identity the callback
  names has to be created BEFORE the call, not assigned from its result.
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

## The dev-server list

Generated HTML uses `app_file_preview.go` and `internal/filepreview`, sharing
transport's preview authorization at separate origins. `MintFilePreviewURL` pins
to the selected computer and derives locality from the authenticated host proof,
including paired local browsers. Never take locality from RPC arguments.
Changing LAN/tailnet sharing calls `resetNetworkPreviews`: closing or rebinding
the app connection does not close independently opened preview listeners. Local
file previews survive those policy changes; shutdown closes every manager.

`app_preview.go` is the App's half of the port gateway
(`docs/specs/remote-access.md` §7). `internal/devscan` knows how to look at
the machine and nothing else; this file owns the two things around it —
WHO the owners are, and WHEN a scan is worth doing.

- **The loop is gated on somebody off-machine who may actually receive
  the list**, `EventBus.RemoteReceiverCount("devserver:list") > 0`. A scan
  walks `/proc` and dials loopback ports every three seconds; on a
  desktop-only install nobody ever reads the answer, so the loop does no
  work at all and `GetDevServers` scans on demand for the one caller
  there will ever be. Channel subscription is not the signal: an SPA
  subscriber takes every channel by default.
- **Owners are the provider session and the running terminals of every
  thread**, through `sessionruntime.Manager.ThreadProcesses` and
  `terminal.Manager.ThreadProcesses`. Each is handed over with PGID equal
  to PID, because both spawn paths call `procutil.ConfigureGroup` — and
  the group is the half that still matches after a dev server daemonises
  out of the ancestor chain.
- **A preview outlives the app connection that opened it.** With no remote
  app readers, discovery sleeps and `ReleaseIdlePorts` releases listeners only
  after outstanding tickets, cookie handoffs, and browser grants have expired.
  An independently opened browser must not lose its preview when the app
  disconnects. Every preview request still revalidates the session. A scan
  error or ending life context calls `releasePreviewListeners` (`SetPorts(nil)`)
  to retire the discovery set immediately; removing a shared port also closes
  its listener. Neither idle cleanup nor forced retirement builds a gateway.
- **A scan error stops discovery and is remembered.** Every error a scan
  can return comes from the enumerator (this platform cannot look, or the
  socket tables cannot be read) and neither changes on a retry; the probe
  never errors. So the loop exits, and the RPC answers with the same
  sentence rather than an empty list, which would read as "nothing is
  listening".
- **The gateway is the App's, one per process, built on first reconcile**
  (`previewGateway`). It needs the transport server, so it is nil in every
  fixture that never called `SetTransportServer` — and a nil gateway means
  the list is still published, with nothing served behind it. `Shutdown`
  closes it, which ends every preview URL the backend had handed out.
- **The list may only call a port shareable when a listener is actually
  serving it.** `reconcilePreviewListeners` runs BEFORE the list is
  published: it hands the allowed rows to `SetPorts` as
  `transport.PreviewTarget`s — port AND the scheme discovery found the port
  speaking, because the gateway has to dial the same thing — then clears
  `allowed` on every row the gateway is not serving and copies the
  gateway's own note onto it. `MintPreviewURL` therefore agrees with the list by
  construction rather than by a second derivation.
- **A port THIS process is listening on is never proxied**
  (`refusePreviewOnOwnPorts`). Several of this backend's own loopback
  listeners answer a GET like a page — the transport bind, the design
  listener, pprof, the harness control plane, and one preview listener
  per port already shared — so the scan offers them as candidates and the
  owner can name one by hand. The scan already reports the PID holding
  each socket, and this process knows its own, so the rule is ONE
  comparison against `os.Getpid()` rather than a listener inventory eight
  packages would have to keep in step. It is applied at the top of
  `reconcilePreviewListeners`, which is what stops the port ever reaching
  `SetPorts`, and again in `AllowPreviewPort` — before the write, because
  a candidate that is not in the set yet has no row on the published list
  and would otherwise be stored as a choice that can only come back
  refused. `MintPreviewURL` reads the published list rather than scanning
  again. The attributed half needs none of this: attribution is by
  ancestry from a thread's own session, and this app is not a descendant
  of one.
- **`previewHost` asks the SOURCES, in order**, through the same
  `previewSources` list the gateway binds through
  (`app_preview_source.go`): the tailnet node's MagicDNS name first, this
  machine's LAN address second. Deriving the address separately is how the
  screen would come to show a host nothing is listening on. Both sources
  are TLS-only, with no cleartext fallback: the preview cookie is
  `Secure`, so a tailnet with HTTPS turned off (no `CertDomains`) has no
  preview address at all, and says so.
- **`previewScanner` refuses to build a real scanner inside a test
  binary**, the same shape as `resolveTextGenerationExecutor`. A real scan
  dials every candidate loopback port, and on a developer's machine those
  are their own work. A fixture that wants list behaviour installs a fake
  through `app.preview.scanner`.
- **`GetDevServers` carries `preview:open`, not a read scope.** The list
  names every loopback port on the host that answers like a page and the
  process holding it, which is a port-scan of the machine;
  `TestChannelScopeMatchesItsReadRPC` pins the channel to the same grant.
  `AllowPreviewPort` / `DisallowPreviewPort` carry `access:admin` and NO
  step-up: they expose the owner's own dev server to the owner's own
  devices and change nothing about what this backend binds.

## The activation gate

`app_activation.go` is the backend half of `internal/supervise`: a boot that is
a supervisor TRIAL must prove it works before anything commits to it, which
means booting FULLY — store, migrations, transport bind, ready — and answering
read-only health probes. Client requests and unattended actions wait for commit.

- **One gate.** `Start` hands the whole unattended set to
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
  scheduler. `WaitForActivation` gates every transport request except health.
  Credential rotation is a side effect too: returning a new refresh secret
  before commit lets a rollback invalidate a client's saved pairing.
  Requests wait on the same gate and their connection context, rather than
  receiving HTTP 503 that older Go clients misclassified as revoked auth.
  `TestTrialKeepsEveryClientRouteOutsideTheRollbackBoundary` pins the boundary.
- **A git READ is not an action.** `backfillProjectIdentity` runs unparked, in
  its own goroutine, because its whole effect is two TEXT columns in SQLite —
  inside the snapshot boundary — and `git remote get-url` / `git rev-list`
  change nothing. The rule is about what a rollback cannot undo, not about
  whether a subprocess was spawned.
- **`startWorkflowAutomation` is split out of `initWorkflowEngine`** for that
  reason: the engine must initialize before reporting prepared, and only the autoresume
  sweep and the scheduler are unattended. `startWorkflowEngineForTest` calls
  both, because a fixture with an engine and no scheduler is a state no boot
  produces.
- **`SetServiceUpdateRequester`, `ParkUnattendedWork` and
  `ConfigureServiceUpdates` are bootstrap-boundary FUNCTIONS**, and
  `serviceUpdateRequest` is unexported, for the reason every input in
  `bootstrap.go` is one: an exported method on `App` is a wire RPC by
  construction. A caller that could park this backend's unattended work over
  the wire is a denial of service with a friendly name, and one that could
  point the updater at a release feed of its choosing is worse.

## Update admission

`workAdmission` fences new work against a staged update's final idle check.
Acquire inside shared send, queue, provider-start, terminal-start, remote-command
and transfer entry points, including internal callers. Workflow `BeginWork`
wraps new-run creation and transitions back to running. Leases cover admission;
existing triage, dispatch, runtime, command and workflow owners retain the work's
lifetime. Transfer attempts hold their lease through file/SQLite publication;
parked transfer journals resume after restart.

The provider-account manager receives the same `BeginWork` port. Live sign-in
sessions retain it through teardown; credential-switch, removal, probe,
reconciliation and usage-refresh transactions retain it through publication.
Acquire before account/reconcile locks. This includes periodic refreshes that
can consume a single-use credential: restarting mid-rotation can invalidate a
login even when no agent turn is running.

Chat worktree setup and session-import managers receive it too. Their accepted
asynchronous runs hold the lease through command/worker cleanup and final
publication. History refresh holds it across its thread lock and commit.
Creating a thread or preparing/attaching its worktree holds admission through
the setup kickoff, including deferred kickoff after the thread lock releases.
The async setup lease must overlap its caller's lease: otherwise an update can
leave a created worktree whose setup was never started or reported as failed.

The idle predicate runs under the admission mutex. It may read those owners and
SQLite, but must never call the workflow actor, a provider, another admission,
or acquire a thread action lock. Read triage before dispatch counts to preserve
queue-handoff overlap. On explicit refusal/cancellation reopen admission; after accepted
handoff keep it closed until shutdown. Waiting new callers use a cancellation
context and have not accepted work. Preparation is bounded to 20 minutes; waiting
for existing work has no artificial deadline and remains explicitly cancelable.
Running PTYs block restart until closed, since an idle shell can still own jobs.
At shutdown, reject callers parked behind the restart fence before transport
drains; they have accepted no work and must not await the later app cancellation.

## The remote update trigger

`app_service_update.go` is how the owner of a supervised `serve` host installs
a different version without walking to it. It sits between two things it does
not own: `internal/appupdate`'s `ReleaseSource` above (resolve, download,
verify) and `internal/supervise` below (preflight, stage, request).

**The order is the safety property, and it is the local command's order**
(`internal/aocli`'s `serviceUpdate`):

```
resolving -> downloading -> verifying -> staging -> waiting (if busy) -> requested
```

- **Verify BEFORE stage, always.** Two refusals live in that step and both have
  to happen while the artifact is still a temp file: a download that is not an
  Agent Overflow binary this host can run, and one that speaks an update
  protocol the installed supervisor does not (`supervise.CheckPreflight`). A
  version directory is immutable and is what `acceptUpdate` selects from, so
  either question asked after the staging would mean writing down a version the
  supervisor then has to refuse.
- **The version staged is the PREFLIGHT's answer, not the tag's.** A tag is
  what the release feed calls a build; `__service-preflight` is what the binary
  calls itself, and the directory has to be named for the second or a rollback
  returns to a directory holding something else.
  Which is why "is this the version already running" is asked TWICE, against
  two different facts: once on the tag before anything is fetched, and once on
  the binary's own answer before anything is staged
  (`ErrServiceUpdateAlreadyRunning` both times). Only the second can catch a
  release tagged something else that reports the running version, and without
  it that download is staged straight over `versions/<running>` — a directory
  whose name then asserts a build it does not hold, and the one a rollback
  returns to. When a value is re-derived from a more authoritative source
  mid-flow, every check made on the earlier one is due again.
- **The download lands in `os.CreateTemp` under `layout.Root()`, 0700, removed
  on every exit path** including success. Under the root because
  `StageBinary` must be a local copy on one filesystem rather than a
  cross-device move that could tear.
- **Preparation failures and explicit supervisor refusals set `Phase: error`
  naming the step.** A timeout/broken supervisor pipe is ambiguous: keep work
  admission closed, retain `requested`, and request ordered backend shutdown.
  The supervisor's durable state decides which version boots next. Never retry
  an uncorrelated update request or reuse its reply slot after a lost answer.
  After `requested` the status STAYS there: this
  process is about to be stopped by its own supervisor, and a client polling in
  those seconds needs to see which update to wait for.
- **One flow at a time** (`ErrServiceUpdateBusy`). Two downloads racing for one
  staging layout is a corrupted version directory with a friendly name.
- **Cancellation ends preparation, never an accepted restart.**
  `CancelServiceUpdate` is selected-computer `access:admin`. The flow owns its
  context independently of the requesting socket. Cancellation and the final
  supervisor handoff share the state mutex; a preflight or stage that finishes
  late must check that context under the mutex before claiming the handoff.
  Status advertises `cancelable` only before that boundary. A canceled flow
  publishes `canceled`, while a deadline remains an error. An accepted restart
  continues refusing a second update even after its preparation goroutine ends.
- **A trial boot refuses before it downloads** (`ErrServiceUpdateTrial`). A
  trial IS a supervisor mid-update, and the supervisor accepts one at a time,
  so the only thing a download could earn is a refusal after the stage. The
  parked activation gate is how this process knows it is a trial.

**The passive check is deliberately NOT in the parked set.** One goroutine,
one `Source.Latest`, never retried. It is a network read, which sounds like the
parked set's shape, but the membership rule is "would restoring the database
undo it?" and a read of a public release list undoes itself by ending. Parking
it would only mean a trial's update surface reported no known release for as
long as the trial ran. `TestThePassiveCheckRunsDuringATrial` pins that.

**The RPCs and their scopes.** `GetServiceUpdateStatus` (never touches
the network, answers `Supervised:false` with no error off a supervised host)
and `ListServiceReleases` are `access:admin`, as is `CancelServiceUpdate`.
`RequestServiceUpdate` is
`access:admin` + `//ao:stepup`: choosing which build a machine runs is what a
paired admin device is for, and `host` would have left the whole feature
reachable only from the machine it exists to save a trip to — which is what
`agent-overflow service update` already is. The step-up is what §7 admits the
remote trigger on, and `TestStepUpMethodsAreTheSpecSet` pins the annotation.

**Everything here is absent on every boot but one.** `ConfigureServiceUpdates`
runs only from a supervised `serve` whose build has a release artifact a
supervisor can stage (`serviceArtifactPlatform()` in package main:
`headless-linux` for the windowless Linux build, `linux` or `darwin` for the
ordinary build, and `""` on Windows, which is not a supervised serve mode).
`supervise.PrepareArtifact` expands verified macOS releases before preflight,
retains the entire bundle, and publishes the old flat supervisor entry point. An empty answer becomes the `Unavailable` sentence rather than a
button that cannot work.

The frontend half is `frontend/src/lib/stores/serviceUpdate.svelte.ts` (one box
per attached backend, fed by both channels and re-read on every hello) and
`components/settings/MachineUpdates.svelte`; its guide entry is
`frontend/src/lib/stores/AGENTS.md`.

## A send is answered once, however many times it arrives

A socket that died AFTER the frame reached the backend looks exactly like one
that died before it: the RPC never answers. The client's transport re-sends
the frame for two methods and only two (`RETRY_ON_TRANSIENT_CLOSE` in
`frontend/src/lib/transport/wsClient.ts`), so both have to be idempotent HERE
— a retry that started a second turn would be worse than the lost answer it
was recovering.

`app_send_idempotency.go` is the whole mechanism, and it is deliberately not
a table.

- **The id is the client's, minted once per send** (`buildSendOptions`), and
  it rides `SendMessageOptions.SendID` on both paths. An EMPTY id is legal
  and disables the check: every app-internal injector sends one, as does any
  bundle older than the field, and treating them as one message would collapse
  a workflow's injected sends into the first.
- **The record is the message itself**, in whichever of its two homes it
  reached. A dispatched send is a `user_text` row whose `meta` carries
  `sendId` (`internal/usermessage`); a queued one is a `flush_queue_items`
  row whose `send_id` column carries it. `findRecordedSend` looks in both and
  answers the caller from what it finds — the persisted item, or the queue
  row projected back through `flushqueue.ItemFromStore`.
- **Retained history, matched through sparse indexes.**
  `store.FindUserTextItemBySendID(threadID, sendID)` probes send identities on
  both physical timeline arms and hydrates at most one row. A disconnected
  frontend can retry after another frontend added many messages; a newest-N
  cutoff must never turn that retry into a new send. Migration v89 indexes only
  top-level user rows carrying an identity, preserving cheap misses without an
  extra receipt table. Imported overrides and the reader-authored predicate
  remain part of the lookup. Query-plan tests require both indexes.
- **The check runs under the lock that serializes the path, before any side
  effect.** In `sendMessageLocked` that is first thing inside the thread
  action lock — before the runtime-mode write, before the session start,
  above all before the provider write. In `registerQueueItem` it is
  immediately after `handoffMu`, which is the queue path's serialization
  point, and BEFORE the length cap so a duplicate cannot be answered "queue
  full".

## The flush queue outlives the process

The composer clears the moment `RegisterQueueItem` returns, so between the
register and the provider write the queue is the message's only copy — and it
was process memory, which a crash threw away with no trace anywhere. It now
has a row (`flush_queue_items`, migration v85). `internal/triage` keeps the
live queue and knows nothing about the row; this package owns its whole life.

- **Durable first, then memory.** `registerQueueItem` inserts before
  `triage.RegisterQueueItem`, so an insert failure is a visible refusal to
  queue rather than a message that quietly is not there tomorrow.
- **The row dies at a durable endpoint, and `triage.FlushSettlement` is
  already exactly that.** `flushQueueSettlement` composes the delete with
  whatever an injector passed as `onDurable`, so the two moments the message
  is safely somewhere else — the dispatcher's persisted `user_text` row, and a
  session-death restore into the composer draft — delete it exactly once
  (`sync.Once`) with no call site having to remember the table.
- **A requeue KEEPS its row.** A failed dispatch, a pre-init teardown that
  could not write the draft: the message is still undelivered, so the row is
  still its only durable copy. `requeueEagerPersistedFlushes` deliberately
  does NOT re-create a settled row either — an already-dispatched message has
  a persisted `user_text` row as its durable copy, and a second home would let
  the boot sweep restore text that is also in the timeline.
- **A DROP takes the rows with it.** `teardownAndCloseSession` (Stop, idle
  close, archived-thread close) and `clearFlushDispatchForRollback` (the Codex
  rollback purge) discard the in-memory queue with no restore, so both call
  `dropDurableFlushQueue` — otherwise the next boot resurrects exactly the
  messages the user's Stop or revert threw away. Thread deletion needs
  nothing: the FK cascades.
- **The boot sweep restores into the COMPOSER and never re-dispatches.**
  `restoreDurableFlushQueueAtBoot` runs in `initSubsystems` beside the other
  boot repairs, after the store opens and before any session can start, so
  every remaining row is provably residue. A queued message was written
  against a turn that no longer exists, on a session that is gone, in a
  conversation nobody has looked at since; sending it hours later on somebody's
  behalf is the one outcome nobody asked for. It merges through
  `internal/composerdraft` with the same rule and the same `draft:updated`
  event a session death uses — queued text ahead of whatever the composer
  itself holds — and deletes the rows only once that write succeeded, so a
  failure means the next boot tries again.

## A broadcast about ONE client's attempt names that client

An event channel that is not entity-filtered reaches every client, which is
right for a fact about the THREAD and wrong for a fact about one caller's
call. Three frames are the second kind, and each carries the connection that
produced it so a receiver can tell whose it is:

- `provider:approval` and `provider:user_input` with `action:"fail"`
  (`app_approval.go`, `emitApprovalFailure` / `emitUserInputFailure`). The
  prompt is still open on every other screen; a sticky "Failed to respond to
  approval" banner there is both wrong and unclearable by whoever reads it.
  The `resolve` half is deliberately unstamped — that one IS everybody's.
- `user_message:reverted` with `DraftPendingResend`
  (`app_revert_and_resend.go`). The cut is everybody's and every client
  applies it; the SAGA is not, and a second client recording a marker for it
  makes a later guard rejection read as a committed revert.

Three rules hold for all of them, and for the fourth one somebody adds:

- **The CONNECTION, never the device.** Two tabs of one browser answer and
  edit independently, so a device stamp puts the losing tab's error on the
  other one. It is the same key `app_draft_broadcast.go` suppresses a
  draft echo on, and for the same reason.
- **An empty stamp is a real value**, produced by every in-process caller
  (a saga, a workflow phase, a test) and read by the client as "apply it".
  That keeps a bundle running against an older backend behaving exactly as
  it did rather than silently swallowing a failure whose only surfacing
  this is.
- **Reaching the caller needs a leading `ctx context.Context`** and
  `clientOf(ctx)`. The generator strips it from the TS bindings, so no wire
  signature and no method ID moves — regenerate both halves anyway, because
  the doc comment travels.

## An armed resource belongs to its connection

A bound method that arms something on behalf of ONE client owes that
client's connection a way to give it back. The client's own un-arm call is
the happy path; the connection dying without one is the path that leaks.
`transport.ConnState` is the seam: take a leading `ctx context.Context`,
read `transport.ConnStateFromContext(ctx)`, and register the release with
`state.RegisterCleanup` — and release inline when it returns false, because
a connection already tearing down will run no more callbacks. A `nil`
ConnState is not an error: it is every in-process caller (a saga, a test),
which gets no safety net and owes the explicit release it always did.

- **The take-control lease is the worked example.** A claude-tui PTY
  attachment belongs to the connection that made it
  (`app_claudetui_terminal.go`), so a socket that dies mid-take-control
  gives the input lease back and a second client attaching takes nothing
  from the first. It was one session-wide boolean and one session-wide
  sink: a dead client left the lease held and refused every `Send` on that
  thread until the session restarted, and either client's detach stripped
  the other's. The cleanup is registered once per CONNECTION (`arm`), not
  once per attach, and releases every claim that socket still holds: a
  pane remounting a hundred times over one socket leaves one closure, not
  a hundred.
- **`TestArmingMethodsAreTiedToTheirConnection` is the gate.** It
  AST-scans this package for exported `*App` methods whose name carries an
  arm/un-arm word at a CamelCase boundary (`Attach`, `Detach`,
  `Subscribe`, `Unsubscribe`, `SetControl`, `TakeControl`,
  `ReleaseControl`, `Hold`, `Release`, `Acquire`) and fails unless the body
  — or a helper it calls in the same file — reads
  `ConnStateFromContext`. The alternative is one line in
  `connStateExemptMethods` saying why the resource is not per connection;
  four entries qualify today, all of them releases BY ID whose arming half
  owns the tie. An exemption naming a method that no longer matches fails
  too, so the prose cannot outlive what it excused.
- **Satisfying the gate changes no wire signature.** The generator strips
  the leading `ctx` from the TS bindings and hashes the method NAME, so
  the id is stable — but regenerate both halves anyway (`make methodgen`
  and `wails3 generate bindings -ts`), because the doc comment travels.

## Tests

Application tests stay beside the shell. `main_test.go` changes their working
directory to the repository root because whole-repository AST contracts and
committed fixtures historically use root-relative paths. New tests should still
prefer `t.TempDir()` and explicit paths rather than adding more cwd dependence.

Use package-local tests for private transaction invariants. Put behavior tests
in the narrower owning package whenever the behavior does not require App-level
composition.

`newTestApp` (the rollback fixture) builds NO triage router. A test that
exercises a triage-guarded app path must call `app.ensureTriageRouter()`
first, or the guard answers "no router" and the path silently short-circuits.

## Tailnet listener reconciliation

Pairing uses `network.PairingURL` so the QR endpoint and certificate trust
match the listener being reached. A main-port change retires only the plain
tailnet listener; the phone's HTTPS listener stays on 443. Withdrawing
certificate domains retires HTTPS. See
`TestTailnetRetiresOnlyListenersWhoseConfigurationChanged`.

## Remote project registration

BrowseDirectory is a selected-computer `files:read` RPC, not a native desktop
picker. Keep its bounded dirbrowse implementation available to paired clients;
CreateProject independently requires `git:operate`. Filesystem paths never
identify which computer a caller intends, so frontend operations pin their
computer before dispatch, including follow-up reads after awaits.


The headless pairing console uses `app_local_control.go`: successful startup
publishes the private local endpoint, a listener-port change republishes it,
and shutdown withdraws only its own launch. Never print or export `control.json`;
its credential stays on this computer. CLI pairing exercises the existing
Devices methods over transport rather than bypassing authorization.

## Conversation transfers

`app_thread_transfer*.go` composes provider-owned snapshot/copy formats, Git
workspace preparation, attachment installation, and the store ownership journal.
`BeginThreadTransfer` returns only a public activation challenge. The frontend
accepts an offer on its explicitly chosen destination, then binds that single
operation grant on the source. Neither backend needs the phone's device key or
session credential. The destination resolves its own project/workspace paths
and authorizes the selected execution mode; those choices are immutable.

`startThreadTransfers` runs behind the unattended-work activation gate. Jobs use
the app lifetime, recover journal work at startup, and are joined after app
cancellation before store/provider teardown. `ThreadTransferEndpoints` is the
bootstrap HTTP adapter, not a wire RPC. Status serialization must never include
private grants, activation secrets, or installation recipes.

Snapshot creation closes the source provider, flushes final history, checks
queues/wakeups/background tasks, and takes native identity locks. A read-only
Codex metadata probe must not resume the native thread. Source file/Git state is
checked again after archiving; the completion marker makes retries reuse one
snapshot. Destination validation imports history into an isolated scratch SQLite
file, checks its complete attachment/native closure, and records exact file
baselines plus a registered worktree plan before advertising prepared. Fresh
provider installation/account checks and the native format's minimum CLI version
must pass first. The source's installed version is not a required target version. Activation
publishes files and commits history/ownership atomically at the store boundary.

Source reservation takes the thread action lock, then `LockMutable` before
checking idle state and recording its fence. Ordinary edits (drafts, model
settings, comments, uploads, queue admission, terminal creation) use
`threadapp.LockMutable`; actions already holding the action lock use
`CheckMutable`. Never take an action lock while holding the mutation lock:
composer saves and queue admission must work during a send or edit-resend.
Keep the guard through the write and its synchronous side effects. A guard
only at RPC dispatch races reservation. Group metadata checks the same fence
inside its SQL transaction. Delete checks precede provider/file cleanup.
`CheckCleanup` permits host-local project/retention cleanup after a confirmed
outgoing move, while ordinary thread actions still return the new owner. Retired
worktree caches are never reattached or resumed. Project deletion's returned
thread IDs omit those caches, so a frontend cannot close the new owner's pane;
SQL/native retirement survives every cleanup.

Destination offers reserve their project in the journal's typed `ProjectID`.
Project deletion checks that reservation atomically; private app JSON must not
become a SQL query interface. History restores refuse unfinished transfers and
snapshots predating currently owned incoming conversations. UI status reads
exclude private grants, project reservation data, and installation details.

App integration tests use isolated native files and two real TLS listeners.
Never restart the developer's running backend to test a transfer: it may host
this conversation. Current implementation progress and remaining delivery work
are tracked in `docs/specs/conversation-transfer.md`.

## Commands on peer computers

`app_remote_jobs.go` routes the session-scoped AO CLI to explicitly enabled
paired peers. Pairing a frontend with two computers does not grant those
computers credentials for each other. The source uses its own attached carrier;
never copy the phone's key or rotating session into another backend. Agent
access defaults off and is checked before each start. Status/cancel remain
available after opt-out for already accepted work.

Enabling first verifies the authenticated peer's identity, protocol, command
capability and terminal scope. Disabling never needs a live peer. A saved
profile does not prove confirmation completed; the UI must retain its reconnect
action after a lost acknowledgment or reload. The full pairing grants the
originating computer ordinary device access; the agent toggle separately gates
command starts. Operator details: `docs/architecture/remote-commands.md`.

Destination commands use terminal:operate, resolve a registered workspace and
attribute the receipt to the authenticated session's device (never the screen
ID query argument). Source provenance comes from CallerScope. The source checks
that status/cancel belongs to that conversation. Workflow phases additionally
need remote-commands. Context cancellation of a source RPC does not cancel the
accepted destination process. Stop the job manager before closing SQLite.
