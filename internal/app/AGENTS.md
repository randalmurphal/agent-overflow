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
the generator fails the run without one, and `transport.LocalOnlyMethods` is
derived from those scopes rather than hand-listed. A bound method
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
`*identity.Sessions`, satisfies the three hooks the transport declares
(`SessionForRequest`, `SessionLive`, `PageSessionCredential`), and
implements `transport.AuthEndpoints`.

Everything in that file is adaptation. **No policy decision belongs
there** — a decision made in the adapter is one the session core could not
enforce for a caller that reached it another way; put it in
`internal/identity` and call it from here.

- `initIdentity` runs from `Start` after the store opens, and is
  **deliberately not fatal**. The launch credential still authorizes every
  request, so an App whose identity core failed serves the local page
  exactly as it did before this existed; what it loses is attribution and
  revocation. Refusing to boot would turn a credential-table problem into
  "the app does not start".
- Every accessor answers honestly for an App that never called `Start`.
  Test fixtures build one directly, and nil `identityState` means
  "identity is not wired" — a state, not a fault. The one asymmetry is
  deliberate: a request presenting NO session credential proceeds naming
  none, while one presenting a credential nothing can judge is REFUSED,
  because proceeding would name a session this process cannot revoke.
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

Every method is `CategoryDeviceAccess` in
`internal/transport/internalmethods.go`, and
`TestDeviceAccessSurfaceIsWholeAndLocalOnly` is the tripwire that keeps
the set together. Minting ISSUES a credential, revoking withdraws
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
