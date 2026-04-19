# Chat Rewrite — Unified Item Stream

Status: spec, pre-execution. v2 — incorporates review findings from
t3-code deep-dive, multi-reference cross-check (Claude Code CLI, Codex
CLI, CodexMonitor, Continue, Aider), and 25-scenario UX stress-test.

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
- Rewriting providers, store schema (other than additive), sidebar,
  composer shell, terminal, design mode, discussion mode, or settings.
- UI visual polish — that's a separate pass after the data model
  lands.

## The model

### Item

One row in `items`. Every visible thing in the chat is one of these.

```
id              stable uuid
thread_id       fk
turn_index      int    ordering bucket per turn
item_index      int    assigned on first upsert; immutable after
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
tool_name       optional string (for kind=tool_call: bash/edit/read/etc.)
                drives the per-kind header dispatch in the renderer
decision        optional enum (for kind=tool_call backed by an approval):
                approved | declined | amended | timeout
                renders as a small chip on the row; see Approvals
created_at      int64 ms
updated_at      int64 ms
```

### Kinds (closed set — 7)

| kind              | role      | status semantics                              | notes                                                                                  |
|-------------------|-----------|------------------------------------------------|----------------------------------------------------------------------------------------|
| `user_text`       | user      | always `completed`                             | user message; appears at top of its turn                                               |
| `assistant_text`  | assistant | `streaming` while growing → `completed` at segment end | one per **output segment** (see segmentation rule below)                            |
| `thinking`        | assistant | `streaming` while growing → `completed`        | one per thinking block; default-collapsed when `completed`, expanded while `streaming`   |
| `tool_call`       | assistant | `running` → `completed` / `errored` / `declined` | inline tools mutate in place; backgrounded tools stay `running` and append a partner   |
| `tool_completion` | assistant | `completed` / `errored`                        | only exists for backgrounded tool calls; carries `completion_of`, the result payload, and a summary that **restates the original command** |
| `error`           | system    | always `completed`                             | turn-level error surfaced inline (provider crash, refusal, etc.)                        |
| `compaction`      | system    | always `completed`                             | marker — context was compacted at this point                                           |

Kinds NOT in this list: anything else. Diffs / command output / proposed
plans are **not separate item kinds** — they are tool_call items whose
payload renders specially (see Heavy Payloads).

### Status / streaming / settle

For `assistant_text` and `thinking`:
- `streaming` while content is growing
- `completed` at segment end (text) or block end (thinking)

For `tool_call`:
- `running` from EventToolStart until EventToolComplete
- `completed` if completed cleanly OR if the underlying provider reports success
  even with a non-zero exit code (`grep` returning 1 for "no match" is normal
  — that does **not** flip to errored)
- `errored` only when `meta.is_error == true` (provider-reported failure)
- `declined` when an approval was denied

The non-zero exit code is preserved in the `summary` (e.g. `"Bash: grep foo  (exit 1)"`)
but does NOT change status. This is a deliberate decoupling — many shell tools
use exit codes for flow control, not failure.

### Text segmentation around tool_use

Claude can emit `text → tool_use → text → tool_use → text` inside a
single assistant message. The reading order matches the model's
reasoning order. The spec MUST preserve that order, so:

- One `assistant_text` per **output segment**, NOT per turn.
- Item id format: `text:<turnId>:<segmentIndex>` where segmentIndex
  starts at 0 for the first text in a turn and increments on every
  intervening tool_use block.
- Each segment lifecycle: `streaming` → `completed` at the next
  `tool_use` boundary or at turn-complete.

Without this, two text chunks bracketing a tool_call would fuse into
one item that renders BEFORE the tool — wrong.

### Streaming text body handling

Streaming items (`assistant_text`, `thinking`) grow `summary` directly.
No accumulator in Go memory. No drain at turn-complete. Every delta
upserts the item with the appended summary.

**Promotion to payload:** ONLY at segment-complete (status flip from
streaming → completed). If the final summary exceeds 50KB at that
moment, write it to a payload and replace summary with the first 4KB
as a tail preview. Frontend renders summary by default, expand fetches
the full payload.

This avoids the visual ejection that would happen if we promoted
mid-stream — the user keeps reading uninterrupted; the cap only kicks
in once the segment settles.

### Subagent visual nesting

Items emitted by a subagent (Claude Task, Codex collab) carry
`parent_id` = the parent tool_call's id. Renderer:

- Top-level loop walks items where `parent_id == ""`.
- Each `tool_call` item that has children renders as a card containing
  its children, rendered recursively.
- Children are NOT also rendered at the top level.
- **Default collapsed when status=completed; expanded while running.**
  Card header always shows: tool name + child count + `running` /
  `done (N s)` summary so collapsing doesn't hide live progress.
- **Visual nesting depth capped at 3.** Beyond depth 3, the deeper
  subagent renders as a "open subagent trace →" link that opens a
  side-panel detail view. Prevents visual implosion at unbounded
  recursion.
- Cycle guard: if `parent_id` would create a cycle, render flat at
  top level with a warning chip.

### Tool call lifecycle

Inline (foreground) tool call:
- One `tool_call` row, lives on the launch turn.
- `running` → `completed` / `errored` / `declined`.
- Summary carries: `tool_name + input preview` initially, then
  appends `(exit N)` or `(error)` on completion.
- Result payload attached to the same row.

Backgrounded tool call:
- `tool_call` row stays `running`, `is_background=true`.
- A separate `tool_completion` row is appended at the agent-perception
  ordering position (see below).
- `tool_completion.summary` **restates the original command** plus the
  outcome — e.g. `"npm install → exit 0 in 12s"`. Without this the
  late-arriving row is decontextualized.
- `tool_completion.completion_of` = launch.id; `is_background=true`;
  status = `completed` / `errored`.

### Background completion ordering — agent perception, not wall clock

When `EventToolComplete` arrives for a backgrounded tool, the
completion row does NOT land at wall-clock chronological position.
It lands at **the next point the agent would naturally observe the
completion** — i.e. the next safe boundary that doesn't interrupt the
agent's currently-streaming output.

Algorithm in the triage layer:

```
on EventToolComplete (background):
  1. Find any item in this thread with status=streaming.
  2. If none: upsert the tool_completion immediately at next item_index.
  3. If one exists: buffer the completion in a per-thread "deferred
     completions" queue. When the streaming item flips to completed
     (segment end, turn-complete), drain the queue and upsert each
     deferred completion at next item_index.
```

This matches the model's perspective: the agent only "sees" the
completion when it next runs and checks tool results. Visually, the
completion appears AFTER the assistant's current text/thinking
finishes, never mid-stream.

The launch row stays at its original position (turn N). The completion
can land in turn N (if it finishes within that turn's stream) or in
turn N+M (if the user has continued the conversation while it ran).
Either way, it lands at the post-streaming boundary.

### Stop control for backgrounded tools

Tray rows for `running && is_background` items expose a stop button.
On click:
- Call a backend Stop binding that signals the underlying process.
- Backend emits a synthetic EventToolComplete with `meta.is_error=true`,
  exit_code=-1, summary="Stopped by user".
- Renderer flips the launch row's status to `errored` and appends a
  `tool_completion` with status=errored, summary="Stopped by user".

If the app reopens with a `running && is_background` launch and no
matching completion, the backend probes session liveness:
- Process alive: keep `running`.
- Process dead/unknown: emit synthetic completion with
  summary="Interrupted — outcome unknown", status=errored. Tray row
  drops, timeline shows the launch as `errored` with completion partner.

### Background tray (frontend derivation)

Pure derivation over `pane.items`:
- Show items where `is_background == true && status == 'running'`.
- Pair with their `tool_completion` partner if present.
- Keep a row visible for 2s after its completion lands, then remove.
- **Cap visible rows at 3**, with a "+N more" stack collapsing older
  entries. Order newest-first.
- Tray is a mirror — launch and completion rows ALSO render inline
  in the main timeline at their (agent-perception) positions. Tray
  is a duplicate view for "what's running RIGHT NOW," NOT a
  relocation. We keep both because the inline rows preserve history;
  the tray gives an at-a-glance "what's the agent waiting on."

### Approvals

Approvals do NOT become standalone items. They are overlay state on
the frontend, persisted as a `decision` field on the underlying
`tool_call` item.

**Per-event behavior:**
- `EventApprovalRequest`: emit `provider:approval` `{action:request,
  request}`. Frontend stores it in `pendingApprovals` and renders an
  inline approval panel in the composer footer (matching t3-code's
  pattern). The underlying `tool_call` item already exists in the
  timeline at this point; its summary tells the user what was asked.
- `EventApprovalResolved`: emit `provider:approval` `{action:resolve,
  requestId, decision}`. Frontend removes from pendingApprovals.
  Backend updates the underlying tool_call's `decision` field via the
  upsert chokepoint.

**Decision values:**
- `approved`: user clicked Approve. Tool execution proceeds normally.
- `declined`: user clicked Deny. Tool_call status flips to `declined`.
- `amended`: user approved with modified input (Claude SDK supports
  this). The tool_call's summary updates to the modified input on the
  next upsert (when the tool actually starts with the new args).
- `timeout`: provider timed out the request. Treated as decline. Tool
  status flips to `declined`.

The decision renders as a small inline chip on the tool_call row
(`✔ approved`, `✗ declined`, `~ amended`, `⏱ timeout`). Scrolling
back you can see what was approved/declined when, without polluting
the timeline with extra items.

**AskUser-style tools** (an explicit "ask the user a question" tool)
are NORMAL `tool_call` items — the question is in the summary, the
answer becomes the tool result. Not an approval.

**Approval lost on reopen:** the `pendingApprovals` overlay is
volatile. On app restart, if a tool_call is still `running` and the
provider session can re-emit pending approvals, do that. If the
provider doesn't support re-emission, flip the tool_call to
`errored` with summary "Approval lost on restart — please retry"
and decision unset.

### Heavy payloads

`payloads` table unchanged. Items reference payloads by `payload_id`.

- **Bounded LRU cache on the frontend** — 20 entries max. Keeps
  expand/collapse/expand from re-fetching. Drops on thread switch.
- Always-loaded `summary` carries the preview. The full body lives in
  the payload, fetched on dropdown expand via `GetPayloadData`.
- Renderer dispatches by `payload.kind` when fetched:
  - `command_output` → terminal-style scrollable, ANSI preserved
  - `diff` → DiffPreview with file-tree (lazy per-file fetch — see
    large diff handling below)
  - `tool_result` → existing structured render (file_change schema)
  - `proposed_plan` → ProposedPlanCard
  - `text` / unknown → preformatted

**Large payload caps:**
- Initial fetch caps at 256KB head + 64KB tail of the payload data.
- For payloads larger than that, render a "Show full output (N MB) in
  terminal drawer →" link that opens the existing terminal drawer.
- Diffs with >10 files: render a file tree, fetch each file's diff
  on demand when expanded.

### Per-tool-kind header rendering

The `tool_call` item carries `tool_name`. The renderer maps it to a
header row with appropriate icon + label format:

- `Bash` → terminal icon + command preview
- `Edit` / `Write` / `MultiEdit` → file icon + path + change summary
  on completion
- `Read` → eye icon + path + line range
- `Grep` → search icon + pattern + path filter
- `WebFetch` / `WebSearch` → globe icon + URL/query
- `Task` → robot icon + subagent description (this is also a
  parent-of-children in the visual nesting)
- `Plan` → checklist icon + plan title
- `MCP/<tool>` → puzzle icon + tool name + argument summary
- unknown → generic tool icon + tool_name

This header dispatch is one switch in the ToolCallCard component.
Payload renderer (chevron-expand) is a SECOND switch on payload kind.

### Errors

Two channels, two surfaces:

- **In-chat errors** (`error` item): provider returned an error mid-turn,
  tool refused, model crashed mid-stream, etc. Renders as a red banner-
  style row in the timeline.
- **Persistent provider errors** (banner, NOT toast): provider binary
  missing, auth failure, version incompatibility, rate-limited and
  retrying. These render in a `ProviderStatusBanner` at the top of
  the chat (matching t3-code). They don't go in the item stream
  because they're not turn-scoped.
- **Truly transient infrastructure errors** (toast): SQLite write
  failure, IPC error, anything fatal-but-not-shown-in-chat. Routed via
  the existing toast store.

### Compactions

`kind=compaction` item with `summary` = e.g. "Context compacted —
older messages summarized". Renders as a horizontal divider with a
label. Compactions usually land at turn boundaries. If one fires
mid-turn, items before the divider are NOT marked specially — the
divider is enough indication.

### Working indicator

A footer component derived purely from `pane.isTurnActive`. Renders:

```
· Working · 12s · Esc to interrupt
```

The seconds counter reflects time since the most recent
`status=streaming` or `status=running` item appeared. Hidden when
`isTurnActive` is false.

This is NOT session status — there is no "connecting", "disconnected",
or "retrying" UI. The working indicator is purely turn activity feedback.

### Context window meter

A small circular progress indicator in the composer area (matching
forge / t3-code's `ContextWindowMeter`). Displays:

- Used % as the ring fill
- Tooltip / popover with: used tokens, max tokens, "compacts
  automatically" hint, recent token usage stats

**Subscribed to its own channel** (`provider:usage`) — backend keeps
emitting `EventTokenUsage` and `EventCompactBoundary` events; the
meter listens. Compactions reset the meter.

NOT in the chat history. Pure ambient indicator.

(Rate limits do NOT get UI in v1 — if relevant, surface in the same
popover as the context meter rather than a separate widget.)

### Removed entirely

- Session connection status (`connecting` / `connected` /
  `disconnected` / `reconnecting`). Replaced by working indicator (turn
  activity) + provider banner (persistent provider errors).
- Per-event `provider:meta` channel.
- Per-event `provider:event` channel for everything except status +
  approvals + usage (see Channels below).
- Token usage line items. (Meter still gets data via `provider:usage`.)
- Rate limit UI. (Backend keeps capturing for future.)

## Channels

The frontend listens to these Wails channels for chat state:

| channel                  | payload                              | purpose                                                |
|--------------------------|--------------------------------------|--------------------------------------------------------|
| `provider:item_upsert`   | full `Item`                          | every timeline state change                            |
| `provider:approval`      | `{action, request \| decision}`      | approval overlay state                                 |
| `provider:usage`         | `{tokens, contextPercent, ...}`      | context meter; not displayed as items                  |
| `provider:status`        | `{kind, message?}`                   | persistent provider banner (binary missing, auth, …)   |

App-shell channels (`design:*`, `thread:mode_changed`, etc.) stay
as-is. Toast channel for fatal infra errors stays as-is.

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

### Deferred completion queue

Backgrounded tool completions defer to a per-thread queue when there
is an active streaming item. Drained when the streaming item settles.

```go
type Router struct {
    ...
    deferredBgCompletions map[string][]store.Item // threadID → queue
}

func (r *Router) maybeDeferOrPersist(item store.Item, payload *store.Payload) error {
    if item.Kind == "tool_completion" && item.IsBackground {
        if r.hasActiveStreamingItem(item.ThreadID) {
            r.deferredBgCompletions[item.ThreadID] =
                append(r.deferredBgCompletions[item.ThreadID], item)
            return nil
        }
    }
    return r.persistItem(item, payload)
}

// Called whenever a streaming item flips to completed.
func (r *Router) drainDeferredCompletions(threadID string) {
    queue := r.deferredBgCompletions[threadID]
    delete(r.deferredBgCompletions, threadID)
    for _, item := range queue {
        r.persistItem(item, nil) // payload was already attached at queue time
    }
}
```

Crash safety: deferred completions are NOT yet in SQLite. On crash,
they're lost. That's acceptable — on resume, the underlying
provider's session log replay will re-emit the completion when the
session reconnects, and the deferral re-applies cleanly. (Worst case:
the user sees the launch row as `running` until the session resyncs.)

### Per-event handler logic

| event                       | handler                                                                                               |
|-----------------------------|-------------------------------------------------------------------------------------------------------|
| `EventTextDelta`            | upsert `assistant_text`; id = `text:<turnId>:<segmentIndex>`; append delta to summary; status=streaming |
| `EventThinking`             | upsert `thinking`; id = `think:<turnId>:<thinkingBlockIndex>`; append delta to summary                |
| `EventToolStart`            | upsert `tool_call`; id = evt.ItemID; status=running; is_background from classifier; tool_name set    |
| `EventToolComplete` inline  | upsert same `tool_call`; status=completed/errored (errored only when is_error=true); attach payload  |
| `EventToolComplete` bg      | route through `maybeDeferOrPersist` for the new `tool_completion`; summary restates command          |
| `EventDiff`                 | find/create the related `tool_call`, attach diff as its payload                                       |
| `EventCommandOutput`        | find/create the related `tool_call`, attach command output as its payload                             |
| `EventProposedPlan`         | upsert a `tool_call` with tool_name="plan"; payload carries the plan markdown                        |
| `EventError`                | upsert `error` item; id = uuid; summary = error message. ALSO: flip any `streaming`/`running` items in this turn to `errored` (live-crash flip) |
| `EventCompactBoundary`      | upsert `compaction` item; id = uuid; summary = compaction note. ALSO: emit `provider:usage` reset    |
| `EventTurnStart`            | open turn span (telemetry); capture checkpoint; no item                                               |
| `EventTurnComplete`         | flip any status=streaming items in this turn to status=completed; drain deferredBgCompletions; no other work |
| `EventApprovalRequest`      | emit `provider:approval` `{action:request, request}`                                                  |
| `EventApprovalResolved`     | emit `provider:approval` `{action:resolve, requestId, decision}`; upsert the underlying tool_call with `decision` set |
| `EventInit`                 | persist session_ref to thread; no item                                                                |
| `EventThreadRenamed`        | persist title to thread; emit existing `thread:*` event; no item                                      |
| `EventModelRerouted`        | persist model to thread; emit existing event; no item                                                 |
| `EventTokenUsage`           | emit `provider:usage` (for the meter)                                                                 |
| `EventRateLimits`           | emit `provider:usage` (folded in)                                                                     |
| `EventSessionStatus` (persistent failure) | emit `provider:status` for the banner (binary missing, auth fail). Transient — drop. |
| `EventBackgroundStart/Delta/Complete` | dropped — superseded by tool_call lifecycle with is_background classifier                    |
| `EventToolProgress`         | dropped — progress is just successive upserts of the tool_call's summary                              |
| `EventPlanUpdate`           | dropped — final plan arrives via EventProposedPlan                                                    |

The `provider.AllEventKinds` list shrinks accordingly.

### Live provider-crash flip

When a fatal provider event fires (subprocess exited, stream closed
unexpectedly, EventError with fatal indication):
- Flip every `status=streaming` and `status=running` item in the
  active turn to `errored` with summary suffix " — interrupted".
- Then upsert the `error` item.
- Drain pending deferred completions to `errored` items.

Without this, the user sees a streaming text item that never
finishes, sitting next to the new error item — confusing. The flip
makes the broken state explicit on the items themselves.

### Store changes

Add `UpsertItem(item Item, payload *Payload) (Item, error)`:

- If `item.ID` exists: update kind/role/summary/payload_id/status/
  is_background/completion_of/parent_id/tool_name/decision/updated_at.
  Item index is preserved.
- Else: assign next item_index for `(thread_id, turn_index)`, insert.
- If payload non-nil, upsert payload in same transaction.
- Returns the canonical persisted Item (with assigned item_index).

The single-writer connection pool (`SetMaxOpenConns(1)`) keeps the
upsert race-free.

Schema: v15 migration adds `tool_name TEXT NOT NULL DEFAULT ''` and
`decision TEXT NOT NULL DEFAULT ''` to `items`. No index changes
needed (these aren't query targets).

`InsertItem`, `AppendItem`, `AppendItemWithPayload`, the various
narrow updaters — kept for now, but most callers migrate to
`UpsertItem`. Audit at end of execution and delete unused.

## Frontend collapse

### Pane state (final shape)

```ts
items: Item[]                    // sorted by (turn_index, item_index)
pendingApprovals: ApprovalRequest[]
contextWindow: ContextWindowMeta // for the meter widget
providerBanner: ProviderBannerMeta | null  // persistent provider error
payloadCache: LRU<string, string>  // ~20 entries, drops on thread switch
thread: Thread | null
loading: boolean
// design + discussion + diff-panel + terminal state stay as-is
```

**Deleted:** `activeToolCalls`, `streamingContent`, `backgroundTasks`,
`pendingPlanUpdate`, `pendingMessage`, `dismissedPlanItemId`,
`payloadMetas` (replaced by LRU above), `tokenUsage` (replaced by
`contextWindow`), `rateLimits`, `sessionApprovedTools`, `error`,
`sessionStatus`. Everything that was a parallel state stream goes
away.

**One mutation:** `upsertItem(item: Item)`. Replaces by id (preserves
position) or inserts at sorted position. That's the only timeline
mutation.

### Listeners

```ts
on('provider:item_upsert', (item) => pane.upsertItem(item));
on('provider:approval', (msg) => {
  if (msg.action === 'request') pane.addApproval(msg.request);
  else pane.removeApproval(msg.requestId);
});
on('provider:usage', (usage) => pane.updateContextWindow(usage));
on('provider:status', (status) => pane.setProviderBanner(status));
```

Plus existing app-shell channels (design, thread mode, etc.) untouched.

### isTurnActive

```ts
get isTurnActive() {
  return items.some(i =>
    (i.kind === 'assistant_text' || i.kind === 'thinking') && i.status === 'streaming'
    || i.kind === 'tool_call' && i.status === 'running' && !i.isBackground
  );
}
```

Note: backgrounded tool_calls do NOT count as "turn active" — they
run independently and shouldn't block sends. Used by Composer to gate
sends + show interrupt button.

### finalizeTurn / switchThread

`finalizeTurn` deleted. The stream is authoritative; nothing to drain.

`switchThread` keeps its initial hydration via `ListItems` + a single
`GetContextWindow(threadId)` call to seed the meter. After that the
upsert + usage streams are the only mutation sources. `payloadCache`
clears on switch.

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

### ToolCallCard internals

- Header line: per-tool-kind dispatcher on `tool_name`. Shows icon,
  label, brief input preview, status badge, decision chip (if set),
  exit code or duration on completion.
- Chevron expand: fetch payload via `GetPayloadData(payloadId)`,
  cache in `pane.payloadCache` (LRU).
- Body switch on `payload.kind`: command_output / diff / tool_result /
  proposed_plan / text.
- Children (if subagent): rendered nested when the card is expanded.
  Default expanded while running (so you see live progress); default
  collapsed when card status=completed.
- Depth cap: at depth 3, children render as a "open subagent trace →"
  link instead of inline.

### Background tray

Renders items where `is_background && status == 'running'`, plus
pairs whose `tool_completion` is younger than 2s. Cap at 3 visible
rows + "+N more". Tray rows expose a stop button. Click row to
expand inline output.

### Working indicator

A small footer component (`ChatWorkingIndicator.svelte`) below the
Composer. Pure derivation:

```ts
let isWorking = $derived(pane.isTurnActive);
let elapsed = /* computed from earliest streaming/running item's createdAt */;
```

Renders `· Working · 12s · Esc to interrupt` when isWorking. Hidden
otherwise. The Esc binding hooks into the existing interrupt path.

### Context window meter

A small `ContextWindowMeter.svelte` component in the composer toolbar.
Circular progress ring, popover on click showing token details. Reads
`pane.contextWindow`. NOT a chat history item.

### Scroll behavior

Encoded as invariants in MessageTimeline:

- **Auto-scroll stickiness:** if the scroll container is within ~64px
  of the bottom, auto-scroll on new items. Otherwise show a
  "↓ N new messages" pill at the bottom edge.
- **Above-fold mutation anchoring:** apply CSS `overflow-anchor: auto`
  on the scroll container (or use scroll-anchor JS) so that an
  upserted item above the viewport doesn't reflow the user's reading
  position. Status flips and summary updates above the viewport
  pin the visible content.
- **Thinking expand/collapse:** preserves user's scroll position
  relative to the row that triggered the toggle.

## Crash recovery

Falls out of the design + the live-crash flip rule:

1. Every event upserts to SQLite **before** emitting. SQLite is
   always up-to-date through the last event (deferred completions
   excepted; see Deferred Completion Queue).
2. On app restart: `ListItems(threadId)` returns everything that was
   persisted, in order.
3. Provider session is restored separately by `session_ref` (existing
   plumbing).
4. If items were left with `status=streaming` or `running` on reopen:
   - For background tool_calls: probe session liveness. Process alive
     → keep running. Dead → emit synthetic completion (errored,
     "Interrupted — outcome unknown").
   - For streaming text/thinking: the resumed session continues
     delta-ing if available; otherwise flip to `errored` with
     "Interrupted" suffix.
5. If `pendingApprovals` were active at crash time: provider re-emits
   if supported (Claude does); otherwise tool_call goes to errored
   "Approval lost on restart — please retry".

The recovery contract: **what's in SQLite is what the user sees**.
Provider session state may be ahead, behind, or equal — none of those
break the UI.

## Demolition list (in execution order)

### Backend deletions

- `internal/triage/tool_lifecycle.go` (current implementation, wrong shape)
- `internal/triage/tool_lifecycle_test.go`
- `internal/triage/tool_result.go` (folds into payload-on-tool_call render)
- `internal/triage/command_inline_diff_persist.go` (same)
- `internal/triage/router.go`: `handleTextDelta` accumulator,
  `handleThinking` accumulator branch, `handleTurnComplete` drain logic,
  `textAccumulators`, `reasoningAccumulators`, `pendingCommandDiffs`,
  all the per-kind persist helpers
- `internal/provider/types.go`: `EventBackgroundStart/Delta/Complete`,
  `EventToolProgress`, `EventPlanUpdate`. Keep `EventTokenUsage`,
  `EventCompactBoundary`, `EventRateLimits`, `EventSessionStatus`
  (rerouted to non-item channels). Item kinds: collapse to the 7-kind
  enum; drop `ItemBackgroundStarted`, `ItemBackgroundDone`,
  `ItemToolResult`, `ItemDiff`, `ItemCommandExecution`, `ItemProposedPlan`.
  `ItemThinking` becomes the canonical thinking kind.

### Frontend deletions

- `pane.activeToolCalls`, `addToolCall`, `completeToolCall`,
  `updateToolProgress`
- `pane.streamingContent`, `appendTextDelta`
- `pane.backgroundTasks`, `addBackgroundTask`, `completeBackgroundTask`
- `pane.pendingMessage`, `setPendingMessage`
- `pane.pendingPlanUpdate`, `setPendingPlanUpdate`,
  `clearPendingPlanUpdate`
- `pane.dismissedPlanItemId`, `setDismissedPlanItemId`
- `pane.payloadMetas`, `addPayloadMeta`, `touchPayloadMeta` (replaced by LRU)
- `pane.tokenUsage`, `setTokenUsage` (replaced by `pane.contextWindow`)
- `pane.rateLimits`, `setRateLimits`
- `pane.error`, `setError`, `clearError` (audit — may be used outside
  chat data flow)
- `pane.sessionStatus`, `setSessionStatus`
- `pane.finalizeTurn`
- `frontend/src/lib/components/chat/StreamingMessage.svelte`
- `frontend/src/lib/components/chat/ProviderStatusBanner.svelte`
  (rewrite to consume `pane.providerBanner` instead of session status)
- `frontend/src/lib/components/chat/RateLimitsMeter.svelte`
- `frontend/src/lib/components/chat/ChangedFilesTree.svelte` — KEEP
- `frontend/src/lib/components/chat/TurnDiffBadge.svelte` — KEEP
- `frontend/src/lib/components/chat/CommandOutput.svelte`,
  `DiffPreview.svelte`, `ProposedPlanCard.svelte`,
  `ThinkingBlock.svelte`, `ToolResultCard.svelte` — fold into
  `ToolCallCard`'s payload renderer (one component per payload kind
  surviving)
- `frontend/src/lib/stores/events.ts`: the giant switch over `evt.kind`
  in `routeEventToPane` collapses to four listeners

### Frontend keeps & adds

- KEEP: `BackgroundTaskTray.svelte` (filter logic stays; add stop button)
- KEEP: `ToolResultDropdown.svelte` (becomes the payload renderer
  wrapped in ToolCallCard)
- KEEP: `MessageTimeline.svelte` (rewritten body, same shell)
- KEEP: `UserMessage.svelte`, `AssistantMessage.svelte` (rename to
  AssistantText if needed)
- KEEP: `SubagentGroup.svelte` (becomes part of ToolCallCard recursion)
- ADD: `ChatWorkingIndicator.svelte` (footer)
- ADD: `ContextWindowMeter.svelte` (composer toolbar; port forge's)
- ADD: `ToolCallCard.svelte` (the per-kind header + payload dispatcher
  + child recursion)
- All composer components, sidebar, terminal drawer, design view,
  discussion view — untouched

## Execution plan

One sequential pass. Each step ends with green tests.

1. **Schema v15** — `tool_name` and `decision` columns. Tests.
2. **`store.UpsertItem`** + tests. Audit and delete unused narrow
   updaters at the end.
3. **`router.persistItem` chokepoint** + `maybeDeferOrPersist` queue.
   Not yet called.
4. **Migrate handlers one at a time**, in this order:
   - `EventTextDelta` (replaces accumulator with streaming item +
     segmentation rule)
   - `EventThinking` (same)
   - `EventToolStart` / `EventToolComplete` (replace tool_lifecycle.go;
     bg deferral; decision field on approval resolve)
   - `EventDiff`, `EventCommandOutput`, `EventProposedPlan` (attach
     to tool_call payload; payload renderer dispatch)
   - `EventError` (new item kind + live-crash flip)
   - `EventCompactBoundary` (new item kind + reset usage emit)
   - `EventApprovalRequest` / `EventApprovalResolved` (new
     `provider:approval` channel; decision field upsert)
   - `EventTokenUsage` / `EventRateLimits` (new `provider:usage`
     channel)
   - `EventSessionStatus` (rewire persistent failures to
     `provider:status` banner; drop transients)
   After each migration, delete the old persist function and any
   dead state. Tests adjusted in the same step.
5. **Delete `handleTurnComplete` drain**. Replace with status-flip
   pass + drainDeferredCompletions.
6. **Delete obsolete event kinds** from `provider.AllEventKinds` and
   provider adapters that emit them.
7. **Frontend pane rewrite** — single PR-size chunk. Delete the state
   slices listed above. Implement `upsertItem` semantics, the four
   listeners, payload LRU, contextWindow state, providerBanner state.
   Update `isTurnActive`. Delete `finalizeTurn`.
8. **MessageTimeline rewrite + ToolCallCard** — new switch, per-kind
   header dispatch, payload renderer dispatch, subagent recursion
   with depth cap, scroll invariants.
9. **Working indicator + Context meter** — new components, wire to
   pane state.
10. **Background tray polish** — stop button, "+N more" cap.
11. **Tests** — integration test for the full flow: provider events
    in, single upsert stream out, frontend renders correctly with no
    shifts at turn boundary, crash recovery (kill mid-turn, reopen,
    verify items intact).

## Acceptance criteria

- During a turn, every observable state change appears in the timeline
  at its agent-perception position. Nothing is invisible until
  turn-complete.
- At turn-complete, the timeline does NOT shift, reorder, or surprise.
  Items already on screen stay where they are; new items append.
- Background completions appear after the active streaming item
  settles, never mid-stream.
- Killing the app mid-turn and reopening shows exactly what was
  persisted. Items in `streaming`/`running` state on reopen transition
  per the recovery rules; user sees explicit "interrupted" markers
  if the session can't be resumed.
- Backgrounded tool calls show one launch row inline + one completion
  row at the agent-perception position + tray mirror while running
  (with stop button).
- Subagents render nested under their parent tool call, default
  collapsed when complete, expanded while running. Depth-3 cap with
  side-trace fallback.
- Approvals resolve into a `decision` chip on the underlying
  tool_call, never a separate item. Decision history is preserved
  for scroll-back.
- Working indicator visible during turn activity. Context window
  meter visible at all times. No session status UI; persistent
  provider failures use a banner.
- Bash exit codes don't flip status to errored. Only is_error does.
- No `pane.activeToolCalls`, no `pane.streamingContent`, no
  `pendingMessage`, no `payloadMetas` map. Grep returns zero hits.
- Frontend `npm run check` clean. `go test ./...` green.
- Integration test covers: send message → tool call → tool result →
  assistant text → turn complete → reload thread → verify identical
  render. Plus: backgrounded command → user sends new message →
  completion lands at next agent boundary.

## What this does NOT cover

- Visual polish toward forge's chat density. Separate pass after this
  lands. The data model is the prerequisite.
- The 1GB memory issue. Separate investigation; suspects are
  eager-loaded syntax highlighter languages and Wails webview baseline.
  Won't be fixed by this rewrite (or made worse).
- Workflow / phase / gate system, remote/web access, auto-updater,
  mid-turn correction — already-deferred items in `AGENTS.md`. This
  rewrite doesn't touch them.
