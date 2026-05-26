# Invariants

Load-bearing rules the whole system depends on. If you're about to
change code that touches any of these, read the rationale first — each
one exists because violating it produced (or would produce) a specific
user-visible bug.

For contributor guardrails that are softer than these (file sizes,
naming), see [`conventions.md`](conventions.md). For recipes that walk
through common changes, see [`how-to.md`](how-to.md).

---

## 1. `item_index` is immutable after first upsert

**Rule.** `items.item_index` is assigned by the store at insert time;
never rewritten. Subsequent upserts to the same `(thread_id, id)` row
preserve its position.

**Rationale.** `item_index` is the ordering key the frontend reads to
lay out the timeline. If two concurrent events could both race to
assign the same `item_index`, rows would shuffle on every refresh. If
an update could shift a row's index, history would renumber itself
mid-conversation and the scroll position would jump.

**Enforcement.** `UNIQUE INDEX idx_items_thread_turn_item_unique`
(migration v15 in `internal/store/migrate.go`) — two rows in the same
thread+turn cannot share an `item_index`. The upsert path in
`internal/store/items.go` only computes `item_index` on INSERT; UPDATE
leaves the existing value untouched.

**Test.** `TestUpsertItemIdempotentPreservesItemIndex` in
`internal/store/items_lifecycle_test.go`, plus the unique-index
test `TestItemIndexUniqueConstraintBlocksDuplicate` in
`items_parent_test.go`.

---

## 2. `item.id` is stable from stream start through completion

**Rule.** A streaming `assistant_text` with id `text:5:2` stays
`text:5:2` when it flips to `completed`. No ID rewrite at state
transitions.

**Rationale.** The frontend tracks streaming items by id; if the id
changed at completion, the reactive diff would see "old item removed,
new item added" and re-mount the DOM node. That breaks selection,
copy-paste, and any in-progress user interaction (e.g., expanding
thinking). Also: `completion_of` and `parent_id` are back-references
to ids — an id rewrite would require a cascade rewrite of every
referencing row, which we don't do.

**Enforcement.** ID format is deterministic per kind (see "Item ID
schemas" in [`chat-rewrite.md`](chat-rewrite.md)). The triage router
computes the id from `(turn_index, segment_index)` etc. at the first
event and keeps computing the same value on subsequent deltas.

**Test.** The streaming-settle coverage in
`internal/triage/stream_state_test.go` and the upsert-idempotency
tests in `internal/store/items_lifecycle_test.go`
(`TestUpsertItemIdempotentPreservesItemIndex` in particular —
replay re-upserts find the same row, which requires id stability).

---

## 3. `turn_index` is monotonic per thread

**Rule.** `turn_index` on `items` is assigned under the per-thread send
mutex. The first item in an empty thread uses turn index `0`; later
user sends use `LastTurnIndex(threadID) + 1`. It never decreases within
a thread's history.

**Rationale.** Turn ordering is how we group items, order message
checkpoints, and scope the interrupt queue. A non-monotonic turn_index
would either group unrelated items into one turn (rollback drift) or
split one turn into two (orphan items, orphan message checkpoint).

**Enforcement.** `App.SendMessage` in `app_send.go` holds the
per-thread action lock (`a.threadLocks().Lock(threadID)`) while
`HasItems` / `LastTurnIndex` → compute → insert happens. Combined with
the store's `SetMaxOpenConns(1)` in `internal/store/store.go`, this
means no two events race on turn_index assignment.

**Test.** `TestSendMessageIncrementsTurnIndex` in
`app_send_test.go`, plus the concurrent-write coverage in
`app_concurrent_test.go` and
`TestConcurrentAppendItemAssignsUniqueIndex` in
`internal/store/items_parent_test.go`.

---

## 4. FIFO drain for the interrupt queue

**Rule.** Parallel tool completions that queue during one streaming
cycle must flush in arrival order so a `_End` never renders before
its `_Begin` partner.

**Rationale.** This was the "end card renders before begin card" bug.
If two backgrounded tools complete while assistant text is streaming,
both completions get deferred into the interrupt queue. Without FIFO
drain, the queue could re-emit them in reverse order, causing the
completion row to appear above the launch row in the timeline.

**Enforcement.** `Router.drainInterruptQueue` in
`internal/triage/stream_state.go` drains entries in insertion order
(slice, not map iteration).

**Test.** The interrupt-queue ordering tests in
`internal/triage/tool_lifecycle_test.go` and the e2e lifecycle test
in `app_e2e_lifecycle_test.go`.

---

## 5. `completion_of` references a `tool_call` with `is_background=true`

**Rule.** Inline tool_calls mutate in place — they never produce a
`tool_completion`. Only backgrounded launches do. Every
`items` row with `kind='tool_completion'` points via `completion_of`
at a `tool_call` row with `is_background=1`.

**Rationale.** `tool_completion` exists to carry the rich result card
for a tool that finished after the launch had already scrolled past.
Creating a completion row for an inline tool would duplicate the
tool's result display.

**Enforcement.** `Router.handleToolComplete` in
`internal/triage/tool_lifecycle.go` checks the launch row's
`is_background` before emitting a completion row; otherwise it
updates the launch row in place.

**Test.** `TestToolCompleteFlipsInlineStatus` and
`TestToolCompleteOnBackgroundedAppendsCompletion` in
`internal/triage/tool_lifecycle_test.go`.

---

## 6. At most one `tool_completion` per `tool_call`

**Rule.** The relationship is 1:1. Synthetic "stopped by user"
completions use the same id (`complete:<tool_call.id>`) and therefore
upsert-replace any pre-existing completion row.

**Rationale.** If stop-then-late-completion produced two rows, the
UI would render two result cards for the same tool. The id format
guarantees replacement.

**Enforcement.** Deterministic id format
(`complete:<tool_call.id>`) + `INSERT OR REPLACE` semantics of
`UpsertItem`.

**Test.** `TestAppendCompletionItemPairsLaunchAndCompletion` and
`TestAppendCompletionItemForcesInvariants` in
`internal/store/items_lifecycle_test.go`.

---

## 7. `parent_id` points to a `tool_call`

**Rule.** Only `tool_call` kinds can be parents (subagent containers,
MCP tools with nested tool_calls). Not `assistant_text`, not
`thinking`, not `user_text`.

**Rationale.** The nesting semantics are "tool did work that produced
sub-events." A text item can't produce sub-events; treating it as a
parent would let the UI render child tool_calls under a text bubble,
which is nonsense.

**Enforcement.** `Router.persistItem` in `internal/triage/router.go`
calls `shouldDropParentID` before the insert; dangling or
cycle-producing parent_ids are silently dropped (logged). The partial
index `idx_items_parent` is filtered on non-empty parent_id.

**Test.** `internal/store/items_parent_test.go` (column round-trip,
listItems projection) plus the parent-drop coverage invoked through
the `persistItem` tests in `internal/triage/router_test.go`.

---

## 8. One writer per thread, one item stream per thread

**Rule.** The per-thread action lock (`threadActionLocks` in
`app_thread_locks.go`) serializes send/steer/flush/revert/fork flow;
`SetMaxOpenConns(1)` serializes SQLite writes. Together these mean no
two events for the same thread race on `item_index` assignment.

**Rationale.** If two goroutines could concurrently assign
`item_index`, one of them would lose the `UNIQUE` index race at
commit time — by which point the first row might already be visible
to the frontend. Serializing up-front keeps `item_index` contiguous
and eliminates the retry path.

**Enforcement.** `a.threadLocks().Lock(threadID)` in
`app_thread_locks.go` + `db.SetMaxOpenConns(1)` in
`internal/store/store.go`.

**Test.** `app_concurrent_test.go` drives concurrent sends;
`TestConcurrentAppendItemAssignsUniqueIndex` and
`TestConcurrentAppendItemWithPayloadAssignsUniqueIndex` in
`internal/store/items_parent_test.go` cover the store-side race
by assertion.

---

## 9. `segmentIndexByScope` / `blockIndexByScope` keyed by `(threadID, turn_index, scope)`

**Rule.** The counters used to mint `text:N:M` and `think:N:K` ids
are keyed by a triple: `(threadID, turn_index, scope)`, where `scope`
is either `""` (top-level, parent thread) or a subagent `card_id`
(for child items inside that card). Reset to 0 on `EventTurnStart`
(top-level scope) or on card creation (child scope). Incremented on
every `EventToolStart` within that scope.

**Rationale.** Without the scope key, two subagents launched in the
same turn would both mint `text:5:0` for their first child segment
and collide on upsert. With the triple, each scope counts
independently.

**Enforcement.** `Router` state in `internal/triage/router.go` (the
maps are defined on the struct; bump sites live in
`stream_items.go` / `block_events.go`).

**Test.** `TestTextItemIDDisambiguatesSubagentScopes` and the
surrounding subagent-id coverage in
`internal/triage/subagent_test.go`.

---

## 10. Subagent child events inherit the subagent card's `turn_index`

**Rule.** A subagent spawned in turn 5 that emits events while the
parent has moved to turn 7 persists its events under turn 5 (the
launching turn). `item_index` remains monotonic across the full
thread.

**Rationale.** If child events inherited the parent thread's
current turn_index, the turn the user is actively typing into would
get polluted by background subagent output, and the revert-modes
would restore the wrong baseline.

**Enforcement.** The Codex child-event re-emission path (in
`internal/provider/codex/session.go`) stamps the subagent card's
`turn_index` onto the re-emitted event before triage sees it. The
Claude Task path preserves `parent_tool_use_id` which triage then
resolves to the card's turn.

**Test.** `TestParentToolUseIDFlowsThroughInlineEmit` and
`TestParentToolUseIDPersistsOnTurnText` in
`internal/triage/subagent_test.go`.

---

## 11. `item_index` is assigned in intended-appearance order, not wire-arrival order

**Rule.** For inline events, the begin event fixes the index and the
end event mutates the same row — automatic. For anything that triage
creates as a NEW row during an active streaming phase (currently:
backgrounded `tool_completion`), the streaming-phase interrupt queue
defers `item_index` assignment until the stream settles.

**Rationale.** If a backgrounded tool completes mid-stream, the new
completion row would get the next available `item_index` and render
ABOVE the still-streaming text row (which got its lower index when
the segment began). The queue defers the insert until streaming
settles, then assigns indexes in the intended visual order.

**If new event kinds are added later that produce fresh rows mid-turn,
they must route through the queue too**, or the same "new row inserts
before the streaming tail" bug recurs.

**Carve-out: deferred queued user_text (`RegisterPendingFlushSend`).**
Queued user messages dispatched at a boundary are NOT routed through
the interrupt queue. `persistDeferredUserText` calls the standard
`persistItem` path at echo time, which allocates `MAX+1` of the
dispatch-decided turn. This is correct because the "intended visual
order" for a queued send is the tail at the moment the agent observes
it — anything the model emits between dispatch and echo should sort
BEFORE the queued message, not after. Capturing an item_index at
dispatch (and then inserting at that captured slot via the now-removed
`InsertItemAtIndex`) was the queued-message ordering regression
documented in
`internal/triage/handle_user_text_test.go::TestHandleUserText_DeferredFlush_LandsAfterContentThatArrivedFirst`.

**Enforcement.** `Router.maybeDeferOrPersist` /
`Router.drainInterruptQueue` in
`internal/triage/stream_state.go`.

**Test.** Queue ordering coverage in
`internal/triage/tool_lifecycle_test.go` alongside the end-to-end
`app_e2e_lifecycle_test.go` scenarios that drive mixed inline +
backgrounded mid-stream completions. Queued-user-text MAX+1-at-echo
coverage in
`internal/triage/handle_user_text_test.go::TestHandleUserText_DeferredFlush_LandsAfterContentThatArrivedFirst`
and `app_flush_queue_test.go::TestDispatchFlush_EchoLandsAfterRowsThatArrivedFirst`.

---

## 12. `persistItem` is the single write+emit chokepoint

**Rule.** Every timeline row that lands in SQLite goes through
`Router.persistItem`. The same function handles `parent_id` cycle
guards, store upsert, canonical `provider:item_event` emission, and the
persisted-items counter.

**Rationale.** Split write/emit paths are how "item appears in DB but
not on screen" (or vice versa) bugs happen. Centralizing the two
operations means a caller can't accidentally persist without emitting
or emit without persisting. It also gives us exactly one place to
enforce cross-cutting concerns (parent_id drop, counter bump, future
observability hooks).

**Enforcement.** Callers under `internal/triage/` route through
`persistItem`; `go vet`-style conventions are enforced by code review
(there is no lint rule, so a reviewer must notice a direct
`store.UpsertItem` call).

**Test.** Every lifecycle test exercises the full persist → emit
path; absence of a direct emit for a persist would fail the
end-to-end sequence tests.

---

## 13. Provider adapters don't write to the store directly

**Rule.** `internal/provider/{claude,codex}` produce normalized
`ProviderEvent` values on a channel. They do not hold a
`*store.Store` reference and do not call `app.Event.Emit`.

**Rationale.** Principle 6: "Provider-specific code stays in
provider-specific packages." Triage and store are provider-agnostic;
letting the provider write to SQLite would force the store to handle
provider-specific types and defeat the abstraction.

**Enforcement.** Architectural — the `provider/` package `AGENTS.md`
calls this out explicitly, and the package has no import of
`internal/store`. Reviews catch any new import.

**Test.** No dedicated test; the absence of a `store` import in
`go mod graph`-style tooling would suffice if we had it.

---

## 14. Triage contains no provider-specific types

**Rule.** Triage reads `provider.ProviderEvent` and its typed fields
only. No case statements on provider-name strings; no
Claude-/Codex-specific structs.

**Rationale.** Same principle as #13 from the other side. If triage
branches on "if claude then A else B," the promise that triage is
provider-agnostic is broken, and adding a third provider would
require opening every triage handler.

**Enforcement.** `internal/triage/AGENTS.md` rule "No
provider-specific types." Reviews enforce.

**Test.** Architectural review; no automated gate.

---

## 15. Cost computation lives in provider adapters, not triage

**Rule.** `CalculateCost` (in `internal/provider/cost.go`) is called
by the provider adapter when it attaches turn usage/cost accounting to
turn completion metadata. Triage does not recompute cost.
Context-window `EventTokenUsage` events are meter snapshots, not cost
events.

**Rationale.** Model pricing is provider knowledge (per-provider
pricing tables); putting the calculation in triage would leak model
awareness into the provider-agnostic layer. See the
[`triage-routing.md`](triage-routing.md) entry for `token_usage`.

**Enforcement.** `handleTokenUsage` in `internal/triage/router.go`
only accepts provider-normalized context-window snapshots. Turn
usage/cost accounting is carried on turn completion metadata.

**Test.** `internal/provider/cost_test.go`.

---

## 16. Every `EventKind` has a triage handler

**Rule.** Every `provider.EventKind` constant is present in
`provider.AllEventKinds` AND has a matching `case` in
`Router.Handle`. The `default` branch in `Handle` returns
`ErrUnhandledEventKind` so a missing case is caught on the test
loop rather than silently dropped at runtime.

**Rationale.** A kind the router doesn't handle is a silent drop —
the event evaporates and the frontend sees nothing. The Go-side
exhaustiveness tests guarantee that every declared kind is
reachable; if a new kind is added to the enum but not handled, the
test fails.

**Enforcement.** `TestHandleEveryEventKindCovered` and
`TestAllEventKindsListIsComplete` in
`internal/triage/router_test.go`. The former loops
`provider.AllEventKinds` and asserts `Handle` returns without the
unhandled-kind sentinel; the latter guards the drift between the
`const` block and the `AllEventKinds` slice.

**Test.** `TestHandleEveryEventKindCovered` and
`TestAllEventKindsListIsComplete` (router_test.go).

**Frontend.** The frontend subscribes to typed routing channels
(`provider:item_event`, approval, usage/status, turn lifecycle,
subagent/background notifications, and app-shell events) rather than
branching on individual `EventKind`s, so this invariant is Go-only
today. If the frontend ever starts branching on a new enum it receives
over the wire, add a `never`-guard pattern at that switch.

---

## 17. Every long-lived map has a documented cleanup path

**Rule.** Every map on `Router`, `Parser`, or any other long-lived
struct has an explicit lifetime boundary and a documented cleanup
site. Per-turn maps clear on `EventTurnComplete`; per-thread maps
clear on `CleanupThread`; correlation maps clear when their
correlated event resolves.

**Rationale.** Without cleanup paths, every new per-thread map is a
slow memory leak that only shows up in sessions that accumulate
dozens of turns. The triage router accumulated three such leaks
during the chat rewrite before we formalized the cleanup rule.

**Enforcement.** `internal/triage/AGENTS.md` documents the current
set and the rule. Adding a new map requires documenting its cleanup
in the same commit.

**Test.** `TestCapturedTurnsClearOnTurnComplete` and
`TestUnknownSessionStatusLoggedStaysBounded` in
`internal/triage/memory_cleanup_test.go`, plus per-map cleanup
coverage under `router_test.go` (`CleanupThread` paths).

---

## 18. SessionRef writes clear PendingForkRef atomically

**Rule.** `store.UpdateSessionRef` in `internal/store/threads.go`
writes the new `session_ref` and clears `pending_fork_session_ref`
in the same statement.

**Rationale.** A freshly-forked thread points at the source session
via `PendingForkRef`. The first time we start under it, we use
`--fork-session --resume <source-ref>` to clone and get a new session
id. If the write that captures the new session id didn't also clear
`PendingForkRef`, the next restart would re-fork from the source and
create a second branch of the same timeline.

**Enforcement.** `Store.UpdateSessionRef` is a single UPDATE that
sets both columns.

**Test.** `TestUpdateSessionRefClearsPendingForkRef` in
`internal/store/threads_test.go`.

---

## 19. WAL mode is verified at startup

**Rule.** `configureDatabase` in `internal/store/migrate.go` reads
back `PRAGMA journal_mode=WAL` and logs a visible warning when
SQLite silently falls back to rollback journaling. The app proceeds
on a rollback-journaled DB (still correct), but the log line is the
only signal that SQLite concurrency has degraded.

**Rationale.** SQLite silently downgrades `journal_mode=WAL` to the
prior mode under some filesystems (network mounts, read-only mounts,
shared-cache DBs). A store running in DELETE mode has different
concurrency semantics — concurrent readers during a write block on
the file — which we rely on not blocking for responsive thread
switches. The warning tells us that the concurrency model has
degraded so we can investigate before users hit the visible symptom.

**Note.** Earlier docs described this as "boot fails loudly." The
code today logs and continues. If the hard-fail behavior is desired,
it needs a deliberate change plus a test — today's rule is "warn
and proceed."

**Enforcement.** `configureDatabase` runs on every `Open` via
`runMigrations`; there is no way to construct a `*Store` that
skipped it.

**Test.** Happy-path WAL coverage is in
`internal/store/store_test.go`. No explicit fallback-mode test
today.

---

## 20. Every `tool_use` emits exactly one tool-lifecycle completion

**Rule.** Every `tool_use` the provider emits produces exactly one
`EventToolStart` and exactly one `EventToolComplete` keyed by its own
`tool_use_id`. No ID rewriting between start and complete. No
"consumed by another handler." Task-lifecycle enrichments (Claude's
TaskOutput, `task_updated` terminal) emit a SEPARATE
`EventBackgroundTaskTerminal` — they never substitute for the
tool-lifecycle completion.

**Rationale.** Violating this produced the TaskOutput stuck-`running`
bug: the parser consumed TaskOutput's `tool_result` to emit a
completion for a backgrounded task's `tool_use_id`, skipping
TaskOutput's own completion. TaskOutput's `tool_call` row stayed
`running` forever; `isTurnActive` stayed true; the "Working…"
indicator never cleared.

**Enforcement.** Claude parser (`parse_user.go`) always runs the
standard completion path before any task-lifecycle enrichment; task
enrichment is additive via `EventBackgroundTaskTerminal`, never via
the tool-completion event. Codex's `item/started`/`item/completed`
are symmetric one-shot upserts by design.

**Test.** Replay
`docs/references/fixtures/claude/ndjson_task.log` and
`docs/references/fixtures/claude/taskoutput_multi.ndjson` through the
parser; assert TaskOutput's own `tool_use_id` receives an
`EventToolComplete` in addition to any task-terminal events.

**See also.** [`turn-lifecycle.md §Tool lifecycle`](turn-lifecycle.md#1-tool-lifecycle);
[`claude-wire.md §tool_result`](../references/claude-wire.md#user-message--tool_result-blocks).

---

## 21. `task_notification` is not a completion source (but it can drain a stash)

**Rule.** The Claude `system/task_notification` envelope is not a
completion **status** source — its arrival never decides whether a
task ended in `completed` / `failed` / `killed`. Lifecycle status
comes only from `system/task_updated` `patch.status` and TaskOutput
`tool_use_result.task.status`.

`task_notification` IS a valid timing trigger for the agent-observed
half of the two-phase task terminal flow: when a stash row exists in
`pending_background_task_terminals` for the same `task_id`, the
notification's arrival means the agent has now observed the
queued completion attachment, and triage drains the stash and writes
the `tool_completion` sibling at the current write head (using the
status from the stash, not from the notification).

**Rationale.** `task_notification` is an "attention signal" that
Claude fires so the next user turn's prompt includes the task's
summary. It also fires for non-terminal foreground bash (with
`output_file: ""`), so treating its arrival as the *status*
authority would corrupt the lifecycle. The two-phase decoupling
means the host-side process exit (the status authority) lands
earlier in `task_updated`; the notification only chooses *when* the
chat row appears.

**Enforcement.** `parseTaskNotificationEvent` in `parse_system.go`
emits `EventBackgroundTaskNotification`, never a lifecycle terminal
or tool completion. `triage/background_task_notifications.go`
persists the notification row first, then drains any matching stash
via `TakePendingBackgroundTerminal` and routes the merged data
through the same `writeBackgroundCompletionSibling` helper used by
the TaskOutput observation path. Status carried on the notification
itself is ignored for lifecycle purposes (the stash + task_updated
already settled it).

**Test.** `parse_system_test.go` asserts `task_notification` emits
one `EventBackgroundTaskNotification` and zero lifecycle events.
`triage/background_task_notifications_test.go` asserts the stash
drain + sibling write behaviour.

**See also.** [`turn-lifecycle.md §Task lifecycle`](turn-lifecycle.md#2-task-lifecycle-claude-only);
[`turn-lifecycle.md §Tray decoupling`](turn-lifecycle.md#tray-decoupling--process-state-vs-agent-observation-tray-a);
[`claude-wire.md §task_notification`](../references/claude-wire.md#systemtask_notification).

---

## 22. Turn activity is wire-pushed, never derived from items

**Rule.** The frontend's "Working…" indicator and any "is the agent
busy?" gate come exclusively from provider-pushed
`provider:turn_started` / `provider:turn_completed` events. Never
derive turn state from item state (e.g., `items.some(running
tool_call)`). Never compute "is working" from backend process
liveness. The active-turn record lives in a single global registry
keyed by threadId (`frontend/src/lib/stores/threadStatuses.svelte.ts
→ getActiveTurn`); panes do not hold a per-pane copy.

**Rationale.** Deriving turn state from items means any parser bug
that drops a completion freezes the UI. Deriving from process
liveness means a legitimately backgrounded task makes the agent look
like it's still working. The single-source-of-truth global registry
(rather than a per-pane field) means the indicator survives a
thread switch — switching away from and back to a thread that's
still working preserves the live record because nothing in pane
lifecycle clears the global map. Invariant 20 (force every tool_use
to complete) addresses the parser-bug case; this invariant ensures
the UI reads the authoritative wire signal even if a future parser
bug resurfaces.

**Enforcement.** `getActiveTurn(threadId)` reads from the global
`activeTurnByThread` map in `threadStatuses.svelte.ts`. The map is
populated only by `projectTurnStarted` (called from the
`provider:turn_started` event listener) and cleared only by
`projectTurnCompleted`. Every reader (chat working indicator,
sidebar pill, message timeline empty-state, composer mid-turn gate,
LiveTodoPanel pull-up, workspace-change lock) calls
`getActiveTurn(pane.threadId)` directly — no per-pane shim, no
parallel state slice. No code path rehydrates the registry from
SQLite or item state.

**Test.** Frontend test: simulate a stuck `tool_call` row + empty
registry; assert the working indicator is hidden. Regression test
in `ChatWorkingIndicator.test.ts`: switch away from a thread with
a live turn and back; assert the indicator returns.

**See also.** [`turn-lifecycle.md §Frontend state shape`](turn-lifecycle.md#frontend-state-shape).

---

## 23. Turn-complete force-closes orphan non-background tool_calls

**Rule.** When a turn's `EventTurnComplete` arrives, triage flips
every `tool_call` row matching
`status='running' && !is_background && turn_index=currentTurn` to
`status='errored'` with a synthesized completion summary.
Backgrounded launches are exempt.

**Rationale.** Provider bugs (or a crash mid-turn) can drop a
`tool_result`, leaving a tool_call row orphaned at `running`. The
turn-complete handler acts as a safety net so the timeline always
settles cleanly. Backgrounded launches are exempt because their
work can legitimately outlive the turn — see invariant 24.

**Enforcement.** `handleTurnComplete` in
`internal/triage/turn_lifecycle.go` iterates the turn's items and
flips any matching rows before emitting `provider:turn_completed`.

**Test.** Integration test: synthesize a `tool_use` with no matching
`tool_result`, fire `EventTurnComplete`, assert the row flips to
`errored`.

---

## 24. Backgrounded work outlives its launching turn

**Rule.** A `tool_call` with `is_background=true` can remain
`status='running'` after its launching turn has completed. This is
expected for Claude background Bash / Task and for Codex projected
background commands / subagents. The background tray shows the live
launch, and the timeline shows both the launch and, when it lands, the
sibling `tool_completion` row.

**Rationale.** Claude's `task_updated` terminal / TaskOutput
enrichment and Codex's background terminal / subagent completion
signals can arrive AFTER the turn that launched the work. Agents
expect this — they send messages that dispatch background work and
move on. Captured Claude evidence:
`docs/references/fixtures/claude/ndjson_outlives.log` shows the turn-1 `result`
landing before the backgrounded task's `task_updated`.

**Enforcement.** Triage's force-close (invariant 23) exempts
`is_background=true` rows. Background completions append at the
current thread write head when one is open, otherwise the latest
persisted turn. Tray derivation clocks retention off the completion
row's `createdAt`, not turn boundaries.

**Test.** Replay `ndjson_outlives.log` through the full pipeline;
assert the turn closes cleanly and the background task's
`tool_completion` sibling row is written when its `task_updated`
arrives later.

**See also.** [`turn-lifecycle.md §Task lifecycle`](turn-lifecycle.md#2-task-lifecycle-claude-only)
and [`turn-lifecycle.md §Codex background projection`](turn-lifecycle.md#codex-background-projection).

---

## 25. Codex backgrounding uses wire-typed signals, never heuristics

**Rule.** `is_background=true` may only be set on a Codex item when a
**wire-typed** signal authorizes it. Two sanctioned authorization signals
today:

1. A typed `item/commandExecution/terminalInteraction` notification for an
   empty `write_stdin` poll targets the process, proving the model explicitly
   waited on a live background PTY. See [`codex-wire.md`](../references/codex-wire.md).
2. `collabAgentToolCall` with `tool == "spawnAgent"` whose
   `agentsStates` map reports a non-terminal child at the parent's
   `turn/completed` boundary. The spawn row itself completes on the
   wire immediately; backgrounding reflects that work continues on
   the child thread past the parent's turn.

Heuristic classifiers ("assistant text after tool start", "turn still
open while tool running", etc.) are forbidden as the AUTHORIZATION for
setting `is_background`. That rule is what protects against the
ghost-row problem the former `BackgroundClassifier` had. The TRIGGER
for stamping an already-authorized row is not a classifier; it's the observable
moment at which the wire-typed commitment becomes visible. The canonical typed
`item/commandExecution/terminalInteraction` notification for an empty
`write_stdin` poll stamps the matching process because that poll is itself
model-visible evidence that the PTY is a background terminal. Raw
`exec_command` output can enrich live process metadata, but it must not gate or
fabricate transcript history. See
`internal/triage/codex_background.go`.

**Rationale.** Codex has no `run_in_background` flag on ThreadItems, but
it does have backgrounded execution: `exec_command` yields to the model
after `yield_time_ms` (10s default) while the PTY keeps running in
`UnifiedExecProcessManager`. The tool call item stays `inProgress` until
`spawn_exit_watcher` eventually fires `ExecCommandEnd` — potentially
across turns, up to `background_terminal_max_timeout` (1h default).
`source: "unifiedExecStartup"` is the unambiguous wire signal for "this item is
a unified exec candidate"; typed `TerminalInteraction` is the visible wait
signal. Typed `item/completed` owns transient live-state cleanup and owns the
command transcript row with the same item id only while a Codex wire round is
active, matching Codex TUI timing. Treating raw `function_call_output` text as
the history source is wrong because it is model-facing transcript, not UI
history.

`spawn_agent` child threads live on their own `thread_id` and the
parent's `spawn_agent` tool_call completes on the wire immediately. The
`is_background` flag on the parent's tool_call row reflects that the
referenced child thread may still be running — authorized by the
wire's `agentsStates` map, not by any event-ordering heuristic.
This was re-checked against `codex-cli 0.128.0` on 2026-05-04: the
parent `turn/completed` arrived before the child thread's
`turn/completed`, so no Codex soft-round-close path is needed.

**Enforcement.** Only `internal/triage/codex_background.go` and the
`internal/provider/codex/` enrichment that feeds it may set
`is_background=true` on Codex items. No heuristic derivation elsewhere.
The former `BackgroundClassifier` at
`internal/provider/codex/background.go` remains deleted. The
`BackgroundTaskTray` component renders nothing when no items have
`is_background=true`.

**Test.** Codex projector unit tests:
- `TestCodexUnifiedExecStartWaitsForTypedTerminalInteraction` — text,
  reasoning, and turn-complete events do not background unified exec without
  a typed wait signal; the raw running result can still enrich live state but
  not transcript history or tray backgrounding.
- `TestCodexUnifiedExecQuickCompletionPersistsNormalCommand` —
  typed command completion persists as a normal command row.
- `TestCodexUnifiedExecIdleCompletionAfterTurnCompleteClearsTransientStateWithoutHistory`
  and
  `TestCodexUnifiedExecIdleCompletionAfterInterruptedTurnClearsTransientStateWithoutHistory`
  — late typed completions clear the live tracker but do not append chat
  history once no Codex wire round is active.
- `TestCodexUnifiedExecLateRawYieldResultDoesNotCreateDuplicate`
  — a late raw exec result cannot create duplicate or reordered transcript
  history after typed command completion.
- `TestCodexSubagentRunningPastTurnEnd_Backgrounded` — spawn_agent
  with running child stamps background at parent turn close.
- `TestCodexSubagentCompletion_WaitResolvesBackgroundedSpawn` and
  `TestCodexSubagentCompletion_SynthesizesSiblingAtTail` — wait and
  notification terminal paths synthesize sibling completions.

**Upstream gap.** The app-server protocol exposes only thread-wide
cleanup (`thread/backgroundTerminals/clean`). Per-process termination
requires model-facing tools (`write_stdin`) that aren't client-callable.
Per-row stop for Codex background terminals is deferred pending upstream
(see [`codex.md §Known upstream constraints`](../references/codex.md#known-upstream-constraints)).

**See also.** [`codex-wire.md §The two critical differences from Claude`](../references/codex-wire.md#the-two-critical-differences-from-claude).

---

## 26. Migrations are forward-only and append-only

**Rule.** Never edit a migration that has shipped. Add a new one. The
`migrations` slice in `internal/store/migrate.go` is an ordered list;
versions are contiguous integers.

**Rationale.** An edited migration would apply differently to users
who've already run the original version vs. fresh installs. That
drift produces "works on my machine" schema bugs that are
untraceable.

**Enforcement.** Code review. Changing an existing `SQL:` string
on a shipped `Version: N` entry is disallowed.

**Test.** The migrate tests apply migrations in order; a new
migration adds a new `TestMigrateVXX*` block.

---

## 27. Soft round-close from `message_delta.stop_reason` is wire-typed

**Rule.** The Claude adapter emits `EventTurnComplete` from
`stream_event.message_delta.delta.stop_reason` (with
`parent_tool_use_id == null`) when the stop_reason is one of the
authorized "model has stopped" values:

- `"end_turn"`
- `"stop_sequence"`
- `"refusal"`

It does NOT emit on:

- `"tool_use"` — model paused for a tool; more text follows
- `"pause_turn"` — model explicitly asked for more time
- `"max_tokens"` — harness may auto-continue

The emitted event carries a typed `provider.SoftRoundCloseMeta` with
`stop_reason` and the parser's PEEKED `assistant_message_id` (peek,
not take — the trailing wire `result` envelope still consumes via
`takeLastAssistantMessageID`). Usage/cost are NOT on the soft event
— those land on the trailing `result`. Triage's
`handleTurnComplete` settles the round on this signal; the trailing
wire `result` envelope folds late payload via
`persistLateTurnPayload` → `store.UpdateTurnLatePayload`. Per-column
fold semantics:

- `token_usage_json`: first non-empty wins (preserves first
  settle's value).
- `assistant_message_id`: last non-empty wins (overwrite). Each
  subsequent round's settle replaces the persisted column so it
  always reflects the FINAL assistant message of the turn —
  matches the documented contract on
  `SettledTurn.assistantMessageId`.

**Rationale.** Claude Code 2.1.118 withholds the `result` envelope
when a `local_agent` (Task) subagent is still running at parent
end_turn — the wire turn stays alive even though the parent is
idle. Without this signal, the working indicator stays on for the
full subagent runtime (~10s in the captured spike).
Backgrounded `local_bash` does NOT trigger this delay — the
distinction is keyed on `local_agent` specifically. Backed by
fixtures
[`local_agent_outlives.ndjson`](../references/fixtures/claude/local_agent_outlives.ndjson),
[`local_agent_user_input_during_wait.ndjson`](../references/fixtures/claude/local_agent_user_input_during_wait.ndjson),
[`local_agent_plus_bg_bash.ndjson`](../references/fixtures/claude/local_agent_plus_bg_bash.ndjson).

The gating rules are not heuristics — `parent_tool_use_id == null`
distinguishes parent messages from subagent messages, and the
authorized stop_reason set is the closed list documented in
[`claude-wire.md`](../references/claude-wire.md). Treating this as a
wire-typed signal is consistent with invariant 25's "wire-typed
signals, not heuristics" rule.

The composer / Stop button safely unblock alongside the indicator.
Spike fixture `local_agent_user_input_during_wait.ndjson` confirms
Claude CLI accepts mid-wait stdin within 32ms (re-rounds cleanly,
parent processes the new message coherently, original subagent
keeps running uninterrupted).

**Enforcement.** Only `parse_stream.go`'s `message_delta` case may
emit `provider.SoftRoundCloseMeta`. The gating logic
(`parent_tool_use_id == null` + closed stop_reason set) is unit-
tested. The `result` envelope path remains authoritative for
cumulative token usage / cost. Claude's assistant id is derived from
the parser's last in-stream assistant `message.id` (not from a raw
`result.assistant_message_id` field); the `assistant_message_id`
column on the persisted row converges on the FINAL round's id via
`UpdateTurnLatePayload`'s last-write-wins rule for that column.

**Test.**
- `partial_messages_test.go` unit tests that gate on
  `parent_tool_use_id` and stop_reason set.
- Fixture replay test using `local_agent_outlives.ndjson` asserts
  `EventTurnComplete` fires from the parent's message_delta
  stop_reason=end_turn before the wire `result` envelope.

**See also.**
[`claude-wire.md §Soft round close`](../references/claude-wire.md#soft-round-close--message_deltastop_reason),
[`turn-lifecycle.md §Wire-round vs logical-turn cadence`](turn-lifecycle.md).

---

## See Also

- [`chat-rewrite.md`](chat-rewrite.md) — the spec these rules were
  distilled from.
- [`conventions.md`](conventions.md) — softer contributor guardrails.
- [`how-to.md`](how-to.md) — step-by-step recipes for common changes.
- [`adrs/`](adrs/) — the decisions behind these rules.
