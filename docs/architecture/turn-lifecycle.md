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

### Wire-round vs logical-turn cadence

`provider:turn_started` and `provider:turn_completed` fire **per wire
round**, not per logical turn. A round corresponds to one Claude
`result` envelope (or one Codex `turn/completed`); a logical
agent-overflow turn — one user-typed prompt — can span multiple
rounds. The canonical multi-round case is Claude's CLI synthesizing a
`type:"user"` envelope from a `task_notification`: the assistant's
first `end_turn` lands as result envelope #1, the synthesized prompt
provokes another model call, and the second response lands as result
envelope #2 (the `interactive_outlives_taskoutput_monitor.ndjson`
fixture captures seven such envelopes in one logical turn).

Two cadences run in parallel:

| Cadence | Driver | Granularity | What it controls |
|---|---|---|---|
| Frontend visibility | `currentRoundID` / `setOpenRound` / `takeOpenRound` | Per wire round | `provider:turn_started`/`provider:turn_completed` emissions — working indicator, Stop button, composer block, read-state projection |
| Persistence | `markTurnSettled` / `settleTurnRow` | Per logical turn (turnIndex) | `turns` row UPDATE, checkpoint capture, streaming-item settlement |

Round entry points:

- **`handleTurnStart`** opens round 1 of every logical turn. It calls
  BOTH `setOpenTurn` (per-turn flow-control + counter re-init) AND
  `setOpenRound` (per-round id allocation), then emits
  `provider:turn_started` with the per-round uuid as TurnID.
- **`handleInit` re-round branch** opens rounds 2+ when an
  `EventInit` arrives for a thread whose current logical turn is
  already settled (`settledTurns[turnKey]==true`). Calls
  `setOpenRound` only — does NOT call `setOpenTurn`. This is
  load-bearing: id-allocating counters must survive across the
  multi-result-per-turn boundary so post-round-1 rows don't collide
  with rows already persisted under the same logical turn (see
  `internal/triage/multi_result_test.go`).
- **`handleTurnComplete`** uses `takeOpenRound` (read-and-clear) to
  decide whether to emit. An empty slot means a synthetic complete
  already raced ahead (the fatal-error-then-real-result pattern in
  `handleError`); the second wire complete then emits nothing, so
  the frontend sees exactly one `turn_completed` per round.
  Persistence work stays gated by `markTurnSettled` at logical-turn
  granularity.

The persisted `turns.turn_id` stays on `resolveTurnID` (logical-turn
granularity) so multi-round logical turns share a single row.

### Invariant (load-bearing)

> **Turn state is wire-pushed.** The UI's "Working…" indicator and
> active-turn flag come exclusively from provider-pushed
> `EventTurnStart` / `EventTurnComplete` (per wire round; see above).
> Never derive turn activity from item state (e.g. "some tool_call
> is still running, so the turn must be active"). A dropped
> completion must not freeze the UI.

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
  item id for the settled-turn projection.
- `token_usage_json` — snapshot of `usage` for trace/debug surfaces.
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

The frontend splits "live turn metadata" between a global registry
and a per-pane settlement record:

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

// Global, keyed by threadId. Lives in
// frontend/src/lib/stores/threadStatuses.svelte.ts. Both the
// chat working indicator AND the sidebar pill read from here.
getActiveTurn(threadId): ActiveTurn | null

// Per-pane. Carries the latest settled-turn projection for read-state
// and trace/debug consumers.
pane.latestSettledTurn: SettledTurn | null
```

**Single source of truth.** `ActiveTurn` lives only in the global
registry, never on `ThreadPane`. The pane exposes `pane.activeTurn`
as a transparent shim onto `getActiveTurn(pane.threadId)` so existing
readers don't change shape, but no per-pane copy of the data exists.
This avoids the bug where switching threads cleared the per-pane
`activeTurn` while the global store still held the live record —
the chat working indicator would go dark on a thread the backend
was actively working on.

`aborted` and `errorMessage` on `SettledTurn` remain part of the
settled-turn projection for read-state and trace/debug consumers. The
chat transcript no longer renders a settled-turn divider from this state;
the visible "Response" divider is structural and appears when assistant
text first follows tool activity in the same turn.

### `isTurnActive` replacement

```ts
// Pre-refactor (REMOVED)
get isTurnActive() {
  return items.some(/* running tools, streaming text, approvals */);
}

// Post-refactor
get isTurnActive() {
  return getActiveTurn(this.threadId) !== null;
}
```

### Rehydration on thread switch

On `SwitchThread`, the frontend calls `ListRecentTurns(threadId, 2)`
to rehydrate `latestSettledTurn` from the DB. The global active-turn
registry is NOT rehydrated from persistence — it's only set on live
`provider:turn_started` events. A turn with `completed_at=null` in
the DB renders as "turn was interrupted" not "turn is currently
active." When the user switches AWAY from a thread with a live
turn and back, the indicator returns because the global registry
held the record across the switch — nothing in pane lifecycle
clears it.

### Per-thread send queue

The composer is always-typeable. When the user submits a message
mid-round (`getActiveTurn(threadId) !== null`), it lands in the
per-thread send queue (`frontend/src/lib/stores/sendQueue.svelte.ts`)
instead of dispatching `SendMessageWithOptions` immediately. The
queue is in-memory only (`SvelteMap<threadId, QueueItem[]>`),
keyed identically to the global active-turn registry, and survives
thread switches.

`QueueItem` captures everything needed to dispatch the message
later: `message`, full `attachments` (not ids — click-to-edit
needs to restore them into the composer without a backend
round-trip), `terminalChips`, and plan-revision metadata
(`sourceProposedPlan`, `revisionSourceProposedPlan`,
`revisionSourceCommentIds`).

**Drain trigger.** Every `provider:turn_completed` listener fires
`tryDrainNextQueued(threadId)` after the existing
`projectTurnCompleted` call. Drain is uniform across cause —
success, error, or aborted — matching both reference UIs:

- Claude Code's `useQueueProcessor` flips on every `!isQueryActive`
  transition (`src/hooks/useQueueProcessor.ts`).
- Codex's `maybe_send_next_queued_input` is called from 11 sites
  (`codex-rs/tui/src/chatwidget.rs`), every state-clearing
  transition.

Stop-with-queue ("user hits Esc with messages queued") falls out of
the same uniform rule: `InterruptTurn` → backend emits an aborted
`turn_completed` → drain fires → first queued item is dispatched as
the next user message. No special-case wiring.

**Drain sequence.**

1. `provider:turn_completed` arrives → `activeTurn` cleared.
2. `popFront(threadId)` → head item lifted; if undefined, return.
3. `projectSendStarted(threadId)` → `pendingSendThreads.add` —
   the working-indicator bridge predicate keeps the spinner up
   across the RPC roundtrip (see below).
4. `await SendMessageWithOptions(...)` — typically 50–200ms.
5. Success → backend emits `provider:turn_started` → existing
   `projectTurnStarted` handler clears `pendingSendThreads`.
6. Failure → `enqueueAtFront(threadId, item)` restores the popped
   item, `clearPendingSend(threadId)` collapses the bridge, the
   error fans out to matching panes via `pane.setGeneralError`.

**Working-indicator bridge.** Without intervention,
`activeTurn` would be null between steps (1) and (5). To prevent
the spinner from flickering for ~200ms, the working indicator's
`isWorking` predicate is

```ts
isWorking = activeTurn !== null
  || hasQueueItems(threadId)
  || hasPendingSend(threadId);
```

The elapsed-counter span is gated separately on `activeTurn !== null`
so the bridge moment renders just `Working` (no `for 0s` flash);
the next round's `provider:turn_started` arms a fresh `startedAt`
and the counter ticks from `0s`.

**Approval gate.** During a pending tool approval, the wire round
hasn't completed (backend's `currentRoundID` stays set). No
`turn_completed` fires until approval resolves. Drain naturally
waits — there's no special-case approval-aware drain code.

**Stdin race (Claude only, accepted).** When round N ends with
both a queued user message AND a pending bg-subagent task
notification: our `tryDrainNextQueued` writes the user message
to stdin while the CLI may auto-inject the task_notification.
Whichever reaches the CLI input handler first becomes round N+1.
Claude Code's source resolves this deterministically via
in-process priority (user `next` beats notification `later`); we
cannot — stdin write order is non-deterministic. Accepted: the
model handles both messages in arrival order, ordering is
non-deterministic but the timeline reflects what the agent
actually saw, not a presumed order.

**Cleanup.** `clearThreadStatus(threadId)` (called when a thread
is archived/deleted) calls `clearForThread` on the queue. Tests
should call `resetSendQueueForTest()` in `beforeEach`.

## Error routing

Every terminal failure mode lands on one of four paths so the
working indicator clears and the user gets actionable copy:

1. **API error mid-turn (session alive)** — Claude `assistant.error`
   parses to a fatal `EventError` tagged `expect_turn_complete: true`.
   The wire `result{is_error:true}` then arrives and settles the turn
   normally; triage routes the error item as `kind: "api_error"`
   with the SDK enum on Meta. The frontend renders an `APIErrorRow`
   with branched CTA copy (rate_limit → "Add credits" link,
   authentication_failed → "Run /login", etc).
2. **Process exit during turn** —
   `EventSessionStatus{Content:"error"}` → triage promotes to
   `provider:session_died` event, persists a `notification` row with
   `meta.kind = "session_died"`, and synthesizes a truncated
   `EventTurnComplete{aborted:true}` if a turn is open. Three
   loosely-coupled UI projections: the truncated turn-complete
   clears the working indicator; the notification row shows in the
   timeline as historical record; the typed event drives the
   `ProviderStatusBanner`'s session-error slot with Reconnect
   button.
3. **Clean EOF mid-turn (no `EventSessionStatus{"error"}`)** —
   `Router.CleanupThread` is the safety net: any open turn at
   teardown synthesizes a truncated turn-complete before state is
   torn down. Idempotent against the path above via
   `markTurnSettled`.
4. **Codex `error+willRetry:false`** — sets `meta.fatal:true` so the
   triage `handleError` fatal branch closes the turn. No
   `expect_turn_complete` opt-in (Codex doesn't follow up with a
   `result` envelope), so the synthetic truncated turn-complete fires.

### Retry envelopes

Transient retries (Claude `system.api_retry`, Codex
`error+willRetry:true`) land on `EventAPIRetry` and produce a
single timeline row with deterministic id `retry:<turnIndex>` so
re-attempts upsert in place. Mirroring Claude Code's
`SystemAPIErrorMessage.tsx`, attempts < 4 are dropped silently —
most retries succeed within three attempts. Forward-progress events
(text, tool start/complete, turn complete) flip the row's status
to `completed` so it reads as historical context. There is no
counterpart "retry succeeded" wire signal from either provider.

## UI components driven by this state

| Component | State read | Render rule |
|---|---|---|
| `ChatWorkingIndicator` | `pane.activeTurn.startedAt` | Self-ticking timer, appears iff `activeTurn !== null`. |
| `MessageTimeline` (response divider) | Ordered timeline nodes | Separator rendered before assistant text when tool activity immediately precedes the response in the same turn. |
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
