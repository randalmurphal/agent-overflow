# Chat Rewrite — Unified Item Stream

Status: spec, pre-execution.

## Why

The current chat data flow has five parallel state streams on the
frontend (`items`, `activeToolCalls`, `streamingContent`,
`backgroundTasks`, `pendingMessage`) reconciled against bulk
`ListItems` reloads at turn-complete. Items appear and disappear at
turn boundaries because half the streams arrive live and the other
half land in batch. This rewrite collapses to **one stream, one
listener, one list**.

## Non-goals

- Reframing the framework (Svelte stays).
- Rewriting providers, store schema, sidebar, composer shell, terminal,
  design mode, discussion mode, or settings.
- UI visual polish — that's a separate pass after the data model
  lands.

## The model

### Item

One row in `items`. Every visible thing in the chat is one of these.

```
id              stable uuid
thread_id       fk
turn_index      int   ordering bucket per turn
item_index      int   assigned on first upsert; immutable after
kind            enum (see below)
role            assistant | user | system
status          streaming | running | completed | errored | declined
                (semantics depend on kind; see table)
summary         TEXT — always-loaded body / preview
payload_id      optional fk to heavy payload
parent_id       optional fk to a tool_call (for subagent children)
is_background   bool
completion_of   optional fk to the tool_call this row completes
                (used only by kind=tool_completion)
created_at      int64 ms
updated_at      int64 ms
```

### Kinds (closed set — 7)

| kind              | role      | status semantics                              | notes                                                                                  |
|-------------------|-----------|------------------------------------------------|----------------------------------------------------------------------------------------|
| `user_text`       | user      | always `completed`                             | user message; appears at top of its turn                                               |
| `assistant_text`  | assistant | `streaming` while growing → `completed` at turn end | one per turn (or one per agent output segment); `summary` grows via deltas             |
| `thinking`        | assistant | `streaming` while growing → `completed`        | one per thinking block; `summary` grows                                                |
| `tool_call`       | assistant | `running` → `completed` / `errored` / `declined` | inline tools mutate in place; backgrounded tools stay `running` and append a partner   |
| `tool_completion` | assistant | `completed` / `errored`                        | only exists for backgrounded tool calls; carries `completion_of` and the result payload |
| `error`           | system    | always `completed`                             | turn-level error surfaced inline (provider crash, refusal, etc.)                        |
| `compaction`      | system    | always `completed`                             | marker — context was compacted at this point                                           |

Kinds NOT in this list: anything else. Diff cards / command output / proposed plans are
**not separate item kinds** — they are tool_call items whose payload renders specially
(see Heavy Payloads).

### Streaming

Streaming items (`assistant_text`, `thinking`) grow `summary` directly.
No accumulator in Go memory. No drain at turn-complete. Every delta
upserts the item with the appended summary. SQLite WAL absorbs the
write rate; if profiling shows a bottleneck, debounce later.

If `summary` exceeds 50KB, promote to a payload (write payload, set
`payload_id`, truncate `summary` to first 1KB as preview). Frontend
fetches the rest on expand.

### Subagent nesting (visual)

Items emitted by a subagent (Claude Task, Codex collab) carry
`parent_id` = the parent tool_call's id. Renderer:

- Top-level loop walks items where `parent_id == ""`.
- Each `tool_call` item that has children renders as a card containing
  its children, rendered recursively.
- Children are NOT also rendered at the top level.
- Whole subagent collapsible.

### Tool call lifecycle

- Inline tool call: one `tool_call` row. `running` → `completed`.
  `summary` carries tool name + input preview, then transitions to
  include exit code / brief result on completion. Result payload
  attached to the row.
- Background tool call: `tool_call` row stays `running`,
  `is_background=true`. When the tool actually finishes, append a
  separate `tool_completion` row at the chronological position of
  completion (`turn_index = LastTurnIndex`), with `completion_of` =
  launch.id, status = `completed`/`errored`, and payload.

### Background tray

Pure derivation over `items`:
- Show items where `is_background == true && status == running`.
- Pair with their `tool_completion` partner if present.
- Keep a row visible for 2s after its completion lands, then remove.
- Tray is a mirror — the launch and completion rows ALSO render inline
  in the main timeline at their chronological positions. Tray is not a
  relocation, it's a duplicate view for "what's running."

### Approvals

Approvals do NOT become items. They are pure overlay state.

The provider emits an approval request → frontend stores it in
`pendingApprovals`. UI shows an overlay (modal, inline banner, plan
card, etc.) appropriate to the kind. User responds → frontend calls
the resolution binding → backend resolves → provider emits resolved.

The underlying tool_call that triggered the approval is a normal
`tool_call` item in the timeline; its summary tells the user what was
asked. The approval interaction itself leaves no persisted trace
beyond the tool_call's status.

AskUser-style tools (an explicit "ask the user a question" tool) ARE
just `tool_call` items — the question is in the summary, the answer
becomes the tool result.

### Heavy payloads

`payloads` table unchanged. Items reference payloads by `payload_id`.

- No client-side payload cache. Every dropdown expand calls
  `GetPayloadData(payloadId)`. Collapse drops the in-memory body.
- Always-loaded `payloads.meta` is folded INTO the item (we stop
  emitting `provider:meta` as a separate channel; the item's `summary`
  carries enough preview info, payload header lives in the item or in
  the payload row itself fetched at expand time).
- Renderer dispatches by `payloads.kind` when the payload is fetched:
  `command_output` → terminal-style scrollable, `diff` → DiffPreview,
  `text` → preformatted, etc. The dispatcher is one switch in the
  ToolCallCard component.

### Errors and compactions

- Error: `kind=error` item with `summary` = the error message. Renders
  as a red banner-style row in the timeline.
- Compaction: `kind=compaction` item with `summary` = e.g. "Context
  compacted — older messages summarized". Renders as a horizontal
  divider with a label.

### Removed entirely

- Session status. No `connecting` / `connected` / `disconnected` /
  `reconnecting` UI. The agent is either producing items right now or
  it isn't. The "is producing" signal is derived from
  `items.some(i => i.status === 'streaming' || i.status === 'running')`.
- Token usage, context window, rate limits — not displayed. Drop the
  events from the frontend entirely. Backend may still capture them
  internally for cost accounting if needed, but no UI.
- The `provider:meta` channel.
- The `provider:event` channel for everything except status + approvals
  (see channels below).

## Channels

The frontend listens to exactly **two** Wails channels for chat state:

| channel                  | payload                              | purpose                                                |
|--------------------------|--------------------------------------|--------------------------------------------------------|
| `provider:item_upsert`   | full `Item` (with payload metadata)  | every timeline state change                            |
| `provider:approval`      | `{ action: 'request' \| 'resolve', request }` | approval overlay state                                  |

Plus app-shell channels that already exist (`design:*`,
`thread:mode_changed`, etc.) — those are unrelated to the chat data
flow and stay as-is.

Errors that are NOT in-chat (provider crash on startup, fatal
storage failure) surface as toasts via the existing toast store, not
through these channels.

## Backend chokepoint

`internal/triage/router.go` collapses to one persistence function:

```go
// persistItem is the single chokepoint for any timeline state change.
// All event handlers build an Item (and optional Payload) and call
// this. The store does the upsert (insert if id is new, update in
// place if id exists, preserving item_index). On success we emit the
// canonical item to the frontend.
func (r *Router) persistItem(item store.Item, payload *store.Payload) error {
    persisted, err := r.store.UpsertItem(item, payload)
    if err != nil { return err }
    r.emit("provider:item_upsert", persisted)
    return nil
}
```

The existing `persistTurnText`, `persistHeavy`, `replaceHeavy`,
`insertHeavyItem`, `insertHeavyItemAndPayload`,
`persistFileChangeToolResult`, `persistCommandInlineDiffToolResult`,
`persistToolResult`, `persistToolCallLaunch`,
`persistToolCallCompletion`, `upgradeSummaryOnlyToolResults` — all
deleted. Their work folds into per-event handlers that build an Item
and call `persistItem`.

### Per-event handler logic

| event                       | handler                                                                                               |
|-----------------------------|-------------------------------------------------------------------------------------------------------|
| `EventTextDelta`            | upsert `assistant_text` item; id = `"text:<turnId>"`; append delta to summary; status=streaming       |
| `EventThinking`             | upsert `thinking` item; id = `"think:<turnId>:<thinkingBlockIndex>"`; append delta to summary         |
| `EventToolStart`            | upsert `tool_call`; id = evt.ItemID; status=running; is_background from classifier                    |
| `EventToolComplete` inline  | upsert same `tool_call`; status=completed/errored; attach result payload                              |
| `EventToolComplete` bg      | upsert NEW `tool_completion`; completion_of=launch.id; turn_index=LastTurnIndex; attach result payload |
| `EventDiff`                 | find/create the related `tool_call`, attach diff as its payload                                       |
| `EventCommandOutput`        | find/create the related `tool_call`, attach command output as its payload                             |
| `EventProposedPlan`         | upsert a `tool_call` with kind-of-tool="plan"; payload carries the plan markdown                      |
| `EventError`                | upsert `error` item; id = uuid; summary = error message                                               |
| `EventCompactBoundary`      | upsert `compaction` item; id = uuid; summary = compaction note                                         |
| `EventTurnStart`            | open turn span (telemetry); capture checkpoint; no item                                               |
| `EventTurnComplete`         | flip any status=streaming items in this turn to status=completed; close span; no other work           |
| `EventApprovalRequest`      | emit `provider:approval` `{action:request, request}` (not an item)                                    |
| `EventApprovalResolved`     | emit `provider:approval` `{action:resolve, requestId}` (not an item)                                  |
| `EventInit`                 | persist session_ref to thread; no item                                                                |
| `EventThreadRenamed`        | persist title to thread; emit existing `thread:*` event; no item                                      |
| `EventModelRerouted`        | persist model to thread; emit existing event; no item                                                 |
| `EventTokenUsage`           | dropped (no UI for it)                                                                                |
| `EventRateLimits`           | dropped                                                                                               |
| `EventSessionStatus`        | dropped — derived from items                                                                          |
| `EventBackgroundStart/Delta/Complete` | dropped — superseded by tool_call lifecycle with is_background classifier                    |
| `EventToolProgress`         | dropped — progress is just successive upserts of the tool_call's summary                              |
| `EventPlanUpdate`           | dropped — final plan arrives via EventProposedPlan                                                    |

The `provider.AllEventKinds` list shrinks accordingly.

### Store changes

Add `UpsertItem(item Item, payload *Payload) (Item, error)`:

- If `item.ID` exists: update kind/role/summary/payload_id/status/
  is_background/completion_of/parent_id/updated_at. Item index is
  preserved.
- Else: assign next item_index for `(thread_id, turn_index)`, insert.
- If payload non-nil, upsert payload in same transaction.
- Returns the canonical persisted Item (with assigned item_index).

The single-writer connection pool (`SetMaxOpenConns(1)`) keeps the
upsert race-free. No additional schema migration — v14 columns suffice.

`InsertItem`, `AppendItem`, `AppendItemWithPayload`,
`UpdateItemPayload`, `UpdateItemStatus`, `UpdateItemBackgroundFlag`,
`AppendCompletionItem`, `UpsertTurnPayload` — all callable in the new
chokepoint, but consider folding `UpdateItemStatus` /
`AppendCompletionItem` into `UpsertItem` semantics. Probably keep
`AppendCompletionItem` because it has a distinct invariant
(`completion_of` always set, item always background).

## Frontend collapse

### Pane state (final shape)

```ts
items: Item[]                    // sorted by (turn_index, item_index)
pendingApprovals: ApprovalRequest[]
sessionStatus: 'idle'            // kept only for legacy callers; no UI
thread: Thread | null
loading: boolean
// design-mode + discussion-mode + diff-panel + terminal state stay as-is
```

**Deleted:** `activeToolCalls`, `streamingContent`,
`backgroundTasks`, `pendingPlanUpdate`, `pendingMessage`,
`dismissedPlanItemId`, `payloadMetas`, `tokenUsage`, `contextWindow`,
`rateLimits`, `sessionApprovedTools`, `error`. Everything that was a
parallel state stream goes away. Some of these (sessionApprovedTools,
error) may stay for non-chat purposes — audit each.

**One mutation:** `upsertItem(item: Item)`. Replaces by id (preserves
position) or inserts at sorted position. That's the only timeline
mutation.

**Approval mutations:** `addApproval(req)`, `removeApproval(id)`. Pure
overlay state.

### Listener

```ts
on('provider:item_upsert', (item) => pane.upsertItem(item));
on('provider:approval', (msg) => {
  if (msg.action === 'request') pane.addApproval(msg.request);
  else pane.removeApproval(msg.requestId);
});
```

Plus existing app-shell channels (design, thread mode, etc.) untouched.

### isTurnActive

```ts
get isTurnActive() {
  return items.some(i =>
    (i.kind === 'assistant_text' || i.kind === 'thinking') && i.status === 'streaming'
    || i.kind === 'tool_call' && i.status === 'running'
  );
}
```

Used by Composer to gate sends + show interrupt button. Same call
sites; just different derivation.

### finalizeTurn / switchThread

`finalizeTurn` deleted. The stream is authoritative; nothing to drain.

`switchThread` keeps its initial hydration via `ListItems`. After that
the upsert stream is the only mutation source.

### MessageTimeline

```svelte
{#each topLevelItems as item (item.id)}
  {#if item.kind === 'user_text'}<UserMessage {item} />
  {:else if item.kind === 'assistant_text'}<AssistantText {item} />
  {:else if item.kind === 'thinking'}<Thinking {item} />
  {:else if item.kind === 'tool_call'}
    <ToolCallCard {item} children={childrenOf(item.id)} />
  {:else if item.kind === 'tool_completion'}<ToolCompletion {item} />
  {:else if item.kind === 'error'}<ErrorRow {item} />
  {:else if item.kind === 'compaction'}<CompactionMarker {item} />
  {/if}
{/each}
```

Where:
- `topLevelItems = items.filter(i => !i.parentId)`
- `childrenOf(id) = items.filter(i => i.parentId === id)`
- `ToolCallCard` recursively renders its children with the same switch.

The ChangedFilesTree and TurnDiffBadge logic from the current
MessageTimeline survives unchanged — they're pure derivations over
items by turn.

### Background tray

Renders items where `is_background && status == 'running'`, plus
pairs whose `tool_completion` is younger than 2s. Pure derivation
over `pane.items`. Already implemented in `BackgroundTaskTray.svelte`;
filtering logic stays.

### Heavy payload UI

`ToolCallCard` shows the tool header (name, summary, status, exit
code) always. Click chevron → call `GetPayloadData(payloadId)` →
render based on payload kind:
- `command_output` → preformatted terminal block
- `diff` → DiffPreview
- `tool_result` → existing structured render
- `proposed_plan` → ProposedPlanCard
- `text` / unknown → preformatted

No client-side cache. Collapse → drop the body. Re-expand re-fetches.

## Crash recovery

Falls out of the design:

1. Every event upserts to SQLite **before** emitting. SQLite always
   has the latest persisted state.
2. On app restart: `ListItems(threadId)` returns everything that was
   persisted, in order.
3. Provider session is restored separately by `session_ref` (existing
   plumbing). Resumption is a no-op for the data model.
4. If items were left with `status=streaming` or `running`:
   - On resume, mark them `interrupted` if the provider session can't
     be re-opened. (`status=errored` with summary = "Interrupted".)
   - Otherwise let the resumed session continue delta-ing. Each new
     delta upserts the same item; status flips to completed at turn
     end as normal.
5. Backgrounded tool calls that were in-flight at crash: the launch
   row is in SQLite. The `tool_completion` row will arrive when the
   provider session resumes and reports completion. If the session is
   gone, the launch row sits as `running` indefinitely — user sees it
   in the tray as "interrupted" (renderer treats `running` items
   without a live session as such).

The recovery contract: **what's in SQLite is what the user sees**.
Provider session state may be ahead, behind, or equal — none of those
break the UI.

## Demolition list (in execution order)

### Backend deletions

- `internal/triage/tool_lifecycle.go` (current implementation, wrong shape)
- `internal/triage/tool_lifecycle_test.go`
- `internal/triage/tool_result.go` (folds into payload-on-tool_call render)
- `internal/triage/command_inline_diff_persist.go` (same)
- `internal/triage/router.go`: `handleTextDelta` accumulator, `handleThinking`
  accumulator branch, `handleTurnComplete` drain logic, `textAccumulators`,
  `reasoningAccumulators`, `pendingCommandDiffs`, all the per-kind persist
  helpers
- `internal/provider/types.go`: `EventBackgroundStart/Delta/Complete`,
  `EventToolProgress`, `EventTokenUsage`, `EventRateLimits`,
  `EventSessionStatus`, `EventCompactBoundary` keep, `EventPlanUpdate` drop,
  `ItemBackgroundStarted`, `ItemBackgroundDone`, `ItemToolResult`,
  `ItemDiff`, `ItemCommandExecution`, `ItemProposedPlan`, `ItemThinking`
  (collapse to the 7-kind enum)
- Anything in `provider.AllEventKinds` not in the per-event handler table

### Frontend deletions

- `pane.activeToolCalls`, `addToolCall`, `completeToolCall`,
  `updateToolProgress`
- `pane.streamingContent`, `appendTextDelta`
- `pane.backgroundTasks`, `addBackgroundTask`, `completeBackgroundTask`
- `pane.pendingMessage`, `setPendingMessage`
- `pane.pendingPlanUpdate`, `setPendingPlanUpdate`,
  `clearPendingPlanUpdate`
- `pane.dismissedPlanItemId`, `setDismissedPlanItemId`
- `pane.payloadMetas`, `addPayloadMeta`, `touchPayloadMeta`
- `pane.tokenUsage`, `setTokenUsage`
- `pane.contextWindow`, `setContextWindow`
- `pane.rateLimits`, `setRateLimits`
- `pane.error`, `setError`, `clearError` (audit — may be used outside
  chat data flow)
- `pane.finalizeTurn`
- `frontend/src/lib/components/chat/StreamingMessage.svelte`
- `frontend/src/lib/components/chat/ContextWindowMeter.svelte`
- `frontend/src/lib/components/chat/RateLimitsMeter.svelte`
- `frontend/src/lib/components/chat/ProviderStatusBanner.svelte` (audit
  — may move to toast)
- `frontend/src/lib/components/chat/ChangedFilesTree.svelte` (KEEP — pure
  derivation, useful)
- `frontend/src/lib/components/chat/TurnDiffBadge.svelte` (KEEP)
- `frontend/src/lib/components/chat/CommandOutput.svelte`,
  `DiffPreview.svelte`, `ProposedPlanCard.svelte`,
  `ThinkingBlock.svelte`, `ToolResultCard.svelte` — fold into
  `ToolCallCard`'s payload renderer (one component per payload kind
  surviving)
- `frontend/src/lib/stores/events.ts`: the giant switch over `evt.kind`
  in `routeEventToPane` collapses to two listeners (item_upsert + approval)

### Frontend keeps

- `BackgroundTaskTray.svelte` (filter logic stays)
- `ToolResultDropdown.svelte` (becomes the payload renderer)
- `MessageTimeline.svelte` (rewritten body, same shell)
- `UserMessage.svelte`, `AssistantMessage.svelte` (rename to
  AssistantText if needed)
- `SubagentGroup.svelte` (becomes part of ToolCallCard)
- All composer components, sidebar, terminal drawer, design view,
  discussion view — untouched

## Execution plan

One sequential pass. Each step ends with green tests.

1. **`store.UpsertItem`** + tests. Delete `UpdateItemStatus`,
   `UpdateItemBackgroundFlag` (fold semantics into UpsertItem).
   Keep `AppendCompletionItem` (distinct invariant).
2. **`router.persistItem` chokepoint** — new function, not yet called.
3. **Migrate handlers one at a time**, in this order:
   - `EventTextDelta` (replaces accumulator with streaming item)
   - `EventThinking` (same)
   - `EventToolStart` / `EventToolComplete` (replace tool_lifecycle.go)
   - `EventDiff`, `EventCommandOutput`, `EventProposedPlan` (attach to
     tool_call payload)
   - `EventError`, `EventCompactBoundary` (new item kinds)
   - `EventApprovalRequest` / `EventApprovalResolved` (new
     `provider:approval` channel)
   After each migration, delete the old persist function and any
   dead state. Tests adjusted in the same step.
4. **Delete `handleTurnComplete` drain**. Replace with status-flip
   pass over streaming items in the turn.
5. **Delete obsolete event kinds** from `provider.AllEventKinds` and
   provider adapters that emit them. Update adapters to map their
   wire events into the surviving kinds only.
6. **Frontend pane rewrite** — single PR-size chunk. Delete the state
   slices listed above. Implement `upsertItem` semantics and the new
   listener wiring. Update `isTurnActive`. Delete `finalizeTurn`.
7. **MessageTimeline rewrite** — new switch. Delete dead components.
   Wire `BackgroundTaskTray` over the new derivation.
8. **Tests** — integration test for the full flow: provider events
   in, single upsert stream out, frontend renders correctly with no
   shifts at turn boundary, crash recovery (kill mid-turn, reopen,
   verify items intact).

## Acceptance criteria

- During a turn, every observable state change appears in the timeline
  at its chronological position. Nothing is invisible until turn-complete.
- At turn-complete, the timeline does NOT shift, reorder, or surprise.
  Items already on screen stay where they are; new items append.
- Killing the app mid-turn and reopening shows exactly what was
  persisted at kill time. Items in `streaming`/`running` state on
  reopen are visibly marked as such; if the session is unrecoverable,
  they transition to `errored` with summary "Interrupted".
- Backgrounded tool calls show one launch row inline + one completion
  row at completion time + tray mirror while running.
- Subagents render nested under their parent tool call, collapsible.
- No `pane.activeToolCalls`, no `pane.streamingContent`, no
  `pendingMessage`, no `payloadMetas` map. Grep returns zero hits.
- Frontend `npm run check` clean. `go test ./...` green.
- A single end-to-end integration test covers: send message → tool
  call → tool result → assistant text → turn complete → reload thread
  → verify identical render.

## What this does NOT cover

- Visual polish toward forge's chat density. Separate pass after this
  lands. The data model is the prerequisite.
- The 1GB memory issue. Separate investigation; suspects are
  eager-loaded syntax highlighter languages and Wails webview baseline.
  Won't be fixed by this rewrite (or made worse).
- Workflow / phase / gate system, remote/web access, auto-updater,
  mid-turn correction — already-deferred items in `AGENTS.md`. This
  rewrite doesn't touch them.
