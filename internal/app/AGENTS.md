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
is no second, hand-listed reachability table. A bound method
addition/removal must annotate it, regenerate Wails bindings with `-ts`,
regenerate methodgen,
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
`*identity.Sessions`, satisfies the four hooks the transport declares
(`SessionForRequest`, `SessionLive`, `SessionScopes`,
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
  unexported adapter type, **not** two exported `App` methods. An exported
  method on `App` is promoted onto `main.App` and becomes a wire RPC by
  construction (see § Bootstrap boundary); redeeming a pairing link over
  the RPC wire would let a caller who already holds a session enroll
  another device, which is the one thing the HTTP route's shape exists to
  constrain. Anything the transport calls through an interface belongs on
  a type of its own for the same reason.
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

## Settings answer per caller

`GetSettings` and `UpdateSettings` are still one method each on one service,
but the DEVICE tier is resolved from the connection: `settingsBucket`
(`app_uistate.go`) derives the caller's `ui_state` bucket exactly as
`uiStateScope` does, and `settings.Service.For(bucket)` is the service seen
from there (`internal/settings/residency.go`). Two screens on one backend read
two font sizes and one shared set of confirmations.

- **A connection with no bucket is not an error here**, which is the one
  difference from `uiStateScope`. `GetUIState` with no bucket has nothing to
  answer with; settings always have an answer — the device defaults — and a
  background saga asking for settings must get them. A session the core
  REFUSES still errors, because that refusal is about the credential.
- **A backend-initiated device write attributes to the caller when there is
  one.** `recentWorkspaces` is written from thread creation, so the create
  paths carry `SettingsBucket` down to `threadapp`; `callerSettingsBucket` is
  the non-failing variant they use, because losing the attribution is not
  worth failing the create. A genuinely caller-less write lands on the backend
  machine's own screen rather than being dropped.
- **A writer that reaches `ui_state` around the settings service owes it
  `InvalidateTierCache`.** There is exactly one — the harness reset's
  `ClearUIState` — and `harnessHost.ClearUIState` makes the call.

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
set together: one annotation, so the surface moves as a unit. Minting
alone adds `//ao:stepup`, because issuing a credential that enrolls
ANOTHER device is the one call a standing grant must not make — a session
that could mint could enroll its way around its own revocation. The rest
answer a device the owner granted `access:admin`, which is what makes
revoking a lost phone from the other phone possible.
Minting ISSUES a credential, revoking withdraws
every credential a device holds, and restoring re-admits a revoked
device's KEY to pairing without moving any credential (the revoked-key
redemption refusal names it as the remedy); the overview read goes with them because
it carries the device map, the connection counts, the audit log, and a
pending pairing's verification number — which is only a check if the
owner is the only party comparing it.

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

## Tests

Application tests stay beside the shell. `main_test.go` changes their working
directory to the repository root because whole-repository AST contracts and
committed fixtures historically use root-relative paths. New tests should still
prefer `t.TempDir()` and explicit paths rather than adding more cwd dependence.

Use package-local tests for private transaction invariants. Put behavior tests
in the narrower owning package whenever the behavior does not require App-level
composition.
