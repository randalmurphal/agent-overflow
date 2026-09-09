# internal/app/

This package is the application shell. It owns `App`, Wails-bound facades and
wire DTOs, service composition and lifecycle, explicit cross-owner
transactions, harness adapters, and application integration tests. Behavior
with one responsibility belongs in the narrower `internal/` package.

See [Application composition](../../docs/architecture/root-decomposition.md)
for the current ownership map and the transactions that remain here.

## Wire surface

Root `service.go` declares a named `main.App` that embeds `*app.App`.
Wails and the custom transport register promoted methods as
`main.App.<Method>`. Do not replace the wrapper with a type alias, register
`*app.App` directly, or change the explicit registration labels. Those
changes alter method IDs.

Every bound method needs an `//ao:scope <name>` annotation and a route.
`methodgen` infers `thread` or `project` when the first non-context
parameter is named `threadID` or `projectID`; all other methods need
`//ao:route home|selected|all`. Add `//ao:stepup` when fresh per-call proof
is required. The vocabulary and enforcement rules live in
[`internal/transport/AGENTS.md`](../transport/AGENTS.md).

After changing a bound method, regenerate Wails bindings with
`wails3 generate bindings -ts`, run `make methodgen`, and verify existing
`$Call.ByID` values remain stable. App-owned generated models live in
`frontend/bindings/agent-overflow/internal/app/models.ts`; never edit them
directly. Keep transport-only adapters on separate unexported types because an
exported method on `*App` becomes a candidate RPC.

## Composition boundary

Executable-only inputs cross through `bootstrap.go`: version, data directory,
test isolation, provider-control environment, notification and updater
adapters, backend identity, and window geometry. Root must not reach into App
fields.

Extracted services receive capability-named dependencies. Do not pass `*App`,
add a catch-all `Host`, or duplicate a service's lock, timer, registry, cache,
or policy here. Keep provider-specific transactions separate.

Cross-owner transactions remain visible here when correctness depends on one
ordered sequence across services. This includes provider-event processing,
send/flush/revert, cross-provider fork, project/worktree deletion and
switching, conversation transfer, and platform lifecycle. Preserve their lock,
rollback, persistence, event, and shutdown order.

## Authorization and caller state

Method scopes are the authorization floor. Argument-dependent checks remain in
`app_authz.go`. Judge resolved behavior, such as the effective runtime mode,
rather than only the literal argument. Use the transport's resolved step-up
answer because checking a single-use proof twice consumes it incorrectly.
In-process and launch-credential calls have no session principal and remain
valid unless the operation explicitly requires one.

A leading `context.Context` gives a bound method access to the connection
principal and is omitted from generated TypeScript signatures. Regenerate both
binding sets after adding it.

Per-screen settings must be obtained through `settingsCaller(ctx)` so bucket
and device class come from one derivation. Backend work for the local screen
uses `settings.Service.BackendScreen()`. A direct `ui_state` write outside
the settings service must invalidate its tier cache.

Events describing one caller's failed attempt carry the connection ID, not a
device ID. An empty ID means an in-process or older caller and remains
applicable. Entity state changes remain broadcasts.

A method that arms a connection-owned resource registers its release with
`transport.ConnState.RegisterCleanup`. If registration reports that teardown
already began, release immediately. Update
`TestArmingMethodsAreTiedToTheirConnection` only when a method truly does not
own a per-connection resource.

## Transaction rules

User-message placement and SendID admission are centralized. Validate input
before consuming drafts or changing runtime state. Acquire the shared
`(threadID, SendID)` lock before thread-action, queue, or mutation locks, and
check accepted messages before provider-visible effects. See
[user-message-ordering.md](../../docs/architecture/user-message-ordering.md).

Draft consumption compares and removes the exact saved snapshot in one store
operation. Uploads stage outside the thread mutation lock, then reacquire it
for ownership-checked publication. Durable flush rows are written before the
in-memory queue, removed only on settlement or an explicit drop, and restored
to the composer at boot instead of being dispatched without a user present.
See [turn-lifecycle.md](../../docs/architecture/turn-lifecycle.md).

Project and thread service writes return the current row plus whether durable
state changed. Emit updates only for changes, while still returning the row to
the initiating caller. Keep mutation-classification tests current when adding
service methods.

Destructive project and workspace operations recheck their store-derived
footprint after unlocked cleanup and before final mutation. Preserve action to
mutation lock order. Workflow cancellation completes before project deletion
acquires thread locks. Worktree setup is cancelled and joined before removing
its path.

## Lifecycle and tests

Shutdown cancels producers and joins package-owned goroutines before closing
their transports or SQLite dependencies. A service that owns mutable process
state owns its stop gate and join.

The complete mocked-provider isolation configuration belongs in
`ConfigureIsolation`. Every fixture capable of starting a session uses the
guarded helpers, temporary provider homes, and fake binaries. Tests must never
read the developer's Claude or Codex homes or start a real provider CLI.

Application tests run with the repository root as their working directory for
whole-tree contracts and committed fixtures. Prefer `t.TempDir()` and explicit
paths in new tests. Test responsibility-owned behavior in its narrower
package; keep tests here for binding, lifecycle, and multi-service composition.
