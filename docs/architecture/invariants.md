# Invariants

Load-bearing rules the whole system depends on. If you're about to
change code that touches any of these, read the rationale first. Each
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
(migration v15 in `internal/store/migrate.go`). Two rows in the same
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
to ids, and an id rewrite would require a cascade rewrite of every
referencing row, which we don't do.

**Enforcement.** ID format is deterministic per kind (see "Item ID
schemas" in [`chat-rewrite.md`](chat-rewrite.md)). The triage router
computes the id from `(turn_index, segment_index)` etc. at the first
event and keeps computing the same value on subsequent deltas.

**Test.** The streaming-settle coverage in
`internal/triage/stream_state_test.go` and the upsert-idempotency
tests in `internal/store/items_lifecycle_test.go`
(`TestUpsertItemIdempotentPreservesItemIndex` in particular:
replay re-upserts find the same row, which requires id stability).

---

## 3. `turn_index` is monotonic per thread

**Rule.** `turn_index` on `items` is assigned under the per-thread send
mutex. The first item in an empty thread uses turn index `0`; later
user sends use `LastTurnIndex(threadID) + 1`. It never decreases within
a thread's history.

**Rationale.** Turn ordering is how we group items, order message
anchors, and scope the interrupt queue. A non-monotonic turn_index
would either group unrelated items into one turn (rollback drift) or
split one turn into two (orphan items, orphan message anchor).

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

**Rule.** Inline tool_calls mutate in place. They never produce a
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
index `idx_items_parent` is filtered on non-empty parent_id; queries
that want it must state `parent_id <> ''` explicitly so the planner
can prove the predicate (see `descendantsCTEFromRoots`).

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
commit time, by which point the first row might already be visible
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
get polluted by background subagent output, and message-boundary
rollback (fork-from-message, revert-on-interrupt) would slice
provider history at the wrong turn.

**Enforcement.** The Codex child-event re-emission path (in
`internal/provider/codex/session.go`) stamps the subagent card's
`turn_index` onto the re-emitted event before triage sees it. The
Claude Task path preserves `parent_tool_use_id` which triage then
resolves to the card's turn. Rows triage synthesizes for a scoped
launch obey the same rule: `backgroundCompletionTurnIndex` routes a
sibling that carries a `parent_id` through `turnIndexForScope`, and
only a top-level sibling takes the write-head placement of invariant
24. A scoped sibling on a later turn sorted after every row the agent
wrote afterwards (2026-09-01).

**Test.** `TestParentToolUseIDFlowsThroughInlineEmit` and
`TestParentToolUseIDPersistsOnTurnText` in
`internal/triage/subagent_test.go`;
`TestHandleEventBackgroundTaskTerminal_ScopedSiblingStaysOnLaunchTurn`
in `internal/triage/tool_lifecycle_test.go`.

---

## 11. `item_index` is assigned in intended-appearance order, not wire-arrival order

**Rule.** For inline events, the begin event fixes the index and the
end event mutates the same row, which is automatic. For anything that triage
creates as a NEW row during an active streaming phase (currently:
backgrounded `tool_completion`), the streaming-phase interrupt queue
defers `item_index` assignment until the **same-scope** stream settles.
Scope is the row's `parent_id`: `""` for the main loop, the Agent
tool_use_id for a subagent.

**Rationale.** If a backgrounded tool completes mid-stream, the new
completion row would get the next available `item_index` and render
ABOVE the still-streaming text row (which got its lower index when
the segment began). The queue defers the insert until streaming
settles, then assigns indexes in the intended visual order.

**Scope-aware deferral.** The defer is SAME-scope only. A main-scope
completion (`parent_id == ""`) must NOT queue behind a concurrent
subagent-scope stream. A subagent's nested text can't be the tail a
main-timeline row would jump above, and a backgrounded subagent streams
continuously, so the queue would drain only at thread idle. That landed
the completion AFTER later main text it actually preceded (thread
`4d82b192` turn 18: Agent A's "Report CPU model → done" rendered below
"First back" instead of above it). The QUEUE decision keys on scope
(`streamingScopeCounts`); the DRAIN stays thread-wide (drain once every
scope is idle), which is conservative and correct, since relative order
within each scope is what matters and a queued row never strands.

**If new event kinds are added later that produce fresh rows mid-turn,
they must route through the queue too**, or the same "new row inserts
before the streaming tail" bug recurs.

**Carve-out: deferred queued user_text (`RegisterPendingFlushSendWithExpectation`).**
Queued user messages dispatched at a boundary are NOT routed through
the interrupt queue. `persistDeferredUserText` calls the standard
`persistItem` path at echo time, which allocates `MAX+1` of the
dispatch-decided turn. This is correct because the "intended visual
order" for a queued send is the tail at the moment the agent observes
it. Anything the model emits between dispatch and echo should sort
BEFORE the queued message, not after. Capturing an item_index at
dispatch (and then inserting at that captured slot via the now-removed
`InsertItemAtIndex`) was the queued-message ordering regression
documented in
`internal/triage/handle_user_text_test.go::TestHandleUserText_DeferredFlush_LandsAfterContentThatArrivedFirst`.

**Enforcement.** `Router.maybeDeferOrPersist` (queues on
`Router.hasActiveStreamingItemForScope`, backed by the per-scope
`streamingScopeCounts`) / `Router.drainInterruptQueue` (drains on the
thread-wide `Router.hasActiveStreamingItem`) in
`internal/triage/stream_state.go`. Both the thread-wide and scoped
counters decrement in `finishSettle` (settle END, not settle start) so
a same-scope completion keeps queuing FIFO across an async settle.

**Test.** Same-scope FIFO queue ordering in
`internal/triage/stream_state_test.go::TestInterruptQueueDrainsInArrivalOrder`;
the scope-aware refinement (a main-scope completion not deferring behind
a concurrent subagent stream) in
`internal/triage/tool_lifecycle_test.go::TestBackgroundCompletionOrdersBeforeLaterMainTextDespiteSubagentStream`,
alongside the end-to-end
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

**Enforcement.** Architectural: the `provider/` package `AGENTS.md`
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

**Rationale.** A kind the router doesn't handle is a silent drop. The
event evaporates and the frontend sees nothing. The Go-side
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
in the same commit. The App turn-observer registry in
`app_turn_observers.go` removes an observer when its idempotent unsubscribe
function runs and deletes empty per-thread buckets; the built-in global
discussion observer intentionally has the App lifetime.

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
concurrency semantics: concurrent readers block on the file during a
write, and responsive thread switches rely on them not blocking. The
warning tells us that the concurrency model has
degraded so we can investigate before users hit the visible symptom.

**Note.** Earlier docs described this as "boot fails loudly." The
code today logs and continues. If the hard-fail behavior is desired,
it needs a deliberate change plus a test. Today's rule is "warn
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
`EventBackgroundTaskTerminal`. They never substitute for the
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
[`claude-wire.md §tool_result`](../references/claude-wire.md#user-message-tool_result-blocks).

---

## 21. `task_notification` is not a completion source (but it can drain a stash)

**Rule.** The Claude `system/task_notification` envelope is not a
completion **status** source. Its arrival never decides whether a
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
[`turn-lifecycle.md §Tray decoupling`](turn-lifecycle.md#tray-decoupling-process-state-vs-agent-observation-tray-a);
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
thread switch. Switching away from and back to a thread that's
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
`getActiveTurn(pane.threadId)` directly: no per-pane shim, no
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
work can legitimately outlive the turn. See invariant 24.

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

The tray lists by BACKGROUNDED ANCESTRY, not by top-level-ness
(`docs/specs/agent-visibility.md` Q8). `Store.ListLiveBackgroundTasks`
returns every live `is_background=1` launch at ANY depth, every live
agent launch that DESCENDS from one (which also supplies the
intermediate ancestors, so the frontend indents by walking `parentId`
within the result), and the recent completion siblings of that set. A
foreground plain tool call under a background agent is NOT a tray row.
It is the agent's own work, rendered inside its card. This is a DISPLAY
rule only: the reaper and queue gates in `items_lifecycle.go`
(`HasRunningTopLevelForegroundToolCall`, `HasLiveBackgroundToolCall`,
`HasQueueBlockingBackgroundToolCall`,
`CountLiveRunningBackgroundToolCalls`,
`MarkLiveBackgroundToolCallsInactive`) and `paging.go`'s
`topLevelItemsFilter` keep `parent_id = ''`. Whether the tray SHOWS a
nested background Bash and whether that Bash blocks the flush queue or
survives a session teardown are different questions.

**Rationale.** Claude's `task_updated` terminal / TaskOutput
enrichment and Codex's background terminal / subagent completion
signals can arrive AFTER the turn that launched the work. Agents
expect this. They send messages that dispatch background work and
move on. Captured Claude evidence:
`docs/references/fixtures/claude/ndjson_outlives.log` shows the turn-1 `result`
landing before the backgrounded task's `task_updated`.

**Enforcement.** Triage's force-close (invariant 23) exempts
`is_background=true` rows. A top-level background completion appends
at the current thread write head when one is open, otherwise the
latest persisted turn; a completion under a subagent launch stays on
the launch's turn (invariant 10). Tray derivation clocks retention off
the completion row's `createdAt`, not turn boundaries.

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
2. Either collaboration version's typed spawn signal:
   - MultiAgentV1 `collabAgentToolCall` with `tool == "spawnAgent"` whose
     `agentsStates` reports a non-terminal child; or
   - MultiAgentV2 `subAgentActivity` with `kind == "started"`, emitted only
     after core successfully created and started `agentThreadId`.
   The V2 adapter normalizes the latter into the same receiver + running-state
   metadata consumed by triage. The spawn row closes on the parent wire while
   work continues on the independently-running child thread.

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
`spawn_exit_watcher` eventually fires `ExecCommandEnd`, potentially
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
referenced child thread may still be running, authorized by V1's
`agentsStates` or V2's canonical started activity, not by any event-ordering
heuristic. V2 child output is quarantined until the typed ownership edge
arrives, including recursively-spawned grandchildren.
The lifecycle ordering was re-checked against `codex-cli 0.128.0` on 2026-05-04: the
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
- `TestCodexUnifiedExecStartWaitsForTypedTerminalInteraction`: text,
  reasoning, and turn-complete events do not background unified exec without
  a typed wait signal; the raw running result can still enrich live state but
  not transcript history or tray backgrounding.
- `TestCodexUnifiedExecQuickCompletionPersistsNormalCommand`:
  typed command completion persists as a normal command row.
- `TestCodexUnifiedExecIdleCompletionAfterTurnCompleteClearsTransientStateWithoutHistory`
  and
  `TestCodexUnifiedExecIdleCompletionAfterInterruptedTurnClearsTransientStateWithoutHistory`:
  late typed completions clear the live tracker but do not append chat
  history once no Codex wire round is active.
- `TestCodexUnifiedExecLateRawYieldResultDoesNotCreateDuplicate`:
  a late raw exec result cannot create duplicate or reordered transcript
  history after typed command completion.
- `TestCodexSubagentRunningPastTurnEnd_Backgrounded`: spawn_agent
  with running child stamps background at parent turn close.
- `TestCodexSubagentCompletion_WaitResolvesBackgroundedSpawn` and
  `TestCodexSubagentCompletion_SynthesizesSiblingAtTail`: wait and
  notification terminal paths synthesize sibling completions.

**Per-row stop (no longer an upstream gap).** This section used to record
per-process termination as blocked on upstream. It shipped in codex
0.140.0 and is verified on 0.146.0: `thread/backgroundTerminals/terminate
{threadId, processId}` → `{terminated}`, alongside
`thread/backgroundTerminals/list` for enumeration and the thread-wide
`clean`. `processId` is on the wire, Agent Overflow stamps it onto the
item (`enrichItemMeta`) and allowlists it onto the transient tray row
(`codexLiveUnifiedExecMeta`), so a tray row joins to its PTY without a
`list` round trip. The RPCs live in
`internal/provider/codex/session_background.go`, `terminate` is bound as
`App.TerminateCodexBackgroundTerminal`, and the tray renders the same
per-row Stop button Claude's `stop_task` renders; see
[`codex.md §Background terminals`](../references/codex.md#background-terminals).

This changes nothing about the rule above. Stopping a background terminal
is a user action on an already-authorized row. It is not, and must not
become, a source of `is_background` authorization. Whether a row is
backgrounded is still decided only by the two wire-typed signals; the
stop RPC merely acts on rows that already are.

What remains genuinely blocked: killing a spawned collab-agent child
thread. `close_agent` is a model tool with no client-callable equivalent.

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

- `"tool_use"`: model paused for a tool; more text follows
- `"pause_turn"`: model explicitly asked for more time
- `"max_tokens"`: harness may auto-continue

The emitted event carries a typed `provider.SoftRoundCloseMeta` with
`stop_reason` and the parser's PEEKED `assistant_message_id` (peek,
not take: the trailing wire `result` envelope still consumes via
`takeLastAssistantMessageID`). Usage/cost are NOT on the soft event.
Those land on the trailing `result`. Triage's
`handleTurnComplete` settles the round on this signal; the trailing
wire `result` envelope folds late payload via
`persistLateTurnPayload` → `store.UpdateTurnLatePayload`. Per-column
fold semantics:

- `token_usage_json`: first non-empty wins (preserves first
  settle's value).
- `assistant_message_id`: last non-empty wins (overwrite). Each
  subsequent round's settle replaces the persisted column so it
  always reflects the FINAL assistant message of the turn,
  matching the documented contract on
  `SettledTurn.assistantMessageId`.

**Rationale.** Claude Code 2.1.118 withholds the `result` envelope
when a `local_agent` (Task) subagent is still running at parent
end_turn. The wire turn stays alive even though the parent is
idle. Without this signal, the working indicator stays on for the
full subagent runtime (~10s in the captured spike).
Backgrounded `local_bash` does NOT trigger this delay. The
distinction is keyed on `local_agent` specifically. Backed by
fixtures
[`local_agent_outlives.ndjson`](../references/fixtures/claude/local_agent_outlives.ndjson),
[`local_agent_user_input_during_wait.ndjson`](../references/fixtures/claude/local_agent_user_input_during_wait.ndjson),
[`local_agent_plus_bg_bash.ndjson`](../references/fixtures/claude/local_agent_plus_bg_bash.ndjson).

The gating rules are not heuristics: `parent_tool_use_id == null`
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

**Parent-content resume re-arm (Claude 2.1.154+).** The soft-close
above is correct when the parent has genuinely stopped (a subagent
outlives it, or the turn is truly done, with the trailing `result`
following within ~milliseconds). But Claude Code 2.1.154+ also emits a
parent `end_turn` at *intermediate* message boundaries: it splits one
logical turn (interleaved thinking + tool use) into multiple wire
messages and resumes the SAME turn with a fresh parent `message_start`
and no intervening `result` or `system.init`. The soft-close has
already fired `provider:turn_completed` (clearing the working
indicator + Stop button), so without a re-arm the indicator stays
dead for the rest of the turn while the agent is visibly still
streaming. This was the user-reported "working indicator vanished
mid-thinking, couldn't interrupt, but it kept going" bug; it did not
exist before 2.1.154 (verified: 0 occurrences across 25 days of
pre-2.1.154 wire logs, 14 the day 2.1.154 landed).

The fix is reactive, not predictive. At the `end_turn` the three
cases ("done", "parked waiting on subagent", "about to resume") are
byte-identical on the wire, so the only safe discriminator is what
happens *next*. When the FIRST **parent** (`parent_tool_use_id == ""`)
content block of a resumed segment arrives while the logical turn is
already settled and no wire round is open, triage re-opens the round
(`maybeReopenSettledRound`, the shared mechanism behind both this and
the `system.init` re-round) and re-emits `provider:turn_started`.

Two guards keep this invariant-27-safe:

- **parent-only**: subagent content (`parent_tool_use_id != ""`)
  never re-arms. In the `local_agent`-outlives case the parent IS
  done and only the subagent streams until the trailing `result`; the
  indicator correctly stays cleared through that wait. Verified
  against production 2.1.154+ wire: 14/14 real parent resumes
  re-armed, 0 subagent parks lit.
- **settled + no-open-round**: only the first parent block after a
  soft-close re-arms; subsequent blocks in the same resumed round are
  no-ops (no per-block blink), and an ordinary mid-round block start
  (round not yet settled) never fires.

Stuck-ON is structurally impossible: re-arm only ever *opens* a round;
the real `result` (reliably emitted, 36/36 turns on 2.1.154+) and
`CleanupThread`/the next send always terminate it.

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
- `multi_result_test.go` parent-content-resume re-arm coverage:
  `TestParentContentResumeReArmsAfterSoftClose` (two soft-close→resume
  cycles in one logical turn each re-emit `provider:turn_started`),
  `TestSubagentContentDoesNotReArmDuringSoftClose` (the parent-only
  guard: subagent content never re-arms),
  `TestParentContentDoesNotReArmMidRound` and
  `TestParentContentDoesNotReArmFreshSession` (the settled +
  no-open-round guards).

**See also.**
[`claude-wire.md §Soft round close`](../references/claude-wire.md#soft-round-close-message_deltastop_reason),
[`turn-lifecycle.md §Wire-round vs logical-turn cadence`](turn-lifecycle.md).

---

## 28. Claude resume-at must target a row claude's resume will keep

**Rule.** Any uuid Agent Overflow passes as `--resume-session-at` must
be a `user`/`assistant` row that (a) is reachable by walking
`parentUuid` back from the session file's last uuid-bearing transcript
row (the **active branch**, the chain claude itself reconstructs on
resume) AND (b) survives the CLI's resume deserialization filters
(unresolved client tool_uses, orphaned thinking-only rows,
whitespace-only rows + the user-run merge). And any session file AO
writes (fork, revert slice) must keep its writable tail on that branch.

**Rationale.** `--resume-session-at` is validated against the
**deserialized** active branch; a uuid the CLI rejects hard-fails
pre-init (`result{error_during_execution, errors:["No message found
with message.uuid of: ..."]}`) and the process lingers. Both halves
have bricked real threads, deterministically on every retry:

- Off-branch (2026-06-10 incident): deferred `system/api_error` rows
  are appended at the NEXT user send with a stale `parentUuid` that
  bypasses the prior turn's tail (upstream bug, 2.1.167–170); each
  retry re-forked the same poison.
- Filter-dropped (2026-08-03 incident): a Windows BSOD killed a
  34-minute Bash mid-run, leaving the transcript leaf an assistant
  `tool_use` row with no `tool_result`. The row is the branch tip, but
  the CLI drops it (and its thinking sibling) before validating the
  cursor. See
  [`claude-wire.md §resume deserialization filters`](../references/claude-wire.md#session-jsonl-resume-deserialization-filters-crash-tails).

**Enforcement, all three layers:**

- `sessionfork/rechain.go`: fork/slice output force-chains each
  deferred api_error row to its file predecessor (subtype-scoped;
  compact-boundary system rows are legitimate `parentUuid:null` roots
  and are never re-chained). User/assistant rows are never re-chained,
  because claude's own walk correctly ignores abandoned content branches.
- `sessionleaf_branch.go` + `sessionleaf_resumefilters.go`: the cold
  scan validates the file-order leaf against a branch index built in
  the same pass, runs the CLI's three resume filters over the active
  chain (conservative mirror: every blessed uuid survives the CLI's
  possibly-larger list), and repairs rejected picks to the deepest
  surviving row; no usable row → empty leaf → the spawn omits
  `--resume-session-at` entirely (claude's own default-leaf semantics,
  always safe).
- `resolveClaudeResumeAt` (app_session.go): explicit cursors (the
  live-tracker leaf from the context-repair restart) are validated via
  `claude.ResumeAtOnActiveBranch` before spawn, the same branch + filter
  screen, so a wire-derived leaf whose tool_result never reached the
  file (process died mid-tool) is rejected; rejected cursors fall
  back to the file scan, loudly.

The row-admission set for the branch walk
(`sessionfork.TranscriptTypes`: user/assistant/attachment/system/
progress, sidechains excluded) is spike-verified per type against
2.1.170 and is shared by the fork transform and the branch validator
(one exported set, no copies to drift). See
[`claude-wire.md §active-branch semantics`](../references/claude-wire.md#session-jsonl-active-branch-semantics---resume----resume-session-at).
Fixture:
[`session_api_error_offbranch.jsonl`](../references/fixtures/claude/session_api_error_offbranch.jsonl).

**Test.** `sessionfork/rechain_test.go` (re-chain topology, compact
boundaries, idempotence on fork-of-fork);
`sessionleaf_branch_test.go` (off-branch repair, sidechains, broken
chains, `TestResumeAtOnActiveBranch`);
`sessionleaf_resumefilters_test.go` (crash-dangling-tool_use tail,
orphaned thinking, whitespace tails + user-run merge, explicit-cursor
rejection); `app_session_resumeat_test.go`
(spawn-time rejection + scan fallback);
`TestE2E_ClaudeQueuedFlushRepairsRiskyAdvisorContextBeforeSend`
(explicit cursor surviving validation end-to-end).

---

## 29. Stopped-thread event routing is host-controlled

**Rule.** The triage stopped-thread marker is set by
`CleanupThread` (StopSession / revert / thread delete) and cleared
**only** by the host's session-start funnel
(`startSessionNowWithClaudeResumeAt` → `triage.MarkThreadActive`,
pre-spawn). No wire event clears it, not even `EventInit`.
Host-synthesized events (send-failure synthetic turn-completes,
`emitErrorToThread`) route through `triage.HandleSynthetic`, which
bypasses the gate; wire events from a stopped thread's prior session
stay dropped (Bug B5 semantics), including errors a wire event
*triggers* on the read loop, which use `emitWireErrorToThread`.

**Rationale.** The original design cleared the marker on wire
`EventInit`, proof-of-life from the replacement session. But a
replacement session that dies during startup (e.g. an unusable
`--resume-session-at` cursor, invariant 28) emits its only diagnostics,
the pre-init error `result`, and never inits. With the
wire-controlled clear, that error was dropped by the very gate meant
to silence the PREVIOUS session, the turn never settled, and the
thread hung on "Working" forever (2026-06-10 incident, link 4 of 5).

Clearing pre-spawn is safe against the stale-frame interleaving B5
guards: both providers' `Close()` blocks on read-loop drain
(`<-readDone`), so no old-session frame can arrive after
`stopExistingSessionLocked` returns. It must be pre-spawn because
Codex emits `EventInit` synchronously inside `NewSession` and a DOA
Claude process emits its tail before the session registry sees it.
`MarkThreadActive` also resets the thread's settled-turns ledger (the
repair-restart path skips `CleanupThread`, and a stale settlement
marker would misroute a replacement session's orphan error into the
late-fold path) and bumps the thread's reactivation **epoch**:
asynchronous teardowns (`teardownDeadPreInitSession`) capture the
epoch before unregistering and run their cleanup via
`CleanupThreadIfEpoch`, so a teardown that loses the race against a
user retry's session start cannot re-stop the live thread or sweep
its state. The registry token guard alone can't cover that window
because the retry's spawn runs for seconds between `MarkThreadActive`
and re-registration.

Corollary: a wire error `result` with no open round and no open turn
persists an orphan error item (attributed to the pending-send head
when one exists) and suppresses the queued-send flush; a pre-init
error result additionally reaps the dead session
(`teardownDeadPreInitSession`, Claude-only, queued sends restored to
the composer draft before the sweep). There is deliberately NO init
watchdog/timer. Pre-init process exit already surfaces via the read
loop, and liveness probing is an explicit non-goal
([`turn-lifecycle.md`](turn-lifecycle.md)).

**Test.** `router_test.go`
(`TestEventInitDoesNotClearStoppedThread`,
`TestMarkThreadActiveReadmitsPreInitEvents`,
`TestHandleSyntheticBypassesStoppedGate`,
`TestCleanupThreadIfEpochSkipsAfterReactivation`);
`turn_lifecycle_test.go` (orphan error attribution,
`TestMarkThreadActiveResetsSettlementLedger`);
`app_errors_test.go` (`TestEmitWireErrorToThreadRespectsStoppedGate`);
`app_flush_queue_test.go`
(`TestPreInitTeardownRestoresQueuedSendsToDraft`); capstone
`TestE2E_ClaudeStoppedThreadPreInitErrorResultSurfaces`.

---

## 30. The workflow engine's command goroutine is the only mutator, and `teardown` the only release path

**Rule.** Every workflow FSM transition and every mutation of the
engine's scheduler state (`items`, `holders`, `waitingKeys`,
`inflightStarts`) happens on the single goroutine `Engine.loop()` owns
(`internal/workflow/engine/engine.go`). Runner callbacks, bound RPC
methods, the scheduler, and the startup sweep all enqueue commands and
wait for a reply; none touch the maps. `teardown` (`fsm.go`) is the
only function that releases a resource holder and the only caller of
`Runner.Stop`, with `teardownUnit` (`unit_outcomes.go`) as the
per-unit half of the same contract. Normal phase exit, gate park,
validation exhaustion, failure, cancel, pause, takeover, discard, and
crash sweep are *triggers* of that one path, never alternatives to it.

**Rationale.** This is the workflows system's version of invariant 8
(one writer per thread). Resource capacity is a project-local
semaphore with no timeout: a second release site that forgets one
holder does not crash, it silently lowers the project's capacity for
the rest of the process lifetime, and the symptom ("phases stopped
starting") points nowhere near the code that leaked. Serializing on
one goroutine is also what makes a child run's terminal transition
safe to re-enter its parent's call phase: `transition` pushes the
parent's settle onto `e.deferred` instead of recursing, so the child's
teardown finishes before the parent's phase completes.

**Enforcement.** The engine exposes no state accessor that returns a
live map; `Paused` and friends round-trip through the command channel.
`internal/workflow/scheduler/` never imports the engine. It imports
`store` and `workflow/def` only, and takes one start callback from the
app, so an automation firing cannot reach into FSM state; the same
holds for `internal/workflow/runner/`, which is pure helpers. Every
new exit path must route through `teardown`; adding a `releaseResources`
call anywhere else is the defect.

**Test.** `TestSemaphoreReleaseOnEveryImplementedExitPath` and
`TestSemaphoreReleaseWhenRunnerStartFailsAfterAcquisition`
(`engine/semaphores_test.go`); `TestFSMTransitionTableIsClosed` and
`TestRunnerStartupDoesNotBlockEngineCancellation`
(`engine/engine_test.go`); `engine/rebuild_test.go` for the crash-sweep
trigger; `engine/unit_actions_test.go` for the per-unit half.

---

## 31. A parked workflow attempt keeps its provider session

**Rule.** Pausing or interrupting a workflow attempt tears the attempt
down without killing the provider process. `workflowhost.Runner.Stop`
(`internal/workflowhost/runner.go`) interrupts the turn for an agent
attempt (never `StopSession`, never `CleanupThread`), so the CLI
process, its session file, and the thread's history survive the park.
Resume carries the parked attempt's thread through `ContinueThread`
(`engine/start.go`, `engine/units.go`) and the runner sends only the recovery
delta when that provider context is available. The runner proves cold context
availability before sending. If it has disappeared, the engine supersedes the
unsent continuation and reconstructs the same round on a new thread with the
full original prompt and an explicit context-loss note.
Warm loop reuse goes through the same proof and engine-owned fallback: its
replacement remains a new logical round with a full prompt, but the unavailable
reuse attempt is superseded and the cold reconstruction records the degradation
instead of the runner silently substituting a thread.
`paused` and `interrupted` are distinct typed reasons that resume
identically. A tool attempt has no turn to interrupt: teardown kills
its process group, because a command is re-run from the start.

**Rationale.** Core principle 2: the provider process is the source of
truth during a turn. A phase that resumes into a new session loses
everything the model established before the pause and pays the context
cost again; worse, the resumed run would silently produce different
work than the paused one. The asymmetry between agent and tool
attempts is not an inconsistency: an agent attempt's value is the
accumulated session, a tool attempt's is its exit status.

**Enforcement.** `Stop` is reachable only from `teardown`
(invariant 30), so there is one place this contract can be broken.
`Resume` refuses reasons that are not a continuation, and a parked
attempt with no usable provider context reconstructs the round *loudly* rather
than pretending to continue. Thread existence is only eligibility; a live
process or the runner's provider-specific cold preflight is authoritative. The one place that *does* stop a
phase session is the D25 project-deletion cleanup
(`stopWorkflowTreeSessions`, `app_project_delete_cleanup.go`), and it
is not a park: the run has already been cancelled and the thread is
about to be deleted, so there is no session left to resume into.

**Test.** `engine/pause_test.go`:
`TestPauseParksRunReleasesResourcesAndStopsTheTurn`,
`TestResumeContinuesTheParkedProviderThread`,
`TestResumeInterruptedRunUsesTheSameContinuation`,
`TestResumeWithoutASessionStartsAFreshAttemptLoudly`,
`TestResumeRefusesReasonsThatAreNotAContinuation`, plus the fan-out
repair/continuation pair.

---

## 32. A resting run reaches a human through exactly one thread and one delivery path

**Rule.** Only a *root* run may be bound to a thread
(`work_items.origin_thread_id`), and the wake for a resting run is
delivered by `registerQueueItem` (`app_workflow_wake.go`), the same
queued-user-message path a human `SendMessage` uses. There is no second
delivery channel: an idle thread flushes the queued item immediately, a
busy thread queues it behind the running turn, and a deleted thread
falls back to the notification surface. A descendant that parks
surfaces at its root; once the root itself is resting, further
descendant parks are silent.

**Rationale.** Two delivery paths means two orderings, and a wake that
jumps a live turn either interleaves with the user's own message or is
dropped by the thread's one-writer rule (invariant 8). Riding the
queue makes "the run woke you up" indistinguishable from "someone sent
you a message" everywhere downstream (persistence, replay, steer,
undo), so no consumer needs a workflow-shaped special case. Binding
only roots is what keeps the mapping one-to-one: a called run's news is
its parent's business, and the parent's root already has the thread.

**Enforcement.** Structural, in SQLite: migration v39 rebuilt
`work_items` with `CHECK(parent_item_id = '' OR origin_thread_id = '')`
(`internal/store/migrate.go`), so binding a child run is not a bug the
app can have. The insert fails. `WorkflowBindThread` refuses threads a
run cannot report into, and `WorkflowUnbindThread` refuses a called
run. The composed message itself is bounded and quoted per invariant 34.

**Test.** `TestMigrationV39AddsOriginThreadAndPausedReason`
(`internal/store/migrate_test.go`) for the CHECK;
`app_workflow_binding_test.go` for the bind/unbind refusals;
`app_workflow_wake_test.go`:
`TestWorkflowWakeDeliversToAnIdleBoundThread`,
`TestWorkflowWakeQueuesIntoABusyBoundThread`,
`TestWorkflowWakeFallsBackWhenTheBoundThreadIsGone`,
`TestWorkflowDescendantParkSurfacesAtTheRoot`,
`TestWorkflowDescendantParkIsSilentOnceTheRootRests`.

---

## 33. A scoped CLI token lives exactly as long as its session and can call exactly what its phase was granted

**Rule.** Three closed sets, all enforced outside the method bodies:

- **Lifetime.** `App.aoTokens` is mutated only by
  `registerAOTokenLocked` / `revokeAOTokenLocked`, called from the
  session-runtime mutations in `internal/sessionruntime.Manager` through
  `app_session_runtime.go`.
  Registration rides the session map, not the spawn path, so a token
  cannot outlive the process it was minted for.
- **Surface.** `transport.ScopedTokenMethods` is a closed allow-list
  mapping method name to the grants that admit it. Anything absent
  (every non-workflow RPC, every `LocalOnly` method outside the table)
  is `method_not_found` for a scoped token. A phase scope additionally
  needs one of the listed grants and gets the typed `grant_required`
  refusal naming what to add; an interactive scope may call everything
  listed, because a human approves each invocation.
- **Reach.** Every `ScopedTokenMethods` entry is also in
  `LocalOnlyMethods`, and `/rpc` refuses non-loopback peers with a 404
  and does not honour the server's own session token.

**Rationale.** The credential sits in the environment of a full-access
autonomous provider session, the one place in the app where a prompt
injection has hands. Its authority therefore has to be bounded by
construction rather than by what the CLI happens to send: a leaked
entry in the map is standing authority after the session is gone, and
a method reachable-but-ungated is the whole App surface one grep away.
Grants are *frozen at run start*, so widening a workflow's grants
cannot retroactively widen a session already in flight.

**Enforcement.** `ResolveScopedToken` is `//wails:ignore`. The method
that turns a token into authority is not itself callable with one.
Row-level scoping ("which runs may this phase act on") is deliberately
not expressed in the table; it depends on the run record and is
enforced by the bound methods from `CallerScopeFrom` in
`app_workflow_cli.go`.

**Test.** `internal/transport/scopedtoken_test.go`:
`TestScopedTokenMethodsNameOnlyKnownGrants` (grants exist in `def`'s
closed set), `TestScopedTokenMethodsAreLocalOnly` (the LAN-reach
pairing), `TestAuthorizeScopedMethodByKindAndGrant`,
`TestScopedRPCRouteAuthorizesAndRevokes`,
`TestWebviewTokenIsNotAScopedToken`; `app_ao_session_test.go` for
mint/register/revoke riding the session map.

---

## 34. Model-authored text embedded in a prompt is quoted through `internal/untrustedtext`

**Rule.** Any string that came from a model, a provider envelope, a
worktree, or a third party and is then embedded in a prompt we send to
another model goes through `untrustedtext.Field` (single-line values),
`untrustedtext.Quote` (bounded free text), or `untrustedtext.Truncate`
(whole blocks). One package, one rule: no local escaping helper, no
"this field is obviously safe" exception.

**Rationale.** The workflow triage seed, the wake message, and the PR
review composer all splice text one agent wrote into a prompt another
agent obeys. Without a shared rule, two prompts drift on what "this is
data, not an instruction" looks like, and the weaker one becomes the
injection path. The rune-bounded quoting also caps unbounded model
output before it reaches a context window, a correctness property as
well as a safety one.

**Enforcement.** The quoting is `strconv.QuoteToASCII` plus `<`, `>`,
`&` escaping, so the output is always a single visually-delimited token
regardless of what the source contained. Current call sites:
`internal/workflow/wake/compose.go`, `app_workflow_triage.go`,
`app_workflow_pr.go`. A new prompt that interpolates foreign text adds
a call site here rather than inventing a scheme.

**Test.** `internal/untrustedtext/untrustedtext_test.go` for the rule
itself; `internal/workflow/wake/compose_test.go` and
`app_workflow_triage_test.go` for the composed prompts staying quoted
and bounded.

---

## 35. Cancelling a workflow run never happens under a thread lock

**Rule.** No caller may hold `a.threadLocks().Lock(threadID)`, for any
thread, across `Engine.Cancel`, `WorkflowCancelItem`,
`discardWorkflowTree`, or anything else that can drive a run to
teardown. `DeleteProject` (`app_projects.go`) is the shape that forces
the rule: it locks every thread in the project, so its D25 workflow
cleanup runs **first**, before the first lock is taken
(`cleanUpProjectWorkflowWork`, `app_project_delete_cleanup.go`).

**Rationale.** Cancel is synchronous through the engine's command
goroutine (invariant 30), and teardown calls `Runner.Stop`, which
interrupts the turn via the runner's interrupt seam (invariant 31).
The interrupt takes the phase thread's action lock and holds it across
the provider's interrupt ack, so a caller already holding that lock is
asking the stop to wait on itself. Since the stop bound
(`workflowStopSendWait`) that wait is no longer an unbounded hard
deadlock (`Runner.Stop` abandons it at the bound and a background
goroutine fires the interrupt once the caller finally releases the
lock), but the rule stands unchanged. The violation still freezes the
engine's single command goroutine for the bound, still pushes the
interrupt to an arbitrarily later moment, and a bounded stall that
squeaks under most timeouts is harder to notice than the hang it
replaced, not safer. It only appears when a run happens to be live,
which is exactly the case a happy-path test misses.

**Enforcement.** The cleanup runs before the lock acquisition and the
locked section re-reads what it cleaned up, refusing with a retry
message if a cron fire changed the set underneath it. So "stop the
runs first" cannot be softened into "stop them wherever, then
re-check". Anything that stops a run as a *side effect* of a
thread-scoped operation belongs on the same side of the locks.

**Test.** `TestDeleteProjectCancelsLiveWorkflowRunBeforeTakingThreadLocks`
(`app_project_delete_live_run_test.go`) drives a live run on a provider
that acks an interrupt and then says nothing, and bounds the deletion
with a timeout so the reordered version fails loudly instead of hanging
the suite. The fixture pins `stopSendWait` far above that timeout,
because the stop bound would otherwise mask the reintroduced violation
as a survivable stall inside it.

---

## See Also

- [`chat-rewrite.md`](chat-rewrite.md): the spec these rules were
  distilled from.
- [`conventions.md`](conventions.md): softer contributor guardrails.
- [`how-to.md`](how-to.md): step-by-step recipes for common changes.
- [`adrs/`](adrs/): the decisions behind these rules.
