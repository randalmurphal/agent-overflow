# Turn / Tool / Task Lifecycles

The single source of truth for how agent-overflow models the three
independent lifecycles that govern a chat turn. Read this before
touching `internal/provider/{claude,codex}/`, `internal/triage/`, or
any frontend code that reads `pane.activeTurn` / tool-call state.

## The three lifecycles

Provider output is governed by three **independent** lifecycles. Each
has its own identity, its own terminal signal, and its own
persistence target. Conflating them is the root cause of most chat-UI
bugs (rows stuck at `running`, "Working…" pinned forever, turns that
never complete).

| Lifecycle | Identity | Terminal signal | Storage |
|---|---|---|---|
| **Tool** | `tool_use_id` | provider `tool_result` / `item/completed` | `items` row, `kind="tool_call"` |
| **Task** | `task_id` (Claude only) | `task_updated` terminal OR TaskOutput | `items` row, `kind="tool_completion"`, `completion_of=<launch_id>` |
| **Turn** | `turn_id` | provider `result` / `turn/completed` | `turns` row |

The rules below say when each lifecycle fires, what owns its state,
and how the signals interact.

## 1. Tool lifecycle

**One-to-one with every `tool_use` on the wire.** Every tool
invocation the agent makes produces exactly one `tool_call` row.

### Invariant (load-bearing)

> Every `tool_use` emits exactly one `EventToolStart` and exactly one
> `EventToolComplete`, both keyed by the tool's own `tool_use_id`.
> No ID rewriting between start and complete. No consumption by other
> event handlers. No skipping.

### Applies equally to

- Inline tools (Read, Grep, Edit, inline Bash) — completion carries
  the exit/stdout result.
- Backgrounded Claude tools (Bash with `run_in_background:true`,
  Task subagent) — completion is the **placeholder** tool_result
  (`backgroundTaskId: ...`); actual task result lands via the task
  lifecycle (below).
- TaskOutput — a regular inline tool. Its own `tool_use_id`'s
  completion is the `retrieval_status` result. The task-lifecycle
  enrichment it triggers is a **separate** event (see
  [§Task lifecycle](#2-task-lifecycle-claude-only)).
- Codex tools (shell, mcp, fileChange, collab spawn/wait/resume/close)
  — all complete via `item/completed`.

### Status flipping on completion

| Backgrounded? | Placeholder behavior | Launch row status after completion event |
|---|---|---|
| No (inline) | N/A | `completed` / `errored` |
| Yes (Claude Bash or Task) | Placeholder `tool_result` carries `backgroundTaskId` | Stays `running` per spec invariant. Sibling `tool_completion` row arrives later via task lifecycle. |

This per-spec exception exists so the timeline can render both
"agent dispatched this tool" and "the actual work that got done" as
two historically accurate rows. The launch row's `status='running'`
+ `is_background=true` is the render signal for the `"…"` badge
(see [chat-rewrite.md §Background tray](chat-rewrite.md)); it is not
a claim that the tool is currently executing.

### Codex does NOT have backgrounded tools

Codex's `spawn_agent` is a regular inline tool — it completes
immediately with `item/completed` once the child thread is created.
The child thread runs independently but on a different `thread_id`.
The sibling-row and `BackgroundTaskTray` model does NOT apply to
Codex. Codex's `BackgroundClassifier` must not stamp `is_background`
on any item.

### Force-close safety net (turn-complete)

When a turn ends, triage force-closes any `tool_call` rows with
`status='running' && !is_background && turn_index=currentTurn` to
`status='errored'` with a synthesized completion. This handles
provider bugs where a `tool_result` is dropped. Backgrounded
launches are exempt — they legitimately stay `running`.

## 2. Task lifecycle (Claude only)

Backgrounded tasks produce a **separate** event stream keyed by
`task_id`. This is strictly additive — it layers task-completion
details on top of the tool-call row; it never replaces the tool
lifecycle.

### Participants

- `system/task_started` — mirrors the mapping `task_id ↔ tool_use_id`
  into `items.meta.task_id` so reconnect can correlate by task_id
  alone.
- `system/task_updated` with `patch.status` in
  `{completed, failed, killed}` — **authoritative basic terminal**.
  Emits `EventBackgroundTaskTerminal`.
- `user` tool_result for TaskOutput with
  `tool_use_result.task.status` terminal — **authoritative enriched
  terminal** (exit_code, output_file, actual output bytes). Emits
  `EventBackgroundTaskTerminal` with the richer payload, idempotent
  upsert against whatever task_updated wrote.
- `system/task_notification` — **not a completion source**. Dropped.
  See [claude-wire.md §task_notification](../references/claude-wire.md#systemtask_notification).

### Dedup rule

Both `task_updated` and TaskOutput can arrive for the same
`task_id`. The order is undefined. Triage's
`AppendCompletionItem` upsert is idempotent — later events with
richer payloads update in place. No parser-level dedup required.

### Output

Triage writes a `tool_completion` row:
- `id` = `"task-complete:<launch.id>"` (stable; idempotent upsert)
- `completion_of` = the backgrounded tool's launch `tool_use_id`
- `kind` = `"tool_completion"`
- `status` = `completed` / `errored`
- payload with `{output_file, exit_code, output_payload_id}`

### Task-lifecycle events can outlive the owning turn

A `task_updated` for a backgrounded task can arrive AFTER the
turn that launched it has completed. Triage writes the
`tool_completion` row regardless of the turn's state. The tray
renders it on its own retention clock. See
`/tmp/claude-bg-spike/ndjson_outlives.log` for a captured example.

## 3. Turn lifecycle

One-to-one with a user → assistant round-trip. The authoritative
"is the agent currently working?" signal.

### Participants

**Claude**:
- Turn start: implicit when the user sends (agent-overflow tracks
  this at the session layer when `SendMessage` is called).
- Turn end: `result` envelope. Carries `subtype`, `stop_reason`,
  `usage`, `duration_ms`, `total_cost_usd`, `modelUsage`,
  `permission_denials`, `terminal_reason`.
- Final assistant message id: NOT on `result`. Track from the last
  `assistant` envelope's `message.id`.

**Codex**:
- Turn start: `turn/started` notification.
- Turn end: `turn/completed` notification (or `turn/aborted` for
  interrupted).
- Final assistant message id: on `turn/completed.lastAssistantMessageId`.

### Emitted events

- `EventTurnStart` → triage → `provider:turn_started` to frontend,
  writes turn row with `completed_at=null`.
- `EventTurnComplete` → triage → `provider:turn_completed` to
  frontend, updates turn row, force-closes orphan non-background
  running tool_calls.

### Invariant (load-bearing)

> **Turn state is wire-pushed.** The UI's "Working…" indicator and
> active-turn flag come exclusively from provider-pushed
> `EventTurnStart` / `EventTurnComplete`. Never derive turn activity
> from item state (e.g. "some tool_call is still running, so the
> turn must be active"). A dropped completion must not freeze the
> UI.

### Turn-level projections

The `turns` row carries:
- `turn_id` (provider-assigned)
- `thread_id` FK
- `turn_index` (incrementing per-thread counter)
- `started_at` (ms)
- `completed_at` (ms, nullable; null = in-flight or session died)
- `stop_reason` (text: `end_turn` / `max_tokens` / `tool_use` /
  `stop_sequence` / `refusal` / `error` / `interrupted`)
- `assistant_message_id` (text, nullable) — the final assistant_text
  item id, used by the frontend to render the completion divider.
- `token_usage_json` — snapshot of `usage` for the "Worked for 12s"
  label with token counts.
- `error_message` — populated when `stop_reason` indicates error.

### Crash behavior

If the app or provider crashes mid-turn, the `turns` row stays
with `completed_at=null`. On next startup / thread reopen:

- Frontend shows no active-turn spinner (spinner requires a live
  `activeTurn` push, not a nullable DB row).
- Frontend shows no "Worked for Xs" divider for that turn (requires
  `completed_at`).
- UI may optionally render an "interrupted" marker for the turn.
- We do NOT reconcile by probing the session (see §Non-goals).

### Non-goals — no session-liveness probing

The UI does NOT probe provider session liveness to infer turn
state. A session can legitimately have backgrounded tasks still
running while its owning turn has completed (common case:
`run_in_background:true` Bash that outlives `result`). Probing for
"is the process still alive" tells you nothing useful about turn
state. Session probe code exists at
`internal/provider/codex/session_probe.go` for recovery/resume use
cases only — it must not feed turn detection.

## Event table (wire → internal → frontend)

| Wire envelope | Internal event (Go) | Frontend event | Triage action |
|---|---|---|---|
| Claude `tool_use` | `EventToolStart` | `provider:item_upsert` | Upsert `tool_call` row, status=running |
| Claude inline `tool_result` | `EventToolComplete` | `provider:item_upsert` | Update `tool_call` to terminal |
| Claude bg placeholder `tool_result` | `EventToolComplete` | `provider:item_upsert` | Per-spec: keep `status=running`, record `is_background=true` |
| Claude `system/task_started` | `EventToolStart` (meta-only) | `provider:item_upsert` | Merge `task_id` into `items.meta` |
| Claude `system/task_updated` terminal | `EventBackgroundTaskTerminal` | `provider:item_upsert` | Idempotent `tool_completion` sibling |
| Claude TaskOutput `tool_result` | `EventToolComplete` + `EventBackgroundTaskTerminal` | `provider:item_upsert` (×2) | Close TaskOutput row; enrich sibling row |
| Claude `system/task_notification` | — | — | Dropped |
| Claude `result` | `EventTurnComplete` | `provider:turn_completed` | Update `turns` row, force-close orphans |
| Codex `item/started` | `EventToolStart` | `provider:item_upsert` | Upsert item row |
| Codex `item/completed` | `EventToolComplete` | `provider:item_upsert` | Update item row |
| Codex `turn/started` | `EventTurnStart` | `provider:turn_started` | Insert `turns` row |
| Codex `turn/completed` / `turn/aborted` | `EventTurnComplete` | `provider:turn_completed` | Update `turns` row, force-close orphans |

## Frontend state shape

`ThreadPane` carries two per-pane state objects:

```ts
interface ActiveTurn { turnId: string; startedAt: number }
interface SettledTurn {
  turnId: string;
  startedAt: number;
  completedAt: number;
  stopReason: string;
  assistantMessageId: string | null;
  tokenUsage: { inputTokens, outputTokens, cacheReadTokens, costUsd } | null;
}

activeTurn: ActiveTurn | null            // working indicator on iff non-null
latestSettledTurn: SettledTurn | null    // completion divider on iff non-null
```

### `isTurnActive` replacement

```ts
// Pre-refactor (REMOVED)
get isTurnActive() {
  return items.some(/* running tools, streaming text, approvals */);
}

// Post-refactor
get isTurnActive() {
  return activeTurn !== null;
}
```

### Rehydration on thread switch

On `SwitchThread`, the frontend calls `ListRecentTurns(threadId, 2)`
to rehydrate `latestSettledTurn` from the DB. `activeTurn` is NOT
rehydrated from persistence — it's only set on live
`provider:turn_started` events. A turn with `completed_at=null` in
the DB renders as "turn was interrupted" not "turn is currently
active."

## UI components driven by this state

| Component | State read | Render rule |
|---|---|---|
| `ChatWorkingIndicator` | `pane.activeTurn.startedAt` | Self-ticking timer, appears iff `activeTurn !== null`. |
| `MessageTimeline` (completion divider) | `pane.latestSettledTurn.{assistantMessageId, tokenUsage}` | Separator rendered before the item whose id matches `assistantMessageId`. Label `"Response • Worked for Xs · Yk tokens"`. |
| `ToolCallCard` (backgrounded badge) | `item.isBackground && item.status === 'running'` | Renders a `"…"` blue badge next to the tool name. |
| `BackgroundTaskTray` | `items` (unchanged from existing) | Pairs launch + completion siblings; drops pair on retention. |

## Anti-patterns (forbidden)

- **Deriving "is working" from items.** Deriving this from item state
  means any parser bug that drops a completion freezes the UI.
- **Blocking turn-complete on tool_calls.** Backgrounded tasks can
  legitimately outlive a turn. The turn must close on the wire
  signal, not on "all tools done."
- **Probing session liveness for turn state.** See §Non-goals.
- **Rewriting tool_use_id between start and complete.** Breaks the
  tool-lifecycle invariant. See `internal/provider/codex/session.go`'s
  close_agent rewrite — it's symmetric (both start AND complete
  rewrite), but even that pattern is a smell; prefer one-shot upserts.
- **Consuming a tool's `tool_result` in another code path.** This
  was the TaskOutput bug's root cause. The standard completion path
  always runs; enrichments are additive.
- **Using `task_notification` as a completion source.** See
  [claude-wire.md §task_notification](../references/claude-wire.md#systemtask_notification).

## References

- Wire shapes: [`claude-wire.md`](../references/claude-wire.md),
  [`codex-wire.md`](../references/codex-wire.md).
- Event taxonomy full routing: [`triage-routing.md`](triage-routing.md).
- Data flow end-to-end: [`data-flow.md`](data-flow.md).
- Invariants (authoritative guardrails): [`invariants.md`](invariants.md).
- Captured wire samples: `/tmp/claude-bg-spike/*.log`,
  `/tmp/taskoutput-multi-spike/ndjson.log`.
