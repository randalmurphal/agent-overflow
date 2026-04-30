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
| **Task** | `task_id` (Claude only) | `task_updated` terminal; TaskOutput can enrich/fallback | `items` row, `kind="tool_completion"`, `completion_of=<launch_id>` |
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
- TaskOutput — a regular inline tool the agent may call to retrieve a
  still-retained background task. Its own `tool_use_id`'s completion
  is the retrieval result. Any background-task details it surfaces are
  additive and must not replace the TaskOutput tool row.
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

### Codex background projection

Codex has no `run_in_background` flag, but it still has backgrounded
work. Triage may stamp `is_background=true` only from wire-typed
signals:

- `CommandExecution.source == "unifiedExecStartup"` for a yielded
  `exec_command` whose PTY keeps running after the model moves on.
- `collabAgentToolCall` `spawn_agent` whose `agentsStates` still
  reports a non-terminal child when the parent yields or reaches
  `turn/completed`.

Once stamped, the Codex launch row follows the same UI contract as
Claude: it stays `status='running'`, renders the `…` badge inline,
appears in the background tray, and later gets a separate
`tool_completion` sibling. Codex reports terminal state via
`item/completed` for commands, `wait_agent`, or injected
`<subagent_notification>` fragments for detached child agents.

Codex `unifiedExecStartup` command executions are the exception to the
persisted-launch-row shape. Their starts are tracked as transient
running-tray state so the user can see the command immediately, but
they are not written into transcript history at start time. If the
command completes before a yield, triage persists one normal command
row with its output. If Codex yields while the command is still
running, the transient tray item flips to `is_background=true`; its
output becomes chat history only when Codex explicitly polls the
terminal.

The retired `BackgroundClassifier` heuristic must not come back.
Background authorization comes from the wire fields above; model
text/thinking or turn completion is only the trigger that marks an
already-authorized launch as having moved to the background.

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
  `{completed, failed, killed}` — **authoritative lifecycle
  terminal**. Emits `EventBackgroundTaskTerminal`.
- `user` tool_result for TaskOutput with
  `tool_use_result.task.status` terminal — explicit agent retrieval of
  a still-retained task. It can carry `exitCode`, `output`, `result`,
  `description`, and sometimes `output_file`. It is useful as
  enrichment/fallback, but the UI lifecycle must not depend on the
  agent choosing to call it.
- `system/task_notification` — **not a completion source**. It is an
  agent notification. It may carry `summary` and `output_file` that the
  UI can render as a separate notification row, but it must not mutate
  task completion state. See
  [claude-wire.md §task_notification](../references/claude-wire.md#systemtask_notification).

### Merge rule

`task_updated` and TaskOutput can arrive for the same `task_id`.
Fresh spikes show `task_updated` arrives before the TaskOutput
`tool_result` when TaskOutput blocks on a running task. A plain
`task_updated` can also arrive without any TaskOutput call at all.

Triage must coalesce by stable completion id and preserve richer
existing payload data if a later poorer signal arrives. No parser-level
dedup is required, but store-level merging must be monotonic: status
can move to terminal, but output/exit-code/output-file data should not
be erased by a later lifecycle-only event.

### Tray decoupling — process state vs. agent observation (Tray-A)

The tray reflects **process state** — "is this background process
still running on the host?" — while the chat reflects **agent
observation state** — "has the agent observed completion?". The two
diverge when the host process exits but the agent hasn't yet noticed
(e.g. a backgrounded `sleep 30` finishes mid-turn while the agent is
still streaming text). Splitting the two prevents the chat from
"lying" by showing a completion row at process-exit time, before the
agent has actually seen it.

Implementation:

1. **`task_updated`** with status in `{completed, failed}` writes a
   row to `pending_background_task_terminals` (PK
   `(thread_id, task_id)`). The tray query `ListLiveBackgroundTasks`
   joins against this table with `NOT EXISTS` on `tool_use_id`, so
   the launch drops out of the tray immediately. **No chat row is
   written yet.** Triage emits
   `provider:background_task_state{state:"exited"}` so the frontend
   can refresh.
2. **Agent observation** — `system/task_notification` (the model
   sees the queued attachment on the next iteration) or a
   `TaskOutput` `tool_result` (the model explicitly polled) — drains
   the stash via `TakePendingBackgroundTerminal` and writes the
   `tool_completion` sibling at the current write head. Triage emits
   `provider:background_task_state{state:"drained"}`. After this
   point the tray surfaces both rows joined together until they age
   out via retention.
3. **`task_updated` with `status="killed"`** is a deliberate carve-out:
   the `killed` status is only reached via the user's explicit
   `stop_task` (the StopClaudeTask binding behind the tray's Stop
   button). The user already knows the process was stopped — there's
   nothing for the agent to "observe" — so triage skips the stash and
   writes the sibling immediately so chat shows the killed badge
   without waiting for a future turn.

### Crash recovery — orphaned background launches

If the previous app instance died while a backgrounded launch was
still in `status='running'`, the agent will never observe its
completion (the owning provider session is gone). Without intervention
the launch would render as "running" forever in chat and tray.

`Router.RecoverOrphanedBackgroundTasks` runs once during
`App.ServiceStartup`. It queries
`Store.ListOrphanedBackgroundLaunches` for every `tool_call` row
that is `status='running'`, has no completion sibling, and has no
stash entry. For each one with a `task_id` in its `items.meta`, it
writes the `tool_completion` sibling directly with
`source="session_died"`, `status="killed"` — no stash row is staged.
Writing the sibling is the same terminal step the steady-state
observation path takes after draining a stash, so the recovery path
reuses `writeBackgroundCompletionSibling` end-to-end. Idempotent and
crash-safe: if the process dies mid-sweep, the launch row is still
`status='running'` with no sibling and no stash, so the next boot's
sweep finds it again. Launches that never received their
`task_started` meta merge are skipped (no `task_id` to key on).

### Output

Triage writes a `tool_completion` row:
- `id` = `"complete:<launch.id>"` (stable; idempotent upsert)
- `completion_of` = the backgrounded tool's launch `tool_use_id`
- `kind` = `"tool_completion"`
- `status` = `completed` / `errored` / `killed`
- payload metadata may include `{output_file, exit_code, end_time}`
- payload data may include TaskOutput `task.output` / `task.result`
  when the agent explicitly retrieved it, or a lazy pointer to
  `task_notification.output_file` when the notification is available

### Task-lifecycle events can outlive the owning turn

A `task_updated` for a backgrounded task can arrive AFTER the
turn that launched it has completed. Triage writes the
`tool_completion` row at the current thread write head when one is
open, otherwise at the latest persisted turn. The tray renders it on
its own retention clock. See
`docs/references/fixtures/claude/ndjson_outlives.log` for a captured example.

### Desired chat-history contract

The UI should not expose Claude/Codex implementation differences. The
history contract is:

- The launch row stays where the agent made the call. Once backgrounded
  it renders a `...` badge and appears in the background tray.
- The launch row does not flip to completed when background work ends;
  it remains the historical "started background work" row.
- A separate `tool_completion` row appears where the completion was
  presented to the agent/user stream. If completion lands while
  assistant text is streaming, defer the row until the active streaming
  block closes so the transcript stays readable.
- The completion row shows success/failure/stopped in collapsed form.
  Expanding the row lazy-loads output details when available. Keep
  large command output out of the always-loaded timeline window.
- Provider notification rows, such as Claude `task_notification` and
  Codex terminal/subagent notifications, may render as muted chat
  markers. They never decide lifecycle state.

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
| Claude `tool_use` | `EventToolStart` | `provider:item_event` upsert | Upsert `tool_call` row, status=running |
| Claude inline `tool_result` | `EventToolComplete` | `provider:item_event` upsert | Update `tool_call` to terminal |
| Claude bg placeholder `tool_result` | `EventToolComplete` | `provider:item_event` upsert | Per-spec: keep `status=running`, record `is_background=true` |
| Claude `system/task_started` | `EventToolStart` (meta-only) | `provider:item_event` upsert | Merge `task_id` into `items.meta` |
| Claude `system/task_updated` terminal | `EventBackgroundTaskTerminal` | `provider:item_event` upsert | Idempotent `tool_completion` sibling |
| Claude TaskOutput `tool_result` | `EventToolComplete` + optional `EventBackgroundTaskTerminal` | `provider:item_event` upsert | Close TaskOutput row; optional sibling enrichment/fallback |
| Claude `system/task_notification` | — today; future notification event only | — today; future row only | No lifecycle state mutation |
| Claude `result` | `EventTurnComplete` | `provider:turn_completed` | Update `turns` row, force-close orphans |
| Codex `item/started` | `EventToolStart` | `provider:item_event` upsert | Upsert item row |
| Codex `item/completed` | `EventToolComplete` | `provider:item_event` upsert | Update item row |
| Codex `turn/started` | `EventTurnStart` | `provider:turn_started` | Insert `turns` row |
| Codex `turn/completed` / `turn/aborted` | `EventTurnComplete` | `provider:turn_completed` | Update `turns` row, force-close orphans |

## Frontend state shape

`ThreadPane` carries two per-pane state objects:

```ts
interface ActiveTurn {
  turnId: string;
  turnIndex: number;
  startedAt: number;
}
interface SettledTurn {
  turnId: string;
  turnIndex: number;
  startedAt: number;
  completedAt: number;
  stopReason: string;
  assistantMessageId: string | null;
  tokenUsage: { inputTokens, outputTokens, cacheReadTokens, costUsd } | null;
  aborted: boolean;
  errorMessage: string;
}

activeTurn: ActiveTurn | null            // working indicator on iff non-null
latestSettledTurn: SettledTurn | null    // completion divider on iff non-null
```

`aborted` and `errorMessage` on `SettledTurn` are live-read by the
completion-divider label precedence — see
`frontend/src/lib/components/chat/CompletionDivider.svelte` (the
`baseLabel` derivation picks `"Interrupted"` > `"Error"` > `"Response"`
based on these fields).

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
| `ToolCallCard` (backgrounded badge) | `item.isBackground && item.status === 'running'` | Renders a `…` status badge on the inline launch row. |
| `BackgroundTaskTray` | `ListLiveBackgroundTasks(threadId)` | Shows running launches, pending Codex unifiedExec commands, and recent completion siblings; drops completed pairs on retention. |

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
- Captured wire samples: `docs/references/fixtures/claude/*.log`,
  `docs/references/fixtures/claude/taskoutput_multi.ndjson`.
