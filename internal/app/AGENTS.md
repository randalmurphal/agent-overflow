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

That context ends at server shutdown, not when the caller disconnects, unless
the method is marked `//ao:callerbound`. Mark only a method whose context
reaches nothing outside the process: no subprocess, no network request, no
shared or deduplicated operation and no write. A subscription or stream
qualifies when the connection's cleanup stack releases it. List it in
`callerBoundMethods` with what its context reaches; when in doubt, do not.
Work past a method's first effect that must also survive shutdown runs on
`context.WithoutCancel(ctx)`, or `ctxutil.Detach` onto its owner's lifetime,
under its own timeout.

Per-screen settings must be obtained through `settingsCaller(ctx)` so bucket
and device class come from one derivation. Backend work for the local screen
uses `settings.Service.BackendScreen()`. A direct `ui_state` write outside
the settings service must invalidate its tier cache.

Events describing one caller's failed attempt carry the connection ID, not a
device ID. An empty ID means an in-process or older caller and remains
applicable. Entity state changes remain broadcasts.

Thread session banners are `provider:status` events owned by their trackers:
`binary_stale` by `app_provider_binary_watch.go`, `prompt_stale` by
`app_prompt_stale.go`. Each flag is pinned to the session token that earned
it, mirrored into `GetThreadLiveState`, dismissed through
`DismissSessionBanner`, and withdrawn with a `withdrawKind` naming it. The
contract is in
[prompt-tool-overrides.md](../../docs/specs/prompt-tool-overrides.md#prompt-changed-banner).
Error and notice rows the store refused are pushed on `thread:unsaved_rows`
with their severity, at most five per thread, and read back by
`GetThreadUnsavedRows` at the push's threads:read scope. An entry leaves
when its row later saves, when `DismissUnsavedThreadRows` names it, or with
the thread (`app_unsaved_rows.go`).

A method that arms a connection-owned resource registers its release with
`transport.ConnState.RegisterCleanup`. If registration reports that teardown
already began, release immediately. A resource the client can also release
by id registers with `BindCleanup` instead, and its release method unbinds
the same key on its call's connection. Update
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
A teardown's or a revert's drop holds the thread mutation lock that every queue
write takes, so a message queued during it is in the queue the drop reads or
queued whole after it. A caller releases that lock before it tears down; the
idle reaper, which holds it, uses the HoldingMutations variant. See
[turn-lifecycle.md](../../docs/architecture/turn-lifecycle.md).

Agent thread requests settle once. Every state change on a request or receipt
is conditional on the state it expects to find, and a change that does not
apply is a lost race rather than an error. A settlement takes the token's
settle lock before it writes, releases it before queue or provider work, and
is handed to the caller by the same door
(`app_thread_tools_settle.go`). A receipt is bound to the turn its message
was delivered into, and to whether that turn opened for it (the only turn a
cancel interrupts), only from triage's send-delivered hook or a send that
opened its own turn (`bindRequestDelivery`); a message handed to the
provider waits for that, because Claude can hold it for a turn of its own.
A message a turn took and never echoed is bound to that turn when its end
is read (`bindUndeliveredReceiptToTakingTurn`). Triage reports each
delivery and echo once, so a binding or an owner's confirmation whose write
fails is kept and written again, every reader of a receipt's turn reads a
kept binding as that turn (`deliveredReceiptLocked`), and an echoed message
is never given back meanwhile (`app_thread_tools_bookkeeping.go`). A cancel
arms its wait for a delivery only inside the settlement it wins. A wake
(`app_thread_tools_wake.go`, `app_thread_tools_wake_message.go`) writes its
durable queue row and the delivery mark of every request it answers in one
store transaction. Every message the app writes into
a thread for someone else goes through `queueAgentNotice`
(`app_agent_notice.go`): it is accepted once queued, without the action
lock, and the session start it needs runs afterwards on joined notice
work. Nobody watches that start, so its failure notifies, once per run
of failures, as well as writing the error row, and a later loop or watch
fire tries it again. Settling runs off the provider event
worker because it can stop or start a session, and a stop drains that
worker's queue.

Loops and watches run on the same sweep: registration and lifecycle in
`app_agent_wakes.go`, runs and boot recovery in `app_agent_wake_runs.go`,
rendering and queueing in `app_agent_wake_delivery.go`, assistant records,
creation and handoff in `app_standing_agents.go`, and the background tray's reads, Wake now and
Cancel in `app_agent_wake_rows.go`. Every write to a loop, watch or
reminder row emits `thread:wakes_changed` with that row as it stands
(`emitAgentWakeChanged`, `emitThreadReminderChanged`); Wake now goes through
`fireAgentWake` or `fireThreadReminder`, never a delivery of its own. The
Agents segment's reads, stops and change frames are
`app_thread_agent_rows*.go` and `app_thread_agent_incoming.go`; a Stop of a
spawned thread, the person's or its spawner's `thread_cancel`, brings it to
rest through `restThreadHere` (`app_thread_rest.go`) on whichever computer
holds it, sharing archive's capture and stop of the thread's work. The
contract is
[standing agents](../../docs/specs/standing-agents.md#wake-supervisor). A
loop or watch wake is data, not something the person wrote: one still queued
at boot or left unconfirmed by a session that ended never goes to the
composer, and its row records `not delivered`; an outside `thread send` is
handled the same way, with a notice in its thread. A request's answer or
reminder, a remote job's completion and a spawner's answer to its
worker's async questions that never reached the agent go back to their
owners through one path (`giveBackToOwners`), from an End
Session, an archive, a revert, a session end, a failed Codex resend and the
boot sweep,
whether still queued or handed to the provider and not confirmed. A
message taken out of the queue before the agent read it (an Incoming
withdrawal, a rest, an archive, a handoff, a loop's cancel) goes back to
its owner through one dispatch keyed by its send id root
(`withdrawQueuedMessage`); a new root in `appSendIDRoots` needs an owner
there. The
provider's echo confirms the owner's delivery mark (`confirmOwnedDeliveries`,
on triage's `SetFlushSendConsumedHook`), and a completion a session end returned is
paced on the wake retry ladder (`completionRedelivery`). A message
Claude joined from a person's text and such parts is split by the lengths
its dispatch recorded (`usermessage.JoinedPart`), and only the person's
text goes back (`agentPartsNotTakenIn`); another thread's request in it
goes back to its queue, owed again (`giveBackRequest`), from a session
end, and a teardown does the same before it drops the queue
(`returnRequestsAtTeardown`). What the app's messages are owed
is applied once that restore commits (`settleNotTakenIn`): a restore that
fails requeues them whole. Whether a turn was a tick
is read from what triage's delivery resolver says the turn took in
(`SendDelivery.Members`, `noteAgentTurnIntake`), because neither a dispatch
nor the rows' turn says which turn the provider ran a message in.
Triage asks whether a turn that ended cleanly was a tick through
`SetQuietTurnClassifier` (`agentTurnIsTick`), and keeps such a turn out of
the thread's activity and read state; the marks it reads are kept by
`tapAgentWakeTurns`, which runs on the emit funnel before the notification
tap.

A spawned thread's prompts are stamped with their route to its spawner
by triage (`SetPromptRouter`) and delivered, answered and settled in
`app_worker_prompts.go`, off the emit path on one serial queue so a
resolution never overtakes its delivery. Triage's `RoutedTo` is the one
record of a prompt's route, and the registry follows its announcements. A
prompt whose wake cannot reach the spawner (a failed delivery, a session
that ended with it unconfirmed, a cleared queue) is rejected
(`triage.RejectPrompt`): it moves to where the worker's prompts route now,
or to the user when that is the same spawner. A handoff, an archive or a
delete reconciles the prompts routed to the thread it changed
(`reroutePromptsFrom`, `triage.ReroutePrompts`): a route that still holds
is kept, and the others move on or go to the user; none is left routed to
nobody. A token names one prompt occurrence (`triage.PromptRef`): it
answers, settles and releases only that prompt, written only to the session
that asked it, never a later prompt under the same request id. A Codex
thread's async questions are prompts on the same registry, but triage's
live state ends with the session and they do not: their route is kept
in `app_worker_prompts_async.go`, reconciled from the store on every
change to them, on every thread frame but a patch, on a move away, and at
boot. A
reconcile of a thread asking nothing is one indexed read; a spawn that
names a fork's thread adopts the questions it carries
(`adoptAsyncQuestionPrompts`). Shutdown joins the reconcile queue before
the store closes.

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

Every boot binds the transport before `Start` and marks it ready after, so no
bound method runs before `Start` returns (see
[Startup readiness](../../docs/architecture/transport.md#startup-readiness)).
`Start` reports each phase through `bootPhase`, whose ids are the
`boot: phase=` log names, and honors its context between phases and inside
migrations. A phase that fails without stopping the boot reports through
`bootPhaseFailed`, which every client shows; a log line alone is not a
report. The desktop `ServiceStartup` runs `Start` on its own goroutine:
the window opens during it, `SetStartDone` receives the result, and
`ServiceShutdown` cancels and joins it. Code the window or shell reaches
during that time must not assume `Start` finished; check the state it needs,
as `persistWindowGeometry` does.
Unattended work that scans the database waits for `awaitFirstReadsSettled`
so it does not compete with the first client's catalog reads.

The complete mocked-provider isolation configuration belongs in
`ConfigureIsolation`. Every fixture capable of starting a session uses the
guarded helpers, temporary provider homes, and fake binaries. Tests must never
read the developer's Claude or Codex homes or start a real provider CLI.

Application tests call `t.Parallel()`. `TestMain` detaches the process home,
and the fixtures give each App its own provider home through
`credentialHomeOverride`; resolve test provider paths with `testProviderHome`
or `testProviderProjectsDir`, not the environment. A test stays serial only
when it sets the environment, replaces a package-level variable such as a
`network` or WSL seam, resets or inspects a process-wide cache
(`claudecatalog`, editor detection, the default discovery caches), captures
the global logger, counts process-wide goroutines or memory, or must act
inside a fixed real-time window such as a triage flush timer.

Application tests run with the repository root as their working directory for
whole-tree contracts and committed fixtures. Prefer `t.TempDir()` and explicit
paths in new tests. Test responsibility-owned behavior in its narrower
package; keep tests here for binding, lifecycle, and multi-service composition.
