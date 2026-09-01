# Chat Rewrite: Unified Item Stream

Status: spec, pre-execution. v2 incorporates review findings from
t3-code deep-dive, multi-reference cross-check (Claude Code CLI, Codex
CLI, CodexMonitor, Continue, Aider), and 25-scenario UX stress-test.

> **See also.** This doc is the spec that produced today's code. For
> the rules extracted from it, jump to:
>
> - [`invariants.md`](invariants.md): the load-bearing invariants,
>   with rationale and tests.
> - [`conventions.md`](conventions.md): contributor guardrails.
> - [`how-to.md`](how-to.md): step-by-step extension playbooks.
> - [`adrs/`](adrs/): architecture decisions captured as ADRs.
>
> This doc remains the historical record of the design; when it
> drifts from the code, trust the invariants / ADRs.

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
- Wholesale provider rewrites, sidebar, composer shell, terminal,
  discussion mode, or settings.
- UI visual polish. That's a separate pass after the data model
  lands.

## In-scope provider + schema changes

The rewrite requires some provider and schema work beyond "purely
additive." Called out up front so scope is clear:

- **Claude adapter**: preserve `content_block_start`/`stop`; handle
  `system/task_started`/`task_updated` for background lifecycle; keep
  `task_notification` out of lifecycle state and surface it only as an
  optional notification row; keep backgrounded-ack `tool_result`
  echoes on the tool's own lifecycle; persist thinking `signature`
  blobs for API replay.
- **Codex adapter**: handle `CollabAgentToolCall` variants (spawn,
  send, wait, close, resume); open child-thread subscriptions on
  SpawnAgent completion; rewrite the header-comment claim that
  Codex has no parent/child linkage.
- **Store schema v15**: additive columns (`tool_name`, `decision`,
  `meta`, `last_token_usage`) plus two renames
  (`parent_tool_use_id` → `parent_id`,
  `completion_of_item_id` → `completion_of`) plus a wipe of
  `items` and `payloads`. Not purely additive, so scope this work
  explicitly.

## The model

### Item

One row in `items`. Every visible thing in the chat is one of these.

```
id              stable string — format depends on kind; see Item ID schemas
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
decision        optional enum (tool_call lifecycle annotation):
                approved | declined | amended | lost
                approved/declined/amended populate from
                approval resolutions; lost populates on restart
                for approvals OR backgrounded tool_calls that died
                with their session. Renders as a small chip on
                tool_call rows; see Approvals + Crash recovery.
meta            TEXT (JSON), default '{}'. Provider-specific
                per-item metadata: Claude thinking `signature`,
                Claude task_id↔tool_use_id map, Codex
                receiverThreadId for subagent cards, denormalized
                live subagent activity fields. Adapter owns the
                schema per-key; renderer reads specific keys by
                name.
created_at      int64 ms
updated_at      int64 ms
```

**Column name decision:** the existing v14 schema uses
`parent_tool_use_id` and `completion_of_item_id`. Schema v15 renames
both to `parent_id` and `completion_of` in a single migration. The
old names were Claude-centric; the new ones are provider-agnostic and
reflect the unified item model (any kind can be a parent of any
other, e.g., an MCP tool with child tool_calls, not just a
`tool_use`). The v15 migration wipes `items` and `payloads` entirely
(see "Store changes" below), so column-value preservation doesn't
apply. The rename is really a schema-shape change with a fresh
start on data.

### Kinds (closed set: 7)

| kind              | role      | status semantics                              | notes                                                                                  |
|-------------------|-----------|------------------------------------------------|----------------------------------------------------------------------------------------|
| `user_text`       | user      | always `completed`                             | user message; appears at top of its turn                                               |
| `assistant_text`  | assistant | `streaming` while growing → `completed` at segment end | one per **output segment** (see segmentation rule below)                            |
| `thinking`        | assistant | `streaming` while growing → `completed`        | one per thinking block; default-collapsed when `completed`, expanded while `streaming`   |
| `tool_call`       | assistant | `running` → `completed` / `errored` / `declined` | inline tools mutate in place; backgrounded tools stay `running` and append a partner   |
| `tool_completion` | assistant | `completed` / `errored`                        | only exists for backgrounded tool calls; carries `completion_of`, the result payload, and a summary that **restates the original command** |
| `error`           | system    | always `completed`                             | turn-level error surfaced inline (provider crash, refusal, etc.)                        |
| `compaction`      | system    | always `completed`                             | marker: context was compacted at this point                                           |

Kinds NOT in this list: anything else. Diffs / command output / proposed
plans are **not separate item kinds**. They are tool_call items whose
payload renders specially (see Heavy Payloads).

### Item ID schemas

Every item's `id` is deterministic given its kind + turn context. This
matters for two reasons: (a) upsert idempotency on Claude `--resume`
replay, and (b) cross-references (`completion_of`, `parent_id`) are
stable without a lookup table.

Top-level items (parent thread, `parent_id = ""`):

| kind              | id format                                     | notes                                                                       |
|-------------------|-----------------------------------------------|-----------------------------------------------------------------------------|
| `user_text`       | `user:<turn_index>`                           | one per turn; keyed off the synthesized TurnStart's `turn_index` (int)      |
| `assistant_text`  | `text:<turn_index>:<segment_index>`           | one per output segment; `segment_index` from `segmentIndexByScope` counter  |
| `thinking`        | `think:<turn_index>:<block_index>`            | one per thinking block. `block_index` is MONOTONIC across the full turn (never resets per API cycle, even across tool_use → tool_result continuations) |
| `tool_call`       | provider-native id (see adapter contract)     | Claude: `tool_use.id`. Codex: `item.id` from v2 notifications (see adapter) |
| `tool_completion` | `complete:<tool_call.id>`                     | 1:1 with launch; deterministic so replay upserts, not duplicates            |
| `error`           | `error:<turn_index>:<error_seq>`              | `error_seq` is a per-turn counter (multiple errors in one turn possible)    |
| `compaction`      | `compact:<turn_index>:provider:<id>` or `compact:<turn_index>:seq:<n>` | one per compaction event; provider ids keep replay idempotent, seq is fallback |

**Child items (inside a subagent card, `parent_id = <card.id>`):**
id formats append the parent card id before the intra-card counter
so that parallel subagents in the same turn don't collide:

| kind              | id format                                                          |
|-------------------|--------------------------------------------------------------------|
| `assistant_text`  | `text:<turn_index>:<card_id>:<segment_index>`                      |
| `thinking`        | `think:<turn_index>:<card_id>:<block_index>`                       |
| `tool_call`       | provider-native id (adapter scopes within card context)            |
| `tool_completion` | `complete:<tool_call.id>` (still unique because tool_call.id is unique)  |
| `error`           | `error:<turn_index>:<card_id>:<error_seq>`                         |

Without the card_id scope, two subagents launched in the same turn
(parent turn_index=5, card A and card B) would both mint `text:5:0`
for their first child text segment and collide on upsert. The
`card_id` segment is the subagent `tool_call.id`, already unique
across the thread.

`turn_index` is used in id keys rather than any provider-supplied
`turnId` string. This keeps ids stable across adapters (Claude
doesn't emit a stable per-turn id; Codex does). The trade: ids are
only unique within a thread. Cross-thread id collisions are impossible
because `thread_id` is a separate column on every query.

**Adapter contract for tool_call id:** the provider adapter produces a
string that is stable across the tool's Begin/End events (so upsert
finds the same row) AND unique across the thread (to prevent
child-scope collisions). The field name varies per provider. The
triage layer and frontend never need to know which.

### Invariants

Load-bearing rules the spec depends on throughout. Listed here so they
don't get re-derived (or accidentally violated) in implementation.

1. **`item_index` is immutable after first upsert.** Assigned by the
   store at insert time; never rewritten. Updates preserve position.
2. **`item.id` is stable from stream start through completion.** A
   streaming `assistant_text` with id `text:5:2` stays `text:5:2` when
   it flips to completed. No ID rewrite at state transitions.
3. **`turn_index` is monotonic per thread.** Empty threads start at
   `0`; later sends use `LastTurnIndex(threadID) + 1` under the
   per-thread action lock.
4. **FIFO drain for the interrupt queue.** Parallel tool completions
   that queued during one streaming cycle must flush in arrival order
   so a `_End` never renders before its `_Begin` partner.
5. **`completion_of` references a `tool_call` with `is_background=true`.**
   Inline tool_calls mutate in place. They never produce a
   `tool_completion`. Only backgrounded launches do.
6. **At most one `tool_completion` per `tool_call`.** The relationship
   is 1:1. Synthetic "stopped by user" completions use the same id
   (`complete:<tool_call.id>`) and therefore upsert-replace any
   pre-existing completion row.
7. **`parent_id` points to a `tool_call`.** Only tool_calls can be
   parents (subagent containers, MCP tools with nested tool_calls).
   Not assistant_text, not thinking, not user_text.
8. **One item stream per thread, one writer per thread.** The
   per-thread action lock (`threadActionLocks`) serializes send flow;
   `SetMaxOpenConns(1)` serializes SQLite writes. Together these mean
   no two events for the same thread race on item_index assignment.
9. **`segmentIndexByScope` / `blockIndexByScope` keyed by
   `(threadID, turn_index, scope)`** where `scope` is either `""`
   (top-level, parent thread) or a subagent `card_id` (child items
   within that card). Resets to 0 on TurnStart (for the top-level
   scope) or on card creation (for child scopes). Increments on
   every EventToolStart within that scope.
10. **Subagent child events inherit the subagent card's
    `turn_index`.** A subagent launched in turn 5 that emits events
    while the parent has moved to turn 7 persists its events under
    turn 5 (the launching turn). `item_index` remains monotonic
    across the full thread.
11. **`item_index` is assigned in intended-appearance order, not
    wire-arrival order.** For inline events this is automatic (begin
    fixes the index; end mutates the same row). For anything that
    triage creates as a NEW row during an active streaming phase
    (currently: backgrounded tool_completion), the streaming-phase
    interrupt queue defers `item_index` assignment until the stream
    settles. If new event kinds are added later that produce fresh
    rows mid-turn, they must route through the queue too, or the
    same "new row inserts before the streaming tail" bug recurs.

### Status / streaming / settle

For `assistant_text` and `thinking`:
- `streaming` while content is growing
- `completed` at segment end (text) or block end (thinking)

For `tool_call`:
- `running` from EventToolStart until EventToolComplete
- `completed` if the provider reports success (even with non-zero
  exit code: `grep` returning 1 for "no match" is normal; that
  does NOT flip to errored)
- `errored` when the provider reports failure
- `declined` when an approval was denied

**Per-provider status derivation** (adapter's job to normalize):
- **Claude**: `meta.is_error == true` → `errored`; otherwise
  `completed`. Non-zero exit is preserved in the summary but does
  not change status. Declined approvals → `declined`.
- **Codex**: the ThreadItem variant carries an explicit status
  enum. `CommandExecutionStatus`: `in_progress | completed | failed |
  declined`. `McpToolCallStatus`: `in_progress | completed | failed`.
  `FileChangeStatus`: same. `CollabAgentToolCallStatus`: same.
  Adapter maps: `in_progress` → `running`, `completed` → `completed`,
  `failed` → `errored`, `declined` → `declined`.

The non-zero exit code (Bash tools) is preserved in the `summary`
(e.g. `"Bash: grep foo  (exit 1)"`) but does NOT change status.
This is a deliberate decoupling. Many shell tools use exit codes
for flow control, not failure.

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
one item that renders BEFORE the tool, which is wrong.

**Adapter requirement**: to know when segments/blocks start, the
Claude adapter MUST preserve `content_block_start` / `content_block_stop`
events from the wire. The current adapter discards these and only
emits `text_delta` / `thinking_delta`. That needs to change as part
of execution. Without block boundaries, the triage layer cannot tell
when to bump `segmentIndexByScope` / `blockIndexByScope` for a new
segment within the same API response (e.g., model emits `thinking →
thinking → text` with two thinking blocks in one response, and without
block boundaries we'd fuse them into one).

**Router state:** tracks `segmentIndexByScope[threadID][turn_index][scope]`
as an int counter, where `scope` is either `""` (top-level parent
thread) or a subagent `card_id` (for child items inside that card).
Incremented on every EventToolStart emission within that scope
(inside the triage layer, before text-segmentation logic reads it).
The current counter value is used to build the text item id for
subsequent EventTextDeltas in that scope. Cleared at turn-complete
for the top-level scope; cleared when a subagent card's final event
lands for child scopes. Thread-safe via the router's existing mutex.
Same shape applies for `blockIndexByScope` (thinking block counter).

### Streaming text body handling

Streaming items (`assistant_text`, `thinking`) grow `summary` directly.
No accumulator in Go memory. No drain at turn-complete. Every delta
upserts the item with the appended summary.

**Promotion to payload:** ONLY at segment-complete (status flip from
streaming → completed). If the final summary exceeds 50KB at that
moment, write it to a payload and replace summary with the first 4KB
as a head preview. Frontend renders summary by default, expand fetches
the full payload.

This avoids the visual ejection that would happen if we promoted
mid-stream. The user keeps reading uninterrupted; the cap only kicks
in once the segment settles.

**Thinking signature preservation (Claude-specific):** Claude's
`thinking` content blocks carry a `signature` field, an opaque
server-side encrypted blob that the API requires round-tripped
unchanged for resume / tool-use continuations within a turn. The
client never validates it; it just stores and replays. Our Claude
adapter MUST capture the `signature` on every thinking block and
store it alongside the content (in the item's payload or meta JSON).
On resume, the adapter re-submits the thinking block with its
original signature, or the API 400s. This is a payload-preservation
concern, not an identity concern. The `think:<turn_index>:<block_index>`
id is what the UI tracks; the signature rides along for API replay.

A subagent is any sub-session spawned by the model: Claude's `Task`
tool, Codex's `CollabAgentToolCall` (SpawnAgent). Both represent
"parent agent delegated work to a child."

**One `tool_call` card per subagent** in the parent's timeline,
regardless of how many internal lifecycle events the provider emits
for it. For Codex's case, the `SpawnAgent` call creates the card;
later `SendInput` / `Wait` / `Close` events do NOT each produce a
new card. They fold into the card's lifecycle (`SendInput` is
handled separately, see below; `Wait` is implicit in `status=running`;
`Close` triggers the status transition).

**Card header** (visible while collapsed, load-bearing for "I know
what the child is doing without expanding"):
- Agent name / tool name (e.g., "Robie [explorer]" or "Task:
  refactor auth")
- Live count: `N items` where N is number of child items with
  `parent_id == card.id`
- Elapsed: `12s` from card's `created_at` while running
- Latest activity: a short summary derived from the most recent
  child event, e.g., "editing src/foo.rs" / "running: pytest" /
  "thinking". Denormalized into a field on the card (the triage
  handler for child events updates it as they arrive).

**Card content on expand**: the child's FULL conversation in order:
assistant_text, thinking, tool_calls, everything that would show in
the child's own timeline. Not just tool calls. Rendered using the
exact same per-kind switch as the top-level timeline (recursive
MessageTimeline with `items.filter(i => i.parent_id == card.id)`).

**Inline vs. backgrounded**:
- If the subagent is inline (`is_background=false`): the card
  renders as a line item in the parent's timeline, in sequence
  with other items.
- If the subagent is backgrounded (`is_background=true`): the card
  ALSO renders in the background tray as an individual row (matches
  any other backgrounded tool). Timeline still has the inline card;
  tray is the running-indicator mirror.
- Multiple concurrent subagents: each gets its own card in the
  timeline (sequential line items, not side-by-side layout, since they
  just appear in order as their spawn events arrive). User expands
  each independently.

**Child-event persistence** (the adapter's job):
- Each child item is persisted in our store with `parent_id` set to
  the subagent card's id, `thread_id` = parent's thread_id.
- **Child items inherit the subagent card's `turn_index`**, NOT the
  parent thread's current turn_index at event arrival time. A
  subagent spawned in turn 5 that keeps running after the parent
  advances to turn 7 still has its events belong to turn 5 (the
  turn that spawned it). `item_index` is assigned normally
  (monotonic across the thread); subagent events are sparse within
  their turn but `parent_id` filtering always collects them
  correctly for display.
- The card's provider-specific linkage (e.g., Codex
  `receiverThreadId`) is stored in the card item's payload or meta
  JSON so reopen can resubscribe to live children.

**Provider-specific plumbing**:
- **Claude `Task` tool**: events already arrive with
  `parent_tool_use_id` set. Direct 1:1 map onto `parent_id`. No
  extra subscription: Claude emits child events on the same
  stream.
- **Codex `CollabAgentToolCall`**: child runs on a separate
  Codex `thread_id` (`receiverThreadIds`). Codex emits multiple
  `CollabAgentToolCall` items across a subagent's lifecycle
  (SpawnAgent, SendInput, Wait, CloseAgent, ResumeAgent), each as
  its own item with its own `tool` field. **`receiverThreadIds`
  is populated on the SpawnAgent item's COMPLETED notification,
  not STARTED** (the app-server doesn't know the thread id until
  the spawn finishes). The Codex adapter:
  1. On `item/started` for a `CollabAgentToolCall` with
     `tool == "SpawnAgent"`, persist the parent card item
     (status=running, but `receiverThreadId` not yet known).
  2. On `item/completed` for the same SpawnAgent item, extract
     `receiverThreadIds`, store them on the card's meta, and open
     subscriptions to those child threads via the session registry.
  3. Subsequent `SendInput` items are surfaced as separate line
     items in the parent timeline (see "Parent-to-child input" below).
  4. `Wait` items are silent: their state is already reflected in
     the subagent card's `status=running` until `turn/completed`
     fires on the receiver thread.
  5. `CloseAgent` / `ResumeAgent` items transition the card's
     status (close → `completed`/`errored`, resume → back to
     `running`).
  6. Child-thread events from any subscribed receiver are
     re-emitted onto the PARENT thread's store with `parent_id =
     card.id`.
  7. On child's `turn/completed`, if the subagent isn't explicitly
     closed yet, the card stays `running` (agent may Wait on the
     child again). The card transitions to `completed` only when
     Codex emits the CloseAgent item.
  8. Our `internal/provider/codex/protocol.go` header comment
     (claims "no parent/child linkage") is out of date and must
     be rewritten as part of execution.
  9. `ClassifyNotification` must handle `item.type ==
     "collabAgentToolCall"` and dispatch on its `tool` field.

**Parent-to-child input** (Codex `SendInput`):

When the parent agent sends a mid-flight message to a child (Codex
emits a `CollabAgentToolCall` with `tool == "SendInput"`), it
appears as a **separate line item in the parent's timeline**, a
small tool_call row with `tool_name = "send_input"`, summary = the
input content preview + target agent name ("→ Robie: please
continue"). Not nested under the subagent card, not hidden. The
user (human) never sends input to subagents directly; this is
always an agent-to-agent message, and it's rendered as an action
the parent agent took.

**Nesting depth cap** (Codex-only concern):

**Claude's `Task` tool does NOT allow subagents to spawn further
subagents** per the official Claude Agent SDK docs. So
grandchild nesting is architecturally impossible on the Claude
side, so the depth cap does not apply.

**Codex CollabAgent** does support multi-agent orchestration
that could in principle spawn further agents from a child. For v1,
if a Codex subagent itself spawns a sub-subagent (rare but
possible), we show the grandchild as a minimal "spawned subagent"
marker inside the child's expansion, NOT its full conversation.
Grandchild events are not subscribed to, not persisted. The marker
shows the child name/prompt so the user knows delegation happened.
If real usage demands deeper nesting later, this becomes a future
feature.

**Crash recovery for subagents:**
- On reopen, walk all `running` tool_calls with `tool_name =
  "collab_agent"` (Codex) or `"task"` (Claude).
- For Codex: read `receiverThreadId` from the card's meta, call
  `thread/read { threadId }`. If status is alive, resubscribe;
  persist any missed items. If dead/done, fetch final items and
  transition card to completed/errored.
- For Claude: the `--resume` path delivers child events again;
  they re-upsert under the same `parent_id` without duplication.

**Cycle guard**: if `parent_id` would create a cycle at render
time, render flat at top level with a warning chip. Persist guard:
reject child events where `parent_id` doesn't resolve to an
existing tool_call in the same thread.

### Tool call lifecycle

Inline (foreground) tool call:
- One `tool_call` row, lives on the launch turn.
- `running` → `completed` / `errored` / `declined`.
- Summary carries: `tool_name + input preview` initially, then
  appends `(exit N)` or `(error)` on completion.
- Result payload attached to the same row.

Backgrounded tool call:
- `tool_call` row stays `running`, `is_background=true`.
- A separate `tool_completion` row is appended at the current thread
  write head (or the latest persisted turn when no turn is open),
  deferred behind active streaming output when necessary.
- `tool_completion.summary` **restates the original command** plus the
  outcome, e.g. `"pnpm install → exit 0 in 12s"`. Without this the
  late-arriving row is decontextualized.
- `tool_completion.completion_of` = launch.id; `is_background=true`;
  status = `completed` / `errored`.

**Claude backgrounded tool terminal-signal flow** (adapter concern,
load-bearing for correctness). For `run_in_background: true` Bash
and Task tools, the immediate `user.tool_result` echo with content
like "Command running in background…" is NOT a real completion.
It's a backgrounded-ack.

**Primary signal: `system/task_updated` with `patch.status ∈
{completed, failed, killed}`.** Fires when a backgrounded task
ends. Carries `task_id` only, so the adapter must hold a
`task_id ↔ tool_use_id` mapping (learned from
`system/task_started`, persisted to `items.meta` so it survives
reconnect).

**Notification signal: `system/task_notification`.** Fires after the
terminal task state as an agent-facing notification. Carries
`task_id`, usually `tool_use_id`, `status`, `summary`, and
`output_file`. It is NOT a completion source. If surfaced in the UI,
it must be a distinct notification row and must not mutate task state
or dedupe against `task_updated`.

**TaskOutput tool_result**: when the model calls the `TaskOutput`
tool to poll a backgrounded item, the result content carries
`<status>completed|failed</status>` and richer result data (exit
code, output file path, final agent result text) for the SINGLE
`task_id` the model passed in. TaskOutput is 1:1 with its input
by wire-protocol design: its serializer emits one `<task_id>`,
one `<status>`, one `<output>`; it cannot fan out to multiple
completions even if other tasks finish during its blocking wait.
Verified by live spike: if TaskOutput polls task A and task B
finishes during the wait, `task_updated` fires independently for
task B and TaskOutput's returned content contains only task A.

Fresh app-style spikes under
`docs/references/fixtures/claude/` confirmed that TaskOutput does
not consume or replace `task_updated`: when TaskOutput blocks on a
running task, `task_updated` lands first and the TaskOutput
`tool_result` lands second. If a task has already completed and its
notification was delivered, later TaskOutput can return "No task
found". TaskOutput is an explicit retained-task retrieval, not a
durable history lookup.

Implication: `task_updated` is the normal lifecycle completion signal.
TaskOutput's value is output/exit-code detail because the agent
explicitly asked for it. The UI should not rely on TaskOutput for
normal background completion, and should avoid duplicating its content
into the background completion row unless that row is expanded or
needs a fallback detail source.

**Claude adapter execution work required:**
- The `parse_system.go` `task_started` / `task_updated` /
  `task_notification` handlers must cover all three paths:
  - `task_started` → store `task_id ↔ tool_use_id` mapping; persist
    to `items.meta` for reconnect recovery.
  - `task_updated` with terminal status → look up tool_use_id from
    the map; emit the task-lifecycle terminal event for the
    background completion sibling.
  - `task_notification` → optionally emit a distinct notification
    event/row. Do not emit a task completion from it.
- The `parse_user.go` `tool_result` echo path must suppress
  status mutation when the result is a background placeholder, but it
  still emits the tool's own `EventToolComplete`; triage keeps the
  launch row `running`.
- When `TaskOutput`'s tool_result arrives, extract its output /
  exit code / result text as optional enrichment/fallback for the
  corresponding `tool_completion` item's payload. Do not depend on
  TaskOutput for normal completion.

**Provider-specific `item.ID` sources for tool_call:**
- **Claude**: `tool_use.id` from the assistant message content block.
- **Codex**: `ThreadItem.id` on v2 notifications. Per Codex's own
  source (`item_builders.rs`), every tool-bearing item copies
  `payload.call_id` into the `ThreadItem.id` field, so the exposed
  `item.id` IS the `call_id`. Our current Codex adapter already
  reads `item.id` correctly. **Load-bearing correlation.**
  `parallel_tool_calls` is a per-model capability in Codex (not a
  global default); when enabled, parallel tool tasks are
  `tokio::spawn`ed independently and Begin/End events interleave on
  the wire. Codex's own source comments this: "Command completions
  can arrive out of order." Correlation MUST be by `item.id`, not
  arrival order.

### Background completion ordering: streaming-phase interrupt queue

When `EventToolComplete` arrives for a backgrounded tool, the
completion does NOT land at wall-clock chronological position. If a
streaming item is active, the completion is buffered in a per-thread
queue and drained when the streaming item settles.

This is the same mechanism Codex's TUI uses. See
`InterruptManager` / `flush_interrupt_queue` in codex-rs's
`chatwidget/interrupts.rs`. The trigger is simple: "a streaming cell
in this thread is mid-commit, hold tool events until it settles."
Mental model grounded in a working reference implementation.

Algorithm in the triage layer:

```
on EventToolComplete (background):
  1. If hasActiveStreamingItem(threadID) == false:
       upsert the tool_completion immediately at next item_index.
  2. Else: enqueue the completion in the per-thread interrupt queue.
       When the LAST streaming item in the thread flips to completed
       (last assistant_text/thinking settles; or turn-complete fires
       and settles them all at once), drain the queue FIFO and upsert
       each queued completion at next item_index.
```

**`hasActiveStreamingItem(threadID)` predicate:** returns true if any
item in `items` for that thread has `status == streaming` (kind is
`assistant_text` or `thinking`, the only kinds that carry that
status). Drain fires only when this predicate transitions from true
to false. If the model emits concurrent thinking + text (rare but
possible), neither settling alone triggers drain. Only when both
have settled does the queue flush.

**FIFO drain is load-bearing.** If multiple completions land during
one streaming cycle (parallel background tools), they must flush in
arrival order. Out-of-order would let a `_End` render before its
`_Begin` partner on screen. Codex's `InterruptManager` comment
explicitly calls this out; we match.

**Per-provider trigger frequency:**
- **Codex**: common. Parallel tool Begin/End events interleave with
  streaming text deltas routinely. The queue is load-bearing for
  visual coherence.
- **Claude Code**: rare. The model serializes text → tool → result
  within a turn, so background completions concurrent with streaming
  text happen only when the user kills an old shell while a new turn
  streams, or similar edge cases. The queue costs nothing when idle;
  we don't special-case per provider.

Visually, a background completion appears AFTER the assistant's
current text/thinking finishes, never mid-stream. The launch row
stays at its original position (turn N). The completion can land in
turn N (if it finishes within that turn's stream) or in turn N+M
(if the user continued the conversation while it ran). Either way,
it lands at the post-streaming boundary.

### Stop control

Stop controls split by scope:
- The Composer "Stop" button interrupts the active foreground turn.
- The background tray manages outliving work after the turn yields:
  Claude exposes per-row stop where upstream supports `stop_task`,
  plus stop-all; Codex exposes stop-all background terminals only.

**Wire calls per provider:**
- **Codex**: `turn/interrupt { threadId, turnId }`.
- **Claude foreground turn**: the existing interrupt path (SDK
  `abort('interrupt')` on the shared controller, as `REPL.tsx` does
  upstream).
- **Claude background task**: `stop_task` control_request keyed by
  `task_id`.
- **Codex background tray**: `thread/backgroundTerminals/clean`
  (thread-wide only; no per-row terminal/subagent stop upstream).

**Cascade behavior: what survives a user interrupt** (verified
against codex-source and claude-code-source-code; tables below
reflect native CLI behavior, which we match rather than fight):

| Provider | Item kind | Survives interrupt? |
|---|---|---|
| Codex | child subagent turn (CollabAgent card) | YES |
| Codex | running tool_call (bash, unified_exec, etc.) | YES: unified_exec is not terminated by `turn/interrupt` |
| Codex | backgrounded tool_call (`is_background=true`) | YES |
| Claude | sync inline Task subagent (`is_background=false`) | NO: killed via shared abort controller |
| Claude | async Task subagent (`is_background=true`) | YES: unlinked controller |
| Claude | running inline tool_call (non-Task) | NO: shared controller kills |
| Claude | backgrounded Bash (`is_background=true`) | YES: listener detached, reason='interrupt' ignored |

**Triage handling on interrupt** (minimal; let the wire drive
transitions where possible):
1. Immediately flip all `status=streaming` items on the parent thread
   (assistant_text, thinking) to `errored` with summary suffix " —
   stopped". The wire will NOT emit a completion for cut-off
   streams, so we synthesize it here.
2. Do NOT proactively flip `running` tool_calls. Let the natural
   event flow handle them:
   - Codex `turn/completed{status:"interrupted"}` fires for the
     parent turn → synthesize TurnComplete; drain interrupt queue to
     errored.
   - Codex tool_calls that survived keep their `status=running`. The
     wire will emit their completion events normally if/when
     they actually finish.
   - Claude tool_calls that died (shared controller) emit
     tool_results with abort errors → normal `EventToolComplete`
     handling flips them to errored.
   - Claude items that survived (backgrounded Bash, async Task)
     keep their `running` status, which is correct.
3. Emit a system `error` item with summary "Stopped by user" on
   the parent turn for scroll-back clarity.

**Note about subagents**: after a Stop on the parent thread, Codex
subagent cards (and Claude async Task cards) may keep running for
minutes or longer. This matches both CLIs' native behavior and is
visible to the user via the still-running card + the tray. If the
user wants to kill a specific surviving subagent, they must open
that subagent context and interrupt it separately. Per-subagent
Stop is NOT in v1.

**Tray rows**: show `running && is_background` with a live progress
indicator and elapsed time, but NO stop button. Clicking a tray row
scrolls/expands the corresponding inline item. The global Stop
button is the only stop affordance.

**Per-item stop (post-v1 extension, primitives verified)**: Claude
exposes a client-sent `stop_task` control_request with unified
`task_id` namespace covering both `run_in_background` Bash and Task
subagents. See
[`claude-wire.md §stop_task`](../references/claude-wire.md#stop_task).
Codex exposes both: per-process
`thread/backgroundTerminals/terminate {threadId, processId}` (since
codex 0.140.0) and the thread-wide `clean`. See
[`codex.md §Background terminals`](../references/codex.md#background-terminals).
Codex `spawn_agent` child threads still have no client kill path.
`close_agent` is a model tool only.

**On app reopen** with a `running && is_background` launch and no
matching completion: the adapter probes session liveness and
classifies the outcome.

Liveness probe mechanism per provider:
- **Codex**: call `thread/read { threadId }`. Returned
  `Thread.status.type` is one of `notLoaded | idle | active |
  systemError`.
  - `active` or `idle` → session alive, keep `running`.
  - `notLoaded` → **NOT necessarily dead**. This just means the
    thread isn't currently in memory. Call `thread/resume
    { threadId }` to rehydrate. If resume succeeds → treat as
    alive, subscribe to its events. If resume errors → treat as
    lost.
  - `systemError` → treat as lost (flip to errored,
    `decision=lost`).
  - `thread/status/changed` push notifications also fire during a
    live session; consume them after a successful resume.
- **Claude**: session log file exists under `~/.claude/` and can be
  resumed via `--resume <session-ref>`. Liveness = "the session ref
  is still resumable." The Claude adapter's existing resume path
  already distinguishes recoverable vs not; reuse that.

Outcomes:
- Provider session alive: keep `running` (the adapter's stream
  reconnect will deliver the real completion if/when it arrives).
- Provider session alive but no trace of the specific tool/process:
  emit synthetic completion, status=errored, summary="Interrupted —
  outcome unknown".
- Provider session not recoverable (status=systemError, resume
  refused, process gone): flip launch to errored, decision=lost
  (matches the Approval lost on restart rules).

Tray row drops in all but the first case. Timeline shows the launch
as errored with the completion partner.

### Background tray (frontend derivation)

Pure derivation over `ListLiveBackgroundTasks(threadId)` so old live
background work does not need to be loaded in the timeline window:
- Show launch items where `is_background == true && status == 'running'`.
- Pair with their `tool_completion` partner if present.
- Keep a row visible for 2s after its completion lands, then remove.
- **Cap visible rows at 3**, with a "+N more" stack collapsing older
  entries. Order newest-first.
- Tray is a mirror: launch and completion rows ALSO render inline
  in the main timeline at their persisted history positions.
  Tray is a duplicate view for "what's running RIGHT NOW," NOT a
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
  next upsert (when the tool actually starts with the new args). The
  MODIFIED input is the permanent record. The original input is not
  retained separately. Intent: scroll-back shows what actually ran,
  not a prior draft. If audit of the ORIGINAL ask matters, that
  belongs to the approval request's meta (preserved in payload, not
  in the item row).
- `lost`: the app or provider session died before the user could
  respond. See "Approval lost on restart" below.

**Pre-tool-start approvals** (Claude's flow): Claude emits the
approval request BEFORE the matching `tool_use`. If the user
**approves**, the tool_use arrives soon after, the tool_call item is
upserted at EventToolStart, and the decision chip is applied on the
next upsert. No issue.

If the user **declines before tool_use ever fires** (no tool_call
item exists yet), the triage layer MUST create one at
EventApprovalResolved time:

- id = the anticipated tool_use_id for this approval. Claude's
  CanUseTool control_request includes this; the adapter surfaces
  it on the `ApprovalRequest` struct. **Execution note**: the
  current `provider.ApprovalRequest` (`internal/provider/types.go`)
  carries `RequestID`, `ToolName`, `Input`, etc. but NO
  `ToolUseID` field. Adding one is required: the Claude adapter
  populates it from CanUseTool, Codex leaves it empty (Codex
  approvals are kind=command/file-change which always fire AFTER
  the tool's own `item/started`, so a tool_call always exists
  already).
- kind = tool_call, status = declined, decision = declined
- tool_name + summary = derived from the approval request payload
  (Claude includes `toolName` and `input` on the request)

Without this, a declined-before-start request leaves no trace; the
user's decline is visibly lost. The created tool_call row carries
the permanent record of what was asked and that it was rejected.

**Decision chip scope**: the chip (`✔ approved`, `✗ declined`,
`~ amended`, `⊘ lost`) is meaningful for
**tool-flavored approvals** (kinds: `command`, `file-read`,
`file-change`, `permission`). Non-tool approval kinds
(`user-input`, `mcp-elicitation`) don't create tool_calls. They
show in the composer approval panel, the user answers, and the
answer becomes part of the conversation via a subsequent assistant
response or follow-up message. No decision chip because there's no
underlying tool_call to chip. The approval overlay simply clears
on resolve.

Scrolling back you can see what was approved/declined/lost when,
without polluting the timeline with extra items. The tool_call's
`summary` shows the input that was ultimately used: the original
for approved/declined/lost, the modified for amended.

**AskUser-style tools** (an explicit "ask the user a question" tool)
are NORMAL `tool_call` items: the question is in the summary, the
answer becomes the tool result. Not an approval.

**Approval lost on restart:** the `pendingApprovals` overlay is
volatile. In practice, every provider we support (Claude Code, Codex)
kills pending approvals when the session dies. There is no
re-emission path. Don't try to resume them.

On reopen, any `tool_call` with `status=running` and no matching
completion is flipped to `status=errored`, `decision=lost`. The row's
`summary` already carries the question / input preview that was
asked, so the historical record is preserved: "Bash: rm -rf foo"
with a `⊘ lost` chip tells the user exactly what the agent wanted
to do and that it never got an answer. The user can manually re-send
the prompt if they want to retry.

This applies BOTH to approvals that were mid-pending at crash AND to
backgrounded tool_calls whose process died along with the app: same
resolution, same `lost` decision.

### Heavy payloads: two-stage expand

`payloads` table unchanged. Items reference payloads by `payload_id`.

**Memory rule:** nothing heavy lives in frontend memory unless the user
has explicitly expanded it AND asked for the full load. Collapse
releases. No client cache.

**Stage 1: Peek** (cheap, automatic on expand):
- Binding: `GetPayloadPreview(threadId, payloadId, maxBytes) -> {data, nextOffset, totalSize, isComplete}`
- Default `maxBytes = 32KB`. Fetches from the head of the payload.
- `isComplete=true` if totalSize ≤ maxBytes. That's the whole payload
  and stage 2 is skipped.
- Rendered as raw text through the client-side payload renderer
  (`AnsiText` for terminal-style output).

**Stage 2: Full load** (on demand, explicit):
- If stage 1's `isComplete=false`, show a footer inside the dropdown:
  `Show full output (2.3 MB) ↓` (or similar, with the formatted size).
- Click → repeated `GetPayloadChunk(threadId, payloadId, offset, maxBytes)`
  calls append raw chunks from `nextOffset` until `isComplete=true`
  (bounded by the 4MB cap: see size limits below).
- Replaces the stage 1 render with the assembled raw content. Same
  client-side renderer.

**No cache:**
- Collapsing the dropdown discards the loaded data.
- Re-expanding re-fetches the stage 1 peek (32KB over IPC ≈ 5ms,
  imperceptible).
- If user wants the full load back, they click "Show full" again.
- Trade: tiny re-fetch cost for guaranteed bounded memory. Accept it.

**Renderer dispatches by `payload.kind`:**
- `command_output` → terminal-style scrollable, ANSI rendered
- `diff` → file-tree peek (stage 1 shows the tree, each file's diff is
  its own stage-2-style load when expanded)
- `tool_result` → existing structured render (file_change schema)
- `proposed_plan` → ProposedPlanCard (always small; stage 1 loads full)
- `text` / unknown → preformatted

**Size limits:**
- Backend captures payloads with a 2MB head + 2MB tail policy. Beyond
  4MB total, the capture handler writes the first 2MB, inserts a
  marker line `[... N MB truncated ...]`, then keeps writing the last
  2MB (ring buffer). For `cargo build` / `go test -v` that exceed
  4MB, head-and-tail preserves the interesting parts (setup + final
  errors). Implementation lives in each provider adapter's command
  output handler.
- Frontend never loads more than 4MB per payload. Stage 2's full load
  renders in a `<pre>` with `overflow:auto`. Chrome/Firefox handle
  4MB of preformatted text fine (no virtual scrolling needed).
- **Save-to-file escape hatch:** a small "Save to file..." button in
  the dropdown header calls `SavePayloadToFile(threadId, payloadId)` which
  writes the full captured payload (up to 4MB) to a user-chosen path
  via the OS save-file dialog. For users who want to grep / diff /
  editor-view the raw output. No larger-than-4MB recovery: what
  wasn't captured is gone.

**For diffs with >10 files:** the file tree IS the stage-1 peek. Each
file's unified-diff body is its own stage-2-style on-demand fetch when
the user expands that file's tree node. Tree construction is cheap
(filenames + insertions/deletions per file, always loaded with the
payload meta).

### Per-tool-kind header rendering

The `tool_call` item carries `tool_name`. The renderer maps it to a
header row with appropriate icon + label format:

- `Bash` → terminal icon + command preview
- `Edit` / `Write` / `MultiEdit` → file icon + path + change summary
  on completion
- `Read` → eye icon + path + line range
- `Grep` → search icon + pattern + path filter
- `WebFetch` / `WebSearch` → globe icon + URL/query
- `Task` → robot icon + subagent description (Claude inline
  subagent; parent-of-children in the nesting model)
- `collab_agent` → robot icon + agent nickname + role + model
  (Codex subagent; parent-of-children; lifecycle spans
  SpawnAgent through CloseAgent as one card)
- `send_input` → speech-bubble icon + target agent name + input
  preview (Codex parent-to-child message; appears as a sibling
  line item in the parent timeline, NOT nested under the
  subagent card)
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
  - If the error message exceeds 50KB (rare, usually stack traces),
    apply the same promotion rule as text: write to payload, truncate
    summary to first 4KB.
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
mid-turn, items before the divider are NOT marked specially. The
divider is enough indication.

Provider compaction ids are preserved when present. Without one, triage
uses a per-turn sequence so multiple same-turn compactions do not overwrite
each other.

### Working indicator

A footer component derived purely from `pane.isTurnActive`. Renders:

```
· Working · 12s · Esc to interrupt
```

The seconds counter reflects time since the most recent
`status=streaming` or `status=running` item appeared. Hidden when
`isTurnActive` is false.

This is NOT session status. There is no "connecting", "disconnected",
or "retrying" UI. The working indicator is purely turn activity feedback.

### Context window meter

A small circular progress indicator in the composer area (matching
t3-code's `ContextWindowMeter`). Displays:

- Used % as the ring fill
- Tooltip / popover with: used tokens, max tokens, "compacts
  automatically" hint

**Subscribed to its own channel** (`provider:usage`). The backend keeps
emitting `EventTokenUsage` and `EventCompactBoundary` events; the
meter listens.

**Payload shapes on `provider:usage`:**
- Context update: `{action: 'usage', threadId, usedTokens, maxTokens, contextPercent}`

**Seed on thread switch:** we need the meter to show something
immediately when the user switches threads, not stay blank until the
next token event fires. The `threads` row carries a
`last_token_usage TEXT` column (JSON blob of the last usage event for
that thread). Router updates it on every EventTokenUsage. On
switchThread, the frontend reads this directly from the thread row,
with no separate binding.

Compaction with no context snapshot does not emit usage and does not
clear `last_token_usage`; Codex emits a fresh token-usage update after
compaction, and the meter should keep the prior reading until that
arrives. If the provider includes a fresh context-window snapshot on
the compaction boundary, the router persists and emits that snapshot.

NOT in the chat history. Pure ambient indicator.

(Rate limits do NOT get UI in v1. If relevant, surface in the same
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

The frontend listens to these Wails channels for chat state.
Canonical Go payload structs; the frontend `app.ts` binding
regeneration surfaces them as the matching TypeScript discriminated
unions.

| channel                  | payload Go struct                          | purpose                                                |
|--------------------------|--------------------------------------------|--------------------------------------------------------|
| `provider:item_event`    | `triage.ItemStreamEvent`                   | ordered timeline upserts and live text/thinking deltas |
| `provider:approval`      | `ApprovalEvent` (discriminated, see below) | approval overlay state                                 |
| `provider:usage`         | `UsageEvent` (discriminated, see below)    | context meter; not displayed as items                  |
| `provider:status`        | `ProviderStatusEvent`                      | persistent provider banner                             |

**`ApprovalEvent`** is a discriminated union on `action`:
```go
type ApprovalEvent struct {
    Action    string              `json:"action"`    // "request" | "resolve"
    Request   *ApprovalRequest    `json:"request,omitempty"`
    RequestID string              `json:"requestId,omitempty"`
    Decision  string              `json:"decision,omitempty"` // approved|declined|amended|lost
}
```

**`UsageEvent`** is a discriminated union on `action`:
```go
type UsageEvent struct {
    Action                string              `json:"action"` // "usage" | "reset" | "rate_limits"
    ThreadID              string              `json:"threadId"`
    UsedTokens            int                 `json:"usedTokens,omitempty"`
    MaxTokens             int                 `json:"maxTokens,omitempty"`
    ContextPercent        float64             `json:"contextPercent,omitempty"`
    AutoCompactPercent    int                 `json:"autoCompactPercent,omitempty"`
    AutoCompactTokenLimit int                 `json:"autoCompactTokenLimit,omitempty"`
    RateLimits            *RateLimitsSnapshot `json:"rateLimits,omitempty"`
}
```

**`ProviderStatusEvent`** is a closed kind enum for banner behavior. The
wire shape is `providerstatus.Event` (`internal/providerstatus/providerstatus.go`);
a `threadId` scopes a kind to one pane, and `binary_stale` carries
`sessionVersion` / `installedVersion` for the restart banner.
```go
type ProviderStatusEvent struct {
    Kind    string `json:"kind"` // one of the values below
    Message string `json:"message,omitempty"`
}
// legal Kind values:
//   "binary_missing"         — provider CLI not installed / not on PATH
//   "unauthenticated"        — OAuth / API key missing or expired
//   "version_incompatible"   — installed CLI doesn't match expected version
//   "rate_limited_retrying"  — provider rate-limited, adapter is retrying
//   "transient_retry"        — adapter is retrying against a non-rate-limit
//                              cause (5xx, billing, invalid_request, etc.);
//                              Message carries the upstream reason verbatim
//   "binary_stale"           — thread-scoped: this thread's live session runs
//                              an older CLI than the installed binary; carries
//                              sessionVersion/installedVersion and clears via
//                              a thread-scoped status "ready" (or disconnect)
//   "ok"                     — transient issue resolved; frontend clears banner
```

Any `kind` value not in the closed set is dropped by the frontend
with a console warn. Adding a new kind requires updating the frontend
banner component in the same PR.

App-shell channels stayed as-is for this historical rewrite; later subsystem
retirements removed channels they owned. The toast channel for fatal infra
errors remains unchanged.

## Backend chokepoint

`internal/triage/router.go` collapses to one persistence function:

```go
// persistItem is the single chokepoint for any timeline state change.
// The store does the upsert (insert if id is new, update in place if
// id exists, preserving item_index). On success we emit the
// canonical item to the frontend.
//
// Non-bg-completion event handlers call persistItem directly.
// Bg tool-completion events go through maybeDeferOrPersist (below),
// which may queue or may call through to persistItem. Every actual
// write to the timeline goes through persistItem exactly once.
func (r *Router) persistItem(item store.Item, payload *store.Payload) error {
    persisted, err := r.store.UpsertItem(item, payload)
    if err != nil { return err }
    r.emit("provider:item_event", ItemStreamEvent{
        Action:   "upsert",
        ThreadID: persisted.ThreadID,
        Item:     &persisted,
    })
    return nil
}
```

**Why two functions, not one:** `persistItem` is the write+emit
chokepoint. `maybeDeferOrPersist` is a guard applied ONLY to bg
`tool_completion` events (see Streaming-phase interrupt queue below).
Other handlers bypass the guard and call `persistItem` directly
because their events don't need deferral. This keeps the guard from
becoming global policy that every handler has to reason about.

The existing `persistTurnText`, `persistHeavy`, `replaceHeavy`,
`insertHeavyItem`, `insertHeavyItemAndPayload`,
`persistFileChangeToolResult`, `persistCommandInlineDiffToolResult`,
`persistToolResult`, `persistToolCallLaunch`,
`persistToolCallCompletion`, `upgradeSummaryOnlyToolResults` are all
deleted. Their work folds into per-event handlers that build an Item
and call `persistItem`.

### Streaming-phase interrupt queue

Backgrounded tool completions defer to a per-thread FIFO queue when
a streaming item is active. Drained in order when the streaming item
settles. Named after Codex's `InterruptManager`
(`codex-rs/tui/src/chatwidget/interrupts.rs`), which implements
the same pattern.

```go
type Router struct {
    ...
    // interruptQueue holds bg tool_completion items (plus their
    // attached payloads) that arrived while a streaming item was
    // active in the same thread. Drained FIFO on segment-complete
    // or turn-complete. Named after Codex's InterruptManager.
    interruptQueue map[string][]pendingCompletion
}

type pendingCompletion struct {
    Item    store.Item
    Payload *store.Payload
}

func (r *Router) maybeDeferOrPersist(item store.Item, payload *store.Payload) error {
    if item.Kind == "tool_completion" && item.IsBackground {
        if r.hasActiveStreamingItem(item.ThreadID) {
            r.interruptQueue[item.ThreadID] = append(
                r.interruptQueue[item.ThreadID],
                pendingCompletion{Item: item, Payload: payload},
            )
            return nil
        }
    }
    return r.persistItem(item, payload)
}

// Called on segment-complete AND on turn-complete. FIFO order
// preserves Begin/End pairing for parallel background tools.
func (r *Router) drainInterruptQueue(threadID string) {
    queue := r.interruptQueue[threadID]
    delete(r.interruptQueue, threadID)
    for _, p := range queue {
        r.persistItem(p.Item, p.Payload)
    }
}
```

**Crash safety (provider-dependent)**:
- **Claude**: `--resume <session-ref>` replays the full session log
  including tool_results, so queued-but-lost completions re-fire
  after resume. The queuing re-applies cleanly. Worst case: the
  user sees the launch row as `running` until resync delivers the
  completion event again.
- **Codex**: `thread/resume` returns a snapshot, NOT an event
  replay. If a queued completion was dropped pre-persist (app
  crashed between EventToolComplete and SQLite write), Codex will
  NOT re-emit it. The launch row stays `running` after resume
  and the standard crash-recovery flow (`thread/read` probe + flip
  to errored if no longer live) applies. This means for Codex
  specifically, a pre-persist crash during the interrupt queue's
  hold window can permanently lose the real completion outcome.
  The row will end up errored with summary "Interrupted — outcome
  unknown" rather than reflecting the actual completion. Accept
  this as pre-release scope; if load-bearing later, persist queue
  entries to a `pending_completions` side table before queuing.

### Turn lifecycle synthesis

Turn boundaries enter the Go triage layer through provider-specific
signals. `App.sendMessage` persists the user item and registers a
pending-send marker before writing to the provider. Claude opens the
logical turn when its wire `system/init` arrives and `handleInit`
matches that marker; Codex opens the turn from native `turn/started`.
Both paths use the same local `turn_index` semantics so item grouping
stays provider-independent.

**`turn_index` semantics**: this is a LOCAL counter on agent-overflow's
thread, not a provider turn id. Provider turn ids (Codex's
`turn.id`, Claude's per-cycle identifiers) are opaque strings we
don't mint. `turn_index` counts "send cycles" on our thread, one
per `sendMessage` call. The two can diverge (e.g., Claude may
internally loop through multiple API cycles under one of our
turns), and that's fine: our `turn_index` is the UI unit, the
provider's turn id is the wire unit.

**TurnStart**: for Claude, emitted from `handleInit` when wire
`system/init` matches a pending-send marker; for Codex, emitted from
the `turn/started` wire notification after the user item has been
persisted:

1. Acquire the per-thread action lock (`threadActionLocks`).
2. If the thread has no prior items, `turnIndex = 0`; otherwise
   `turnIndex = store.LastTurnIndex(threadID) + 1`.
3. Register the pending-send marker before writing to provider stdin.
   Claude's next `system/init` routes through `handleTurnStart`; Codex's
   native `turn/started` event falls back to the already-persisted user
   item's `turn_index`.
4. Router handler: reset `segmentIndexByScope[threadID][turnIndex][""] = 0`,
   open turn telemetry span, capture git baseline (existing
   `handleTurnStart` behavior).
5. Persist the `user_text` item with that `turn_index`.
6. Forward content to provider; all subsequent stream events under
   this turn.

**Send failure rollback.** This reverses the current `Bug B8`
invariant in `app_send.go` which sends to provider FIRST, then
persists, to avoid orphan user_text on send failure. The new model
intentionally inverts: persist user_text first so the user sees
their message immediately and knows it was captured. If
`provider.Send` then fails:
- Upsert a system `error` item in the same turn with summary
  "Failed to send: <error>".
- Synthesize `EventTurnComplete` with
  `provider.TruncatedTurnCompleteMeta` so the turn lifecycle closes
  cleanly and the frontend exits `isTurnActive`.
- `turn_index` stays monotonic; the next send gets `turn_index+1`.

Rationale for the inversion: silent-disappearing-user-message
(current behavior on send failure) is worse UX than "message
visible + error clearly attached." The user knows what happened;
no mystery. The "orphan" scenario the old rule avoided is not an
orphan in the new model. It's a visible turn with a clear error
item, which is exactly what the user needs.

The existing router `handleTurnStart` is idempotent on
`(threadID, turnIndex)`. If a provider re-sends TurnStart for the same
logical turn (for example a Claude interrupt / re-init path), the
duplicate is silently absorbed.

**TurnComplete**: wire signal primary, with fallbacks:

| source | signal | role |
|---|---|---|
| Claude wire | `{type:"result"}` message | primary: the Claude adapter's `parseResult` handler already emits `EventTurnComplete` |
| Codex wire | `turn/completed` notification (carries `turn.status: completed \| interrupted \| failed`) | primary: the Codex adapter's notification handler already emits `EventTurnComplete` |
| Both | provider process exit while a turn is open (TurnStart seen, no TurnComplete yet) | session lifecycle handler synthesizes `EventTurnComplete` with `provider.TruncatedTurnCompleteMeta` before session teardown |

Tracking "is a turn currently open" in the router: a simple
`openTurns map[threadID]turnIndex` cleared on TurnComplete. Set on
TurnStart. Used to decide whether the idle-fallback and
process-exit-fallback fire.

On any TurnComplete, the router:
1. Drains the streaming-phase interrupt queue for this thread.
2. Flips any still-`streaming` items in this turn to `completed`
   (text and thinking settle).
3. Closes the turn telemetry span.
4. Emits `EventTurnComplete` to the frontend for working-indicator
   and context-meter updates.

When `TurnComplete` is `provider.TruncatedTurnCompleteMeta`, the router
additionally:
5. Marks any still-`streaming` or `running` items in this turn as
   `errored` with summary suffix " — interrupted" (same rule as the
   live-crash flip below).
6. Drains queued completions to `errored` items rather than
   `completed`, since we don't know the real outcome.

### Per-event handler logic

| event                       | handler                                                                                               |
|-----------------------------|-------------------------------------------------------------------------------------------------------|
| `EventTextDelta`            | upsert `assistant_text`; id = `text:<turnId>:<segmentIndex>`; append delta to summary; status=streaming |
| `EventThinking`             | upsert `thinking`; id = `think:<turn_index>:<block_index>` (block_index monotonic across turn, not per-API-cycle); append delta to summary; preserve `signature` on payload/meta for API replay |
| `EventToolStart`            | upsert `tool_call`; id = evt.ItemID; status=running; `is_background` only from provider wire facts (Claude `run_in_background` / Task tracking; Codex triage projection from `unifiedExecStartup` and `spawn_agent` child state); tool_name set |
| `EventToolComplete` inline  | upsert same `tool_call`; status=completed/errored (errored only when is_error=true); attach payload  |
| `EventToolComplete` bg      | route through `maybeDeferOrPersist` for the new `tool_completion`; summary restates command          |
| `EventDiff`                 | find/create the related `tool_call`, attach diff as its payload                                       |
| `EventCommandOutput`        | find/create the related `tool_call`, attach command output as its payload                             |
| `EventProposedPlan`         | upsert a `tool_call` with tool_name="plan"; payload carries the plan markdown                        |
| `EventError`                | upsert `error` item; id = uuid; summary = error message. ALSO: flip any `streaming`/`running` items in this turn to `errored` (live-crash flip) |
| `EventCompactBoundary`      | upsert `compaction` item; id = provider id or per-turn sequence; summary = compaction note. ALSO: emit included context snapshot if present |
| `EventTurnStart`            | from provider wire turn-start signal (Claude `system/init` matched to a pending send, Codex `turn/started`); reset `segmentIndexByScope[threadID][turn_index][""]` and `blockIndexByScope[threadID][turn_index][""]`; open turn span; mark turn open; no item |
| `EventTurnComplete`         | from wire signal or typed synthesis (see Turn lifecycle synthesis); flip `streaming` items in this turn to `completed`; drain `interruptQueue`; close turn span; clear open-turn marker; if `TurnComplete` is `provider.TruncatedTurnCompleteMeta`, flip streaming to `errored` and drain queue as `errored` |
| `EventApprovalRequest`      | emit `provider:approval` `{action:request, request}`                                                  |
| `EventApprovalResolved`     | emit `provider:approval` `{action:resolve, requestId, decision}`; upsert the underlying tool_call with `decision` set |
| `EventInit`                 | persist session_ref to thread; no item                                                                |
| `EventThreadRenamed`        | persist title to thread; emit existing `thread:*` event; no item                                      |
| `EventModelRerouted`        | persist model to thread; emit existing event; no item                                                 |
| `EventModelFallback`        | persist warning notification; project session-scoped effective model; keep requested model unchanged |
| `EventTokenUsage`           | emit `provider:usage` (for the meter)                                                                 |
| `EventRateLimits`           | emit `provider:usage` (folded in)                                                                     |
| `EventSessionStatus` (persistent failure) | emit `provider:status` for the banner (binary missing, auth fail). Transient ones are dropped. |
| `EventBackgroundStart/Delta/Complete` | dropped: superseded by tool_call lifecycle with is_background classifier                    |
| `EventToolProgress`         | dropped: progress is just successive upserts of the tool_call's summary                              |
| `EventPlanUpdate`           | dropped: final plan arrives via EventProposedPlan                                                    |

The `provider.AllEventKinds` list shrinks accordingly.

### Live provider-crash flip

Triggered by any of:
1. **Subprocess exit** while a turn is open: session lifecycle
   observes the exit and calls the handler.
2. **Stream closed unexpectedly** (stdout EOF before turn-complete):
   the reader loop detects and calls the handler.
3. **`EventError` with `meta.fatal == true`**: the emitting adapter
   sets the fatal flag to signal "this error ended the turn."
   Non-fatal errors (transient tool failures,
   Codex `turn/completed{status:"failed"}` with a recoverable-ish
   error) do NOT flip streaming items; they create an `error` item
   and let the turn's own completion signal land.

**Fatal discriminator rule for adapters**: set `meta.fatal = true`
when the error means "no further events will arrive for this turn."
Examples: provider crash, permission denial that aborts the turn,
out-of-context compaction failure. Do NOT set fatal for: a single
tool erroring, a refused approval (the turn continues), rate-limit
retries.

**Strict ordering in the handler:**
1. First: flip every `status=streaming` and `status=running` item in
   the active turn to `errored` with summary suffix " — interrupted".
   Each flip emits its own `provider:item_event` upsert.
2. Then: upsert the `error` item (new row).
3. Finally: drain any pending queued completions to `errored`
   items (not `completed`, since we don't know the real outcome).
4. Synthesize `EventTurnComplete` with
   `provider.TruncatedTurnCompleteMeta` if no wire TurnComplete is
   expected (subprocess exit case). Not needed for fatal EventError on
   an otherwise-alive session.

The ordering matters for the frontend render: by the time the error
item appears, every streaming/running item is already visibly flipped
to errored. No "still-streaming text next to an error item" visual
state.

Without this, the user sees a streaming text item that never
finishes, sitting next to the new error item, which is confusing. The flip
makes the broken state explicit on the items themselves.

### Store changes

Add `UpsertItem(item Item, payload *Payload) (Item, error)`:

- If `item.ID` exists: update kind/role/summary/payload_id/status/
  is_background/completion_of/parent_id/tool_name/decision/meta/
  updated_at. Item index is preserved.
- Else: assign next item_index for `(thread_id, turn_index)`, insert.
- If payload non-nil, upsert payload in same transaction.
- Returns the canonical persisted Item (with assigned item_index).

Add `GetPayloadPreview(threadID string, payloadID string, maxBytes int) (data []byte, totalSize int, err error)`:
- Reads the full payload blob but returns only the head up to maxBytes
  (accepting the 4MB-cap as the worst case). Also returns total size
  so the frontend can render the "show full (N KB)" button.

The single-writer connection pool (`SetMaxOpenConns(1)`) keeps the
upsert race-free.

**Schema v15 migration:**

Additive columns:
- `items.tool_name TEXT NOT NULL DEFAULT ''`
- `items.decision TEXT NOT NULL DEFAULT ''`
- `items.meta TEXT NOT NULL DEFAULT '{}'`: JSON blob for
  per-item provider-specific metadata that doesn't fit the
  column model. Used for: Claude thinking `signature` (for API
  replay), Claude `task_id ↔ tool_use_id` mapping (for bg
  completion correlation across reconnect), Codex subagent
  `receiverThreadId` (for resubscribing children on reopen),
  denormalized live subagent activity (`latestActivityText`,
  `latestActivityAt`). Structured enough to query specific keys
  via `json_extract` when needed, flexible enough to avoid
  column churn per provider detail.
- `threads.last_token_usage TEXT NOT NULL DEFAULT ''` (JSON blob of
  the last EventTokenUsage meta for that thread; seeds the context
  meter on thread switch)

Column renames (for provider-neutral vocabulary):
- `items.parent_tool_use_id` → `items.parent_id` (now used by both
  Claude subagents and Codex's mapped subagent items)
- `items.completion_of_item_id` → `items.completion_of`

**Data: wipe existing items and payloads on migration to v15.**
The app is pre-release; historical chat content has no user value
worth preserving through this schema change, but `threads` carry
session refs, workspace/project identity, runtime settings, and
recovery cursors we DO want to keep. The migration:
- `DELETE FROM items`
- `DELETE FROM payloads`
- (leaves `threads`, `projects`, and all other tables intact)

On next thread open, the user sees an empty chat but their thread
list, worktree bindings, and settings are preserved. They start
fresh content against familiar threads rather than losing the
thread registry entirely.

This avoids the complexity of a live data migration for
kind-splits, `background_started/done` → `tool_call`/`tool_completion`
rollups, and the column renames happening in one shot.

No index changes (new columns aren't query targets; the renamed
columns keep their existing indexes under the new names).

`InsertItem`, `AppendItem`, `AppendItemWithPayload`, the various
narrow updaters are kept for now, but most callers migrate to
`UpsertItem`. Audit at end of execution and delete unused.

## Frontend collapse

### Pane state (final shape)

```ts
items: Item[]                    // sorted by (turn_index, item_index)
pendingApprovals: ApprovalRequest[]
contextWindow: ContextWindowMeta // for the meter widget
providerBanner: ProviderBannerMeta | null  // persistent provider error
thread: Thread | null
loading: boolean
// design + discussion + diff-panel + terminal state stay as-is
```

**Deleted:** `activeToolCalls`, `streamingContent`, `backgroundTasks`,
`pendingPlanUpdate`, `pendingMessage`, `dismissedPlanItemId`,
`payloadMetas`, `tokenUsage` (replaced by `contextWindow`),
`rateLimits`, `sessionApprovedTools`, `error`, `sessionStatus`.
Everything that was a parallel state stream goes away.

**No payload cache.** Expanded content lives in the dropdown's own
component-local `$state` and is discarded on collapse. Peek re-fetch
on re-expand is ~5ms over IPC (32KB default), which is imperceptible
and guarantees no accidental unbounded memory growth from accumulating
expanded rows.

**One mutation:** `upsertItem(item: Item)`. Replaces by id (preserves
position) or inserts at sorted position. That's the only timeline
mutation.

### Sending a message: no optimistic shadow

User clicks send → `SendMessage` binding runs → app_send.go persists
the `user_text` item and emits a `provider:item_event` upsert. The round trip
is SQLite write + Wails event ≈ <10ms on local hardware. The user
sees their message appear without perceivable lag. No `pendingMessage`
optimistic render needed.

If profiling ever shows jank here, reintroduce an optimistic shadow
AFTER measuring, not speculatively.

### Multi-message queueing: no queue

The user cannot send while `isTurnActive === true`. The Composer's
send button is replaced by an Interrupt button (existing behavior).
There is no message queue; if the user wants to redirect, they
interrupt the current turn first, then send.

Single-message-in-flight invariant matches Claude Code CLI, Codex,
and every reference we inspected. Queueing is not a feature we owe.

### Composer approval prompt

New component: `ComposerApprovalPrompt.svelte`. Reads
`pane.pendingApprovals[0]` (the head of the queue if there are
multiple, which shouldn't normally happen). Renders the approval
panel in the composer footer in the style of t3-code's
`ComposerPendingApprovalPanel`:

- Header: tool name + brief input preview (pulled from the approval
  request payload).
- Actions: Approve / Deny / (if supported) Approve for session /
  Approve and modify.
- Calls `RespondToApproval(threadID, response)` binding on click.

While a pending approval exists, the standard send input is replaced
by this panel. On resolve, the panel disappears and the send input
returns.

### Listeners

```ts
on('provider:item_event', (event) => {
  if (event.action === 'upsert') pane.upsertItem(event.item);
  else pane.applyItemDelta(event.delta);
});
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
  return (
    items.some(i =>
      (i.kind === 'assistant_text' || i.kind === 'thinking') && i.status === 'streaming'
      || i.kind === 'tool_call' && i.status === 'running' && !i.isBackground
    )
    || pendingApprovals.length > 0
  );
}
```

Note: backgrounded tool_calls do NOT count as "turn active". They
run independently and shouldn't block sends. Pending approvals DO
count: the turn is waiting on the user's decision, send should be
blocked until they respond. Codex also exposes `waitingOnApproval` /
`waitingOnUserInput` thread states; those are reflected in
`pendingApprovals` via the normal approval flow. Used by Composer to
gate sends + show interrupt button.

### finalizeTurn / switchThread

`finalizeTurn` deleted. The stream is authoritative; nothing to drain.

`switchThread` keeps its initial hydration via `ListItems`. The
context meter seeds from the thread row's `last_token_usage` column
(read alongside the thread itself, with no separate binding). After
hydration, the upsert + usage streams are the only mutation sources.
Nothing to clear on switch. There is no cache.

### MessageTimeline

```svelte
<VList data={groupItemsBySubagent(filterRedundantNotifications(pane.items))}>
  {#snippet children(node, index)}
    {#if node.kind === 'group'}
      <SubagentGroup group={node} {pane} />
    {:else}
      <TimelineLeaf item={node.item} {pane} />
    {/if}
  {/snippet}
</VList>
```

Where:
- `groupItemsBySubagent` only groups Claude foreground Agent/Task rows
  (detected by toolName). Generic `parentId` does not create frontend
  topology.
- `TimelineLeaf` is the item-kind dispatcher for ordinary rows.
- `SubagentGroup` owns only the grouped card presentation; its expanded
  state is stored on `ThreadPane` by `SubagentGroupNode.groupKey`.
- Disclosure rows compose `TranscriptDisclosureHeader` so the chevron,
  toggle button, and trailing actions keep a stable DOM shape as payloads
  and completion metadata arrive.

### ToolCallCard internals

- Header line: per-tool-kind dispatcher on `tool_name` / `payloadKind`.
  Shows icon, label, brief input preview, status badge, decision chip
  (if set), and stable trailing metadata.
- Chevron expand: uses `pane.expansionStateFor(item)` /
  `pane.expansionStateForPayload(...)` to fetch preview chunks via
  `GetPayloadPreview(threadId, payloadId, 32768)`. If `!isComplete`,
  show a "Show full output (N KB) ↓" footer button. Button click loads
  chunks with `GetPayloadChunk(threadId, payloadId, nextOffset, maxBytes)`.
- Body switch on `payload.kind`: command_output / diff / tool_result /
  proposed_plan / text. `command_output` and any text-style payload
  renders as raw text through the frontend ANSI renderer.
- Expanded payload content is held in pane-level expansion registries,
  not component-local state, so virtua remounts do not reset toggles or
  refetch loaded chunks. Collapse and thread switch clear the data.
- Children (if subagent): rendered by `SubagentGroup` when the card is
  expanded. Groups are collapsed by default and are created for
  foreground Agent/Task tool calls.
- **Grandchild depth cap**: a subagent card at depth 1 whose children
  include another subagent (grandchild at depth 2) renders the
  grandchild as a minimal marker: name + spawn prompt only, not
  its full conversation. Grandchild's own children are not
  subscribed to. Keeps visual complexity bounded and avoids
  recursive subscription explosion for rare deep nesting.
- **Card header while collapsed**: always shows live status. For a
  running subagent card: `<name> · <N items> · <elapsed> ·
  <latestActivity>`. The renderer derives all three from the card's
  fields (latestActivity is denormalized by the adapter as child
  events arrive).

### Background tray

Backed by `ListLiveBackgroundTasks(threadId)`, not the loaded timeline
window. Renders running background launches plus pairs whose
`tool_completion` is younger than 2s. Cap at 3 visible rows +
"+N more". Tray rows are informational; the launch and completion
also render inline in chat history.

Stop controls follow provider capabilities. Both providers get the same
two affordances over different primitives, resolved by one helper
(`trayRowStopTarget`) so a row and the bulk button can never disagree:

- **Claude**: per-row Stop when the launch meta carries `task_id`
  (`StopClaudeTask`); Stop-all fans the same call out per id.
- **Codex**: per-row Stop when a yielded unified-exec PTY carries
  `process_id` (`TerminateCodexBackgroundTerminal` →
  `thread/backgroundTerminals/terminate`, available since codex 0.140,
  below AO's provider floor); Stop-all is the single thread-wide
  `thread/backgroundTerminals/clean`. The terminate response's
  `terminated: false` means "matched nothing" and surfaces as an info
  toast, because no `item/completed` follows to change the row.
- **Codex subagent rows** have no stop control in either place:
  `close_agent` is a model tool with no client path.

A not-yet-yielded Codex command is tray-visible but not stoppable: it is
not a background terminal yet, so neither primitive can reach it.

### Working indicator

A small footer component (`ChatWorkingIndicator.svelte`) below the
Composer. Pure derivation:

```ts
let isWorking = $derived(pane.activeTurn !== null);
let elapsed = /* computed from pane.activeTurn.startedAt */;
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
   always up-to-date through the last event (queued completions
   excepted; see Streaming-phase interrupt queue).
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
5. If `pendingApprovals` were active at crash time: approval state
   is always lost on restart. No provider we support re-emits.
   Affected tool_calls flip to `errored` with `decision=lost`. The
   question (tool name + input preview) is preserved in `summary`.

**Provider replay caveat:** Claude's session log persists tool_result
blocks and replays them on `--resume`, so a deferred completion
lost-before-persist is recoverable via the resumed session. Codex's
app-server behavior on this point is not verified; worst case is the
completion is permanently lost. The recovery rules above handle that
cleanly: a `running && is_background` row on reopen with no Codex
replay gets flipped to errored/lost. No deeper Codex-specific handling
needed; the crash recovery semantics are identical regardless of
whether replay works.

The recovery contract: **what's in SQLite is what the user sees**.
Provider session state may be ahead, behind, or equal. None of those
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
- `pane.payloadMetas`, `addPayloadMeta`, `touchPayloadMeta` (payload previews/data held in component-local `$state`, discarded on collapse, no cache)
- `pane.tokenUsage`, `setTokenUsage` (replaced by `pane.contextWindow`)
- `pane.rateLimits`, `setRateLimits`
- `pane.error`, `setError`, `clearError`. Audit resolved: renamed to
  `pane.generalError` / `setGeneralError` / `clearGeneralError` so the
  slot's grab-bag purpose (thread-load / composer send / git action
  failures) is distinct from the wire-level `providerBanner`.
- `pane.sessionStatus`, `setSessionStatus`
- `pane.finalizeTurn`
- `pane.sessionApprovedTools`, `addSessionApprovedTool`,
  `isToolSessionApproved`: approval state is now per-tool_call via
  `decision`, no per-session allowlist
- `pane.turnGeneration`: counter only existed to guard
  `finalizeTurn` async races; with finalizeTurn gone it has no callers
- `pane.appendTextDelta`: accumulator pattern replaced by streaming
  item upserts
- `frontend/src/lib/components/chat/StreamingMessage.svelte`
- `frontend/src/lib/components/chat/ProviderStatusBanner.svelte`
  (rewrite to consume `pane.providerBanner` instead of session status)
- `frontend/src/lib/components/chat/RateLimitsMeter.svelte`
- `frontend/src/lib/components/chat/ChangedFilesTree.svelte`: REMOVED;
  stable transcript rendering superseded inline end-of-turn diff cards.
- `frontend/src/lib/components/chat/TurnDiffBadge.svelte`: REMOVED;
  stable transcript rendering superseded inline end-of-turn diff cards.
- `frontend/src/lib/components/chat/CommandOutput.svelte`,
  `DiffPreview.svelte`, `ProposedPlanCard.svelte`,
  `ThinkingBlock.svelte`, `ToolResultCard.svelte`: fold into
  `ToolCallCard`'s payload renderer (one component per payload kind
  surviving)
- `frontend/src/lib/stores/events.ts`: the giant switch over `evt.kind`
  in `routeEventToPane` collapses to four listeners

### Frontend keeps & adds

- KEEP: `BackgroundTaskTray.svelte` (filter logic stays; remove any
  per-row stop button, since stopping goes through the global Composer
  Stop button per the Stop control section)
- KEEP: `ToolResultDropdown.svelte` (becomes the payload renderer
  wrapped in ToolCallCard)
- KEEP: `MessageTimeline.svelte` (rewritten body, same shell)
- KEEP: `UserMessage.svelte`, `AssistantMessage.svelte` (rename to
  AssistantText if needed)
- KEEP: `SubagentGroup.svelte` (becomes part of ToolCallCard recursion)
- ADD: `ChatWorkingIndicator.svelte` (footer)
- ADD: `ContextWindowMeter.svelte` (composer toolbar)
- ADD: `ToolCallCard.svelte` (the per-kind header + payload dispatcher
  + child recursion)
- All composer components, sidebar, terminal drawer, and discussion view:
  untouched by this historical rewrite
- UNTOUCHED (explicitly out of scope for this rewrite):
  `PlanFollowUpBanner.svelte`, `PlanSidebar.svelte`,
  `LazyContentBlock.svelte`, `ChatHeader.svelte`, `ChatView.svelte`,
  `WorkEntry.svelte`, `DiffPanelDrawer.svelte`. If any of these
  reference pane state that's being deleted (e.g.,
  `pane.pendingPlanUpdate`), they need a minimal adaptation to read
  from the new `items` stream. Adapt in place, don't rewrite. The
  rewrite is about data flow, not these UI shells.

## Execution plan

One sequential pass. Each step ends with green tests.

1. **Schema v15**: `tool_name` and `decision` columns. Tests.
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
   pass + `drainInterruptQueue`.
6. **Delete obsolete event kinds** from `provider.AllEventKinds` and
   provider adapters that emit them.
7. **Frontend pane rewrite**: single PR-size chunk. Delete the state
   slices listed above. Implement `upsertItem` semantics, the four
   listeners, contextWindow state, providerBanner state. Update
   `isTurnActive`. Delete `finalizeTurn`. No payload cache: previews
   live in component-local state, discarded on collapse.
8. **MessageTimeline rewrite + ToolCallCard**: new switch, per-kind
   header dispatch, payload renderer dispatch, subagent recursion
   with depth cap, scroll invariants.
9. **Working indicator + Context meter**: new components, wire to
   pane state.
10. **Background tray polish**: "+N more" cap and provider-specific
    stop controls.
11. **Tests**. Integration test for the full flow: provider events
    in, single upsert stream out, frontend renders correctly with no
    shifts at turn boundary, crash recovery (kill mid-turn, reopen,
    verify items intact).

## Acceptance criteria

- During a turn, every observable state change is visible in the
  timeline the moment it happens (at its post-streaming-boundary
  position). No state change is hidden or buffered until turn-complete
  The current bug where thinking blocks and some text only appear at
  turn end is eliminated.
- At turn-complete, the timeline does NOT shift, reorder, or surprise.
  Items already on screen stay where they are; new items append.
- Background completions appear after the active streaming item
  settles, never mid-stream.
- Killing the app mid-turn and reopening shows exactly what was
  persisted. Items in `streaming`/`running` state on reopen transition
  per the recovery rules; user sees explicit "interrupted" markers
  if the session can't be resumed.
- Backgrounded tool calls show one launch row inline + one completion
  row at the post-streaming-boundary position + tray mirror while
  running. Stopping a running turn uses the global Composer Stop
  button, which calls `turn/interrupt` (Codex) or the Claude adapter's
  interrupt path.
- Stop is a targeted turn interrupt, not a whole-tree kill. It
  matches each CLI's native semantics. After Stop: (a) parent's
  streaming text/thinking flip to errored immediately; (b) Codex
  subagent cards, Codex running exec processes, backgrounded items
  on both providers, and Claude async Task cards KEEP RUNNING; (c)
  Claude sync inline Task subagents and non-Task inline tool_calls
  die via the shared abort controller and flip naturally through
  the wire's tool_result stream.
- Parallel Codex tool calls (two or more `FunctionCall`s emitted
  concurrently) correlate by `call_id`; Begin/End events never render
  a completion before its launch even when the channel interleaves
  them. If both finish while assistant text is streaming, they drain
  FIFO from the interrupt queue after the text settles.
- Claude turns close on `{type:"result"}` primary. Provider process
  exit with a turn open synthesizes a typed truncated TurnComplete and
  flips streaming items to errored.
- Subagents render as one `tool_call` card per subagent in the
  parent's timeline. Card header shows live count + elapsed +
  latest activity; expansion shows the child's full conversation
  (text, thinking, tool_calls). Grandchildren render as marker-only
  (spawn description, no further expansion).
- Approvals resolve into a `decision` chip on the underlying
  tool_call, never a separate item. Decision history is preserved
  for scroll-back.
- Working indicator visible during turn activity. Context window
  meter visible at all times. No session status UI; persistent
  provider failures use a banner.
- Bash exit codes don't flip status to errored. Only is_error does.
- No `pane.activeToolCalls`, no `pane.streamingContent`, no
  `pendingMessage`, no `payloadMetas` map. Grep returns zero hits.
- Frontend `pnpm run check` clean. `make go-test` green.
- Integration test covers: send message → tool call → tool result →
  assistant text → turn complete → reload thread → verify identical
  render. Plus: backgrounded command → user sends new message →
  completion lands at next agent boundary.

## Anti-patterns to avoid (from Claude Code CLI source)

Research against Claude Code's decompiled CLI source (available in
`~/repos/claude-code-source-code` at research time) surfaced
structural bugs we must not regress to during execution. Each pattern
below is something Claude Code's internal renderer does that costs it
correctness or performance; none of them belong in the new
implementation. These are principles, not ticket-tracked claims. If
the citations below rot (different Claude version, different line
numbers), the principles still hold.

1. **`parentUuid` linked-list persistence.** Claude persists
   conversation history as a parent-chain per message. Parallel
   tool_uses produce a DAG the linear walk drops, requiring a
   `recoverOrphanedParallelToolResults` repair sweep (~100 lines) at
   load time. Our `(turn_index, item_index)` integer ordering makes
   the whole class of bug impossible. Stay with integer ordering.
   Never persist a chain.

2. **Synthetic in-memory placeholder items with random UUIDs.**
   Claude's renderer builds `syntheticStreamingToolUseMessages` on
   every frame using `randomUUID()`, then patched to `deriveUUID`
   after the unstable keys caused Ink remounts and overlapping text
   corruption. Our stream claims a stable `item_id` at stream start
   and upserts under that id: no placeholders, no key churn.

3. **Per-render reordering (`reorderMessagesInUI` on every paint).**
   Claude re-pairs tool_use + tool_result at every render because the
   wire can land them in any order. We sort once at the data layer
   by `(turn_index, item_index)`. Never re-sort on render; never
   post-process the items array in the view.

4. **Multiple parallel state slices during streaming.** Claude's
   REPL carries `streamingText` + `streamingToolUses` +
   `streamingThinking` + both synced `messages` and
   `useDeferredValue`-throttled `deferredMessages` simultaneously.
   Our spec deletes all of these into one `items` list with
   in-place mutation. The "Deleted" list under Pane state must
   stay deleted: no re-introducing parallel streams for
   "streaming preview" performance.

5. **`useDeferredValue` ping-pong to hide mid-frame tearing.**
   Fragile; relies on React batching to land two state updates in
   the same frame. Tears when batching fails. Our stable-id upsert
   makes the streaming → completed transition idempotent in a
   single frame, with no handoff to engineer around.

6. **String-matching XML-like tags in message content** to detect
   structured output (Claude does this with `<bash-stdout>` /
   `<bash-stderr>`). We use typed payload fields and kinds. Never
   parse content bodies to recover structure.

7. **Count-based slicing with UUID anchoring for virtualization.**
   Claude caps non-virtualized render at a fixed window, using UUID
   anchors to slice. This has produced bugs where messages disappear
   when the anchor moves. If virtualization is needed, virtualize
   the whole list. Never count-slice.

8. **Unmemoized re-renders of large message arrays.** Claude's
   render path reallocates several Maps over the full message list
   on every scroll, producing long GC pauses on their large heap.
   Svelte 5's fine-grained reactivity avoids most of this, but the
   timeline component must not subscribe to the full items array on
   unrelated updates. Each `ToolCallCard` should re-render only on
   its own item's upsert, not on sibling updates.

## What this does NOT cover

- Visual polish toward a denser chat layout. Separate pass after this
  lands. The data model is the prerequisite.
- The 1GB memory issue. Separate investigation; suspects are
  eager-loaded syntax highlighter languages and Wails webview baseline.
  Won't be fixed by this rewrite (or made worse).
- Workflow / phase / gate system, remote/web access, auto-updater,
  mid-turn correction: already-deferred items in `AGENTS.md`. This
  rewrite doesn't touch them.
