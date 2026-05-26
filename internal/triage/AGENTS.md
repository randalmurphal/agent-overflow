# internal/triage/

Classifies provider events and decides what ships to the frontend vs
what writes to SQLite. The single most important rule is that triage
has **no derived state** — it is a pure function of the current event
plus a narrow, bounded set of per-thread correlation state.

## Layout

The package is split by concern so each file owns a narrow slice of the
routing pipeline. New routing logic belongs in whichever file most
closely matches its concern; create a new file (and list it here) if
none fits.

- `router.go` — entry point. `Router` struct, `Handle` dispatch switch,
  `persistItem` / `emitThreadUpdated` shared helpers, and the top-level
  error / session-status / token-usage / rate-limit routers.
- `session_status.go` — `EventSessionStatus` classifier:
  `classifySessionStatus` maps content + meta → `ProviderStatusEventKind`
  (rate-limit / unauthenticated / transient retry / ok), plus the
  `logUnknownSessionStatusOnce` capped log throttle that keeps novel
  status strings from polluting steady-state logs.
- `approvals.go` — approval-request lifecycle: pending-approval map,
  approval-resolved fan-out, decision → item projection.
- `user_inputs.go` — structured user-input request lifecycle and
  provider:user_input frontend event fan-out.
- `turn_lifecycle.go` — per-turn and per-thread correlation state
  (open turns, interrupt queue, stopped-thread markers, turn span
  bookkeeping, cleanup paths).
- `live_state.go` — refresh/reconnect snapshot of backend-owned live
  session state (active wire round, queue, interactive prompts, live todo)
  copied under one router lock for the App transport DTO.
- `tool_lifecycle.go` — tool-call launch/completion rows,
  background-task pairing (Claude), summary/status derivation.
- `codex_background.go` — Codex-specific background projection.
  Tracks unifiedExec commands as transient running-tray state, persists
  typed completions as normal command rows using the original item id.
  Terminal interactions persist only waited/interacted marker rows while a
  unifiedExec tracker is live. Pending unifiedExec commands are tray-visible
  before a typed wait but only become backgrounded after that wire-typed
  wait signal. Spawn-agent starts are tracker-only; terminal
  spawn completions create the visible transcript row and may later use
  background sibling completion rows. Authorized by the wire-typed signals
  enriched onto Meta in
  `internal/provider/codex/protocol.go` (see invariant 25).
- `terminal_interaction.go` — Codex-specific "Waited for background
  terminal" row persistence. Handles `EventTerminalInteraction` for
  the empty-stdin (polling) variant emitted when the model calls
  `write_stdin` against a backgrounded unified-exec PTY. Non-empty
  stdin persists an "Interacted with background terminal" marker without
  storing stdin bytes.
- `tool_result_file_change.go` — `file_change` tool-result normalisation
  (inline diff projection, unified patch assembly).
- `tool_paths.go` — per-turn agent-touched-path tracking. Extracts paths
  from Claude `Edit`/`Write`/`MultiEdit`/`NotebookEdit` tool args and
  Codex `fileChange` items, normalises to workspace-relative form, and
  persists them to `thread_tracked_files`. Message checkpoints are
  captured before user sends in `app_checkpoint.go`; the tracked-path
  table scopes conversation-and-files revert and agent-only diff
  previews. Bash side effects are intentionally untracked.
- `tool_result_diff_upgrade.go` — late-arriving diff upgrades that
  attach a richer payload onto a previously persisted tool result.
- `command_inline_diff_capture.go` / `command_inline_diff_parser.go` /
  `command_inline_diff_runtime.go` / `command_inline_diff_persist.go` —
  command-execution inline-diff pipeline, split by phase
  (capture → parse → runtime match → persist).
- `payload_items.go` — diff / command output / thinking / plan payload
  writers.
- `stream_items.go` / `stream_state.go` / `block_events.go` —
  streaming text / thinking block lifecycle and the content-block
  index bookkeeping they depend on.
- `usage_compaction.go` — context-window usage normalisation and
  compaction boundary persistence.
- `turn_events.go` — frontend-facing payload shapes for
  `provider:turn_started` / `provider:turn_completed` /
  `provider:subagent_notification`, plus the canonical stop-reason
  normaliser.
- `meta.go` — shared JSON-inspection helpers.
- `maps.go` — generic map utilities (currently just `deleteByPrefix`).

## Routing table

| Event kind | Destination |
|---|---|
| Text delta | Frontend (passthrough). |
| Tool-use start/complete | Frontend event + item in SQLite on completion. |
| Approval request | Frontend event with `request_id` preserved. |
| Diff | SQLite payload + meta to frontend. Full diff is on-demand. |
| Command output | SQLite payload + meta to frontend. |
| Thinking block | SQLite payload + preview to frontend. |
| Turn metadata (cost/tokens) | Persist on turn completion. |
| Context-window usage | Frontend context meter + `threads.last_token_usage`. |
| Background task terminal (Claude) | `tool_completion` sibling row upsert (idempotent). See `turn-lifecycle.md`. |
| Codex unifiedExec / spawn_agent | unifiedExec starts are transient running-tray state; typed command completions persist as normal command rows using the original item id. Spawn-agent starts are pending-only; terminal spawn completions persist the visible row and use sibling `tool_completion` rows. See `codex_background.go` + invariant 25. |
| Codex terminal interaction | Empty stdin persists/reuses one visible `terminal_interaction` wait carrier on the current open turn while the PTY tracker is live. Non-empty stdin first flushes any active wait for that process, then persists an interaction marker without storing stdin bytes. See `terminal_interaction.go`. |
| Turn start/complete | Write `turns` row; emit `provider:turn_*` to frontend; force-close orphan tool_calls on complete. |
| Error | Distinct event kind; frontend renders as status/alert. |
| Unknown | Log with full context, do not drop silently. |

## Lifecycles we route

Authoritative mental model:
[`turn-lifecycle.md`](../../docs/architecture/turn-lifecycle.md).

- **Tool lifecycle** — `EventToolStart`/`EventToolComplete` keyed by
  tool_use_id. Triage upserts `tool_call` rows. Claude background
  placeholders stay `status=running` until the task lifecycle writes the
  `tool_completion` sibling. Codex spawn_agent launch rows settle as the
  completed "spawned" event; child completion is a separate sibling row.
- **Task lifecycle (Claude only)** — two-phase decoupling between
  the host-side process exit and the agent-observation event:
  - `EventBackgroundTaskTerminal` with `source="task_updated"` and
    `status` in `{completed, failed}` → write a row to
    `pending_background_task_terminals` (PK
    `(thread_id, task_id)`); the tray query hides the launch via a
    `NOT EXISTS` join. **No chat sibling yet.**
  - `EventBackgroundTaskTerminal` with `source="task_output"` (agent
    polled via TaskOutput) → drain the stash, merge stash data with
    the observation, write the `tool_completion` sibling at the
    current write head.
  - `EventBackgroundTaskNotification` → persist the notification row;
    if a stash exists for the same `task_id`, drain it through the
    same shared helper (the agent saw the queued attachment now).
  - `EventBackgroundTaskTerminal` with `source="task_updated"` and
    `status="killed"` (user clicked Stop) is the carve-out: write
    the sibling immediately because the user already knows the
    process was stopped — there's no "agent will observe later"
    phase to wait for.
  - **Crash recovery** — `Router.RecoverOrphanedBackgroundTasks`
    runs once at app boot for recoverable Claude launches whose owning
    provider session did not survive the previous app instance. A row is
    recoverable only when it still has no completion sibling and carries
    `items.meta.task_id`. It writes the `tool_completion` sibling
    directly (with `source="session_died"` on the sibling's meta)
    without staging a stash row, so a crash mid-sweep leaves the launch
    re-discoverable on the next boot.
- **Background-terminal projection (Codex only)** —
  `codex_background.go` tracks unifiedExec items as transient state and
  shows them in the running tray immediately. They only become
  backgrounded after a typed terminal-interaction notification for an explicit
  empty write_stdin poll. Typed unifiedExec completions always persist normal
  command rows using the original item id; raw exec output never creates,
  delays, or backgrounds transcript history.
  Spawn-agent starts are tracker-only until terminal spawn completion creates
  the visible tool row. Child-agent transcript completions are owned by
  wait_agent or subagent_notification; direct child lifecycle only updates live
  state.
  Authorized only by the wire-typed signals in Meta (invariant 25); no
  heuristic classifiers.
- **Turn lifecycle** — `EventTurnStart` writes a `turns` row with
  `completed_at=null`; `EventTurnComplete` updates it. Triage
  force-closes any `tool_call` row with `status='running' &&
  !is_background && turn_index=currentTurn` as a safety net.
  Frontend emissions follow a separate per-wire-round cadence — see
  "Wire-round vs logical-turn" below.

⚠ **Wire-round vs logical-turn cadence**:

`provider:turn_started` and `provider:turn_completed` fire **per wire
round**, not per logical turn. A round corresponds to one Claude
`result` envelope, one Claude soft message_delta stop_reason, or one
Codex `turn/completed`; a logical agent-overflow turn — one
user-typed prompt — can span multiple
rounds when Claude's CLI synthesizes a `type:"user"` envelope from a
`task_notification` and the model issues another response. The
frontend uses these per-round emissions to drive its working
indicator, Stop button, and composer-block state
— all of which want "model is engaged right now" semantics rather
than "user-typed prompt is in flight."

Two cadences run in parallel:

| Cadence | Driver | Granularity | Owner |
|---|---|---|---|
| Frontend visibility | `currentRoundByThread` / `setOpenRoundSnapshot` / `takeOpenRound` | Per wire round | `provider:turn_started`/`provider:turn_completed` emissions |
| Persistence | `claimTurnSettlement` / `settleTurnRow` | Per logical turn (turnIndex) | `turns` row UPDATE, streaming-item settlement |

Round entry points:

- **`handleTurnStart`** opens round 1 of every logical turn. It calls
  BOTH `setOpenTurn` (per-turn flow-control + counter re-init) AND
  `setOpenRoundSnapshot` (per-round id allocation), then emits
  `provider:turn_started` with the per-round uuid as TurnID.
- **`handleInit` re-round branch** (`maybeEmitReRoundOnInit`) opens
  round 2+ when an `EventInit` arrives for a thread whose current
  logical turn is already settled (`settledTurns[turnKey]==true`).
  Calls `setOpenRoundSnapshot` ONLY — does **not** call `setOpenTurn`. This
  is load-bearing: id-allocating counters (`segmentIndexByScope`,
  `blockIndexByScope`, `errorSeqByScope`, `terminalInteractionSeq`)
  must survive the multi-result-per-turn boundary so post-round-1
  text/think/error rows don't collide with rows already persisted
  under the same logical turn. See `multi_result_test.go` for the
  regression coverage.
- **`handleTurnComplete`** uses `takeOpenRound` (read-and-clear) to
  decide whether to emit `provider:turn_completed`. An empty slot
  means a synthetic complete already raced ahead (the
  fatal-error-then-real-result pattern in `handleError`); the second
  wire complete then emits nothing, so the frontend sees exactly one
  `turn_completed` per round. Persistence work (settleTurnRow,
  checkpoint, streaming settle) stays gated by `claimTurnSettlement` at
  logical-turn granularity.
- **Soft round-close** (Claude only) — `EventTurnComplete` with
  `provider.SoftRoundCloseMeta` arrives from `parse_stream.go` when
  the parent message ends with stop_reason ∈ `{end_turn,
  stop_sequence, refusal}` and `parent_tool_use_id` is null. Triage
  handles this identically to the `result`-driven complete:
  per-round emission + per-logical-turn settlement. The trailing wire
  `result` envelope arrives later (especially when a `local_agent`
  subagent is in flight — Claude CLI delays it until the subagent
  completes) and folds in cumulative usage / cost /
  `assistant_message_id` via `persistLateTurnPayload` — the
  `claimTurnSettlement` gate makes this a no-op for everything else.
  See
  [`invariants.md §27`](../../docs/architecture/invariants.md#27-soft-round-close-from-message_deltastop_reason-is-wire-typed).

The state diagram lives in
[`turn-lifecycle.md §Wire-round vs logical-turn cadence`](../../docs/architecture/turn-lifecycle.md#wire-round-vs-logical-turn-cadence).

Round id format: opaque per-round `uuid.NewString()` allocated in Go.
Carried as `TurnStartedEvent.TurnID` / `TurnCompletedEvent.TurnID`.
The persisted `turns.turn_id` is chosen at turn-start granularity:
provider-supplied `ProviderEvent.TurnID` wins, and `resolveTurnID`
is the fallback. Completion paths must settle that existing row; when
a completion event has no `TurnID`, use the persisted `(thread_id,
turn_index)` row before falling back to `resolveTurnID`. This keeps
multi-round logical turns on one row and prevents synthetic
completion paths from inventing `thread:index` rows for providers
with opaque wire turn ids.

⚠ **Load-bearing invariants** (see
[`invariants.md`](../../docs/architecture/invariants.md)):

- `task_notification` is NOT a completion source; drop parser
  emission into the lifecycle. Route it through a distinct
  notification row instead.
- Turn activity on the frontend is wire-pushed only — never derived
  from item state.
- No session-liveness probing for turn state inference.
- `setOpenTurn` does NOT fire from `handleInit` (re-round path).
  Calling it there would reset the id-allocating counters and
  re-introduce the multi-result-per-turn id-collision regression.

## Responsibility boundary

- What BELONGS here:
  - Classify a single event → zero or more (persist + emit) decisions.
  - Bounded per-thread transient correlation state with explicit
    cleanup paths (see below).
  - Shared helpers for `persistItem` and `emitThreadUpdated`.
- What does NOT belong here:
  - Cross-turn derivations — do them in the frontend or as a persisted
    projection, not as an in-memory map here.
  - Provider-specific types. Provider packages normalize before handing
    events to triage.
  - Business decisions about when to fork/resume a thread; that's
    `app.go`.

## Correlation state (bounded, not derived)

The Router carries a narrow set of per-thread maps (interrupt queue,
open turn index, content-block counters, active streaming block flags,
pending approvals / approval decisions, pending command inline diffs,
turn spans, stopped-thread markers, streaming render throttle) that
exist purely to correlate one event to the next
within a turn — not to duplicate the store or the provider session.
All of these are bounded and have an explicit cleanup path.

⚠ **Three distinct lifecycles intersect in this package and MUST stay
separate**:

- **Per-turn flow-control state** — `openTurns`, `interruptQueue`,
  `streamingItemCounts`, `activeTextBlocks`, `activeThinkingBlocks`,
  `pendingCommandDiffs`, `pendingApprovals`, `pendingApprovalItems`,
  `pendingUserInputs`. Cleared at turn end via
  `clearOpenTurn` (which fires from `handleTurnComplete`). These maps
  answer "is this turn live right now / what's queued behind a
  streaming row / what's mid-resolution."
- **Id-allocating counters** — `segmentIndexByScope`, `blockIndexByScope`,
  `errorSeqByScope`, `terminalInteractionSeq`. Cleared at
  `CleanupThread` only — except `setOpenTurn` resets the per-scope key
  to seed a fresh re-init (Claude `system.init` resend after interrupt
  is a deliberate from-scratch re-stream). They allocate primary keys
  (`text:N:S`, `think:N:B`, `error:N:S`, `waited:pid:N:S`) for `items`
  rows whose lifetime is the **thread**, not the turn. Wiping them at
  turn boundaries (which is what `clearOpenTurn` MUST NOT do) causes id
  collisions when the wire emits two `result` envelopes for one
  logical turn (Claude's `task_notification` → CLI-synthesized
  `type:"user"` envelope → second `result`, and the fatal-error
  synthetic-truncate then real-wire-complete race). The `LastTurnIndex`
  fallback in `currentTurnIndex` re-attaches post-`clearOpenTurn`
  events to the same turn so the surviving counter advances correctly
  and the next id never collides with rows already persisted under
  this turn. See `multi_result_test.go` for the regression coverage.
- **Logical-turn settlement state** — `settledTurns`. Survives wire
  round boundaries by design. It is reset by `setOpenTurn` (so a
  re-init can re-settle the same logical turn) and swept by
  `CleanupThread`. Tool paths are staged in `pendingToolPaths` between
  mutating tool start and successful completion, then written durably to
  `thread_tracked_files`.

When adding a new map, ask **three** questions:

1. Do the values written here become primary keys of persisted rows? If
   yes, it's an id-allocating counter — clean it in `CleanupThread` (and
   selectively in `setOpenTurn` for re-init), never in `clearOpenTurn`.
2. Does its data need to survive the wire-level turn boundary because
   it represents durable user-visible state? If yes, persist it in the
   store at the point it becomes known. Otherwise, it's per-turn
   flow-control — clean it in `clearOpenTurn`.
3. Does this map represent user-blocking live state that should prevent
   session reaping? (Pending approvals, user-input requests, queued
   flush items, and pending sends qualify.) If yes, add it to
   `HasPendingWork` in `interactive_requests.go` and add a test in
   `interactive_requests_test.go`.

`handleTurnComplete` is **idempotent** at logical-turn granularity
via `claimTurnSettlement`. The first complete drains streaming items,
and UPDATE-s the `turns` row. A second wire complete on the
already-settled logical turn folds late token usage onto the existing
row and otherwise no-ops. Turn token/cost accounting is captured on
the first completion, while context-window meter updates arrive
separately on `EventTokenUsage`. `setOpenTurn` clears the settled
marker so a re-init (Claude `system.init` resend after interrupt;
Codex `turn/started` resend) can re-settle the same turn.

Frontend `provider:turn_completed` emissions are gated INDEPENDENTLY
per wire round via `currentRoundByThread` / `takeOpenRound` (see
"Wire-round vs logical-turn cadence" above) — so a multi-result-per-
turn cascade emits one `turn_completed` per `result` envelope while
persistence stays at one settle per logical turn.

Cleanup paths:

- Per-turn state clears on `EventTurnComplete` (and on a matching
  error branch for errored turns).
- Per-thread state clears on `CleanupThread`.
- Approval and interrupt-queue entries clear when their correlated
  event resolves.

The one deliberate exception is the interrupt queue, which can span a
turn boundary because its contract is "persist queued events once the
interrupt lifts."

⚠ **Async settle bookkeeping**: `settleStreamingTextAsync` /
`settleStreamingThinkingAsync` move the heavy SQLite work off the
provider read-loop (content-block-stop is the freeze hot path). The
sync prelude (`takeActiveTextBlock` / `takeActiveThinkingBlock`) still
runs under `r.mu` to flip `activeTextBlocks` / `activeThinkingBlocks`
before returning, so duplicate settle attempts no-op even while the
goroutine is in flight. The `streamingItemCounts` decrement and
`drainInterruptQueueIfIdle` call BOTH live inside the goroutine
(`finishSettle`) so the count's `0 → drain` transition is durable —
incoming non-streaming rows queue correctly until the settle's persist
commits. `r.settleWG` tracks every fire-and-forget settle goroutine for
shutdown drain; `app.Close` blocks on `WaitForPendingSettles` with a
5s timeout so SQLite isn't torn down underneath an in-flight write.
`settleTurnStreaming` uses a per-turn `WaitGroup` to fan out per-scope
settles in parallel while still blocking on its caller (so the turns
row UPDATE sequences after all per-scope item writes).

## Raw chat content

Triage persists raw item summaries and raw payload data only. It must not
render markdown, ANSI, Mermaid, KaTeX, or code blocks. The frontend owns
chat rendering because it knows which rows are mounted and visible.

Streaming text/thinking rows create a row on first content, then emit all
timeline row mutations on the ordered `provider:item_event` channel:
`action=upsert` for row creation/lifecycle snapshots and `action=delta`
for follow-up raw text. SQLite receives the same raw text through the
stream persistence buffer. Do not split streaming text across separate UI
event channels, and do not add another rendered cache column or a
server-side kind-to-renderer dispatch table.

## Extension points

- To add routing for a new event kind: pick or create the matching
  `*_lifecycle.go` / `*_items.go` file, add a `Handle` switch case in
  `router.go`, write the routing-decision test FIRST. See
  `docs/architecture/how-to.md#add-a-new-event-kind`.
- To add a new persisted payload kind: extend `payload_items.go`,
  update `docs/architecture/schema.md`.

## Anti-patterns

- Do NOT cache store data here. No caching of store data. Transient
  correlation state only. Cross-turn derivation forbidden beyond the
  interrupt queue.
- Do NOT put preview content in the payload data blob. Meta is cheap,
  data is heavy — preview/stats in `meta`, full content in `data`.
- Do NOT combine or split events across boundaries. One event in, zero
  or more routing decisions out.
- Do NOT reach back into provider-specific types. If you need a detail
  the normalized event doesn't carry, fix the normalization upstream.

## Testing

- Every routing decision has a unit test with a representative event.
- When a new provider event type is added upstream, the routing
  decision is the first test — not the last.

## References

- `docs/architecture/data-flow.md` — end-to-end pipeline diagram.
- `docs/architecture/triage-routing.md` — detail on per-kind decisions.
- `docs/architecture/schema.md` — payload / item column reference.
