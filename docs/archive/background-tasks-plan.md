# Background tasks — cross-provider implementation plan

## Goal

Bring Codex background-terminal handling to parity with Claude's
backgrounding UX, add a cross-provider "Stop all" + per-item stop for
Claude, render `killed` as a distinct terminal status, and add
first-class support for Codex's "Waited for background terminal" cell.

## Verified primitives (ground truth)

- **Claude**: `stop_task` control_request over the existing stdio
  `--input-format stream-json --output-format stream-json` transport.
  Verified by spike on Claude CLI 2.1.112. Unified `task_id` namespace
  covers both `run_in_background` Bash and Task subagent. See
  `docs/references/claude-wire.md#stop_task`.
- **Codex**: `thread/backgroundTerminals/clean { thread_id }` —
  thread-wide termination of all unified-exec background PTYs. No
  per-process RPC; no client-side kill for `spawn_agent` child threads.
  See `docs/references/codex.md#known-upstream-constraints`.
- **Codex background-terminal signal**: `CommandExecution.source ==
  "unifiedExecStartup"`. Wire-typed, NOT a heuristic. Required by
  invariant 25.

## Locked decisions

- Claude tray rows get per-item Stop buttons (Claude has the primitive).
- Codex tray rows do NOT get per-item Stop. "Stop all" in the tray
  header covers Codex terminals via the thread-wide RPC.
- Codex subagents (`spawn_agent` child threads) appear in the tray but
  carry no Stop button — no client kill path exists.
- `killed` is a distinct status, rendered as a gray "Stopped" badge
  (separate from red "Failed" for `errored`).
- Completion row for backgrounded items lands at the **latest turn's
  tail** (not injected into the launching turn). Long-running Codex
  terminals can complete hours later; the row appears wherever the
  timeline's write-head is at completion time.
- Click-to-scroll on tray rows is removed; rows are informational.
- Backgrounded completions defer behind active streaming via the
  existing `maybeDeferOrPersist` path — completion rows don't inject
  into the middle of an assistant's streaming output.
- On app restart, any persisted `is_background=true AND status=running`
  rows for a Codex session flip to `errored`, `decision=lost` as the
  session starts (Codex subprocess died → PTYs died with it). Tray
  reconciler widens to include these.
- On thread delete: call `thread/backgroundTerminals/clean` as
  teardown. On thread fork: skip backgrounded running rows entirely
  (parent unaffected; fork doesn't carry running PTYs).
- Interrupt button: no behavior change. Background tasks keep running
  unless explicitly stopped.

## Phases

Each phase is committable on its own and must leave `go build`,
`go test ./...`, `npm run check`, and `npm run build` green. Each
phase is followed by a review-and-fix pass before commit.

### Phase 1 — Claude `stop_task` primitive + `killed` status

**Motivation**: Claude's primitive is fully verified and independent.
Ship it first so we can stop a real task in production while the rest
lands.

**Changes**:

1. `internal/provider/claude/session.go`: add `StopTask(ctx,
   taskID) error`. Writes a `control_request` line with
   `{"subtype":"stop_task","task_id":...}` to stdin. Allocates a
   unique `request_id`, awaits the matching `control_response`, times
   out after a reasonable window (existing IdleTimeout helpers are a
   model). Returns error on `subtype:"error"` response.
2. `internal/provider/claude/parse_control.go` + `session.go`: add
   dispatch for inbound `control_response` envelopes. Correlate by
   `request_id` to a per-session pending-request map
   (`pendingStopTasks`, analogous to the existing approval pattern).
   Unknown `request_id` logs and drops.
3. `internal/triage/tool_lifecycle.go`: `backgroundTerminalStatus` maps
   `patch.status:"killed"` to a new `statusKilled` constant
   (distinct from `statusErrored`). Add the constant.
4. `internal/store/migrate.go` v22: widen the items.status CHECK to
   include `'killed'`. Migration test pins the new enum value.
5. `app_claude_stop.go` (new): `StopClaudeTask(threadID, taskID) error`
   Wails binding. Looks up the active Claude session via
   `a.sessions[threadID]`, calls `session.StopTask(ctx, taskID)`.
   Returns typed errors for session-missing / provider-mismatch /
   timeout cases so the frontend can render them.
6. Unit + integration tests:
   - Session: outbound wire-shape; response correlation; error subtype
     handling; timeout.
   - Parser: `control_response` dispatch; unknown request_id drop path.
   - Triage: `killed` mapping distinct from `errored`; pair-atomic
     retention still works (the fix we shipped last week).
   - Migration v22: CHECK shape + enum value.
   - App binding: session-not-Claude error; session-missing error.

**Out of scope for Phase 1**: frontend rendering of the new status,
tray Stop button, `stop_task` for backgrounded Bash (same primitive,
but no Frontend button yet).

### Phase 2 — Codex background-terminal projection

**Motivation**: wire-typed classifier for Codex. Single source of
truth is `CommandExecution.source == "unifiedExecStartup"`. No
heuristics.

**Changes**:

1. `docs/architecture/invariants.md` §25: amend. Keep "no heuristic
   classifier"; add "wire-typed `UnifiedExecStartup` is the sanctioned
   signal." Update `internal/provider/codex/AGENTS.md` and
   `session.go:86-89` comment to match.
2. `internal/provider/codex/protocol.go`: persist `source`,
   `process_id`, (where present) on `EventToolStart` meta for
   `command_execution` ThreadItems. Round-trip test pinned.
3. `internal/triage/codex_background.go` (new): per-thread projector.
   - Tracks inProgress unifiedExec items + their turn_id.
   - On first **model-produced** event (`EventTextDelta`,
     `EventThinking`, `EventTurnComplete`) for the originating turn
     while such an item is still inProgress → stamp
     `is_background=true`, emit `provider:item_upsert` with the
     updated row.
   - Ignores `EventToolStart` for sibling parallel-batch tool calls
     (distinguished by same `turn_id` with no model output between).
4. `internal/triage/router.go`: wire the projector into the event
   dispatch. Cleanup path in `CleanupThread`.
5. **Completion sibling synthesis**: when `EventToolComplete` arrives
   for a backgrounded unifiedExec, construct a `tool_completion`
   sibling row (`kind=tool_completion`, `completion_of=launchID`,
   stable id `complete:<launchID>`). Use
   `triage/maybeDeferOrPersist` so mid-stream completions defer
   behind active text/reasoning blocks. Place at the thread's latest
   (turn_index, item_index) tail, not the launching turn.
6. **Subagents fold in**: `spawn_agent` tool_call rows that are still
   `running` past `turn/completed` also get `is_background=true`.
   Closure signal is the existing parent-notification path
   (subagent_notification XML tag or wait tool completion). Use same
   sibling-synthesis rule for the completion row.
7. Tests:
   - Projector unit tests (yield detection; parallel batch sibling
     skip; `EventTextDelta` vs `EventToolStart` discrimination;
     replay-from-resume dedup).
   - Sibling-synthesis tests (defer during streaming; tail placement;
     idempotent upsert via stable id).
   - Subagent folding tests.
   - Round-trip parser tests for `source` + `process_id` meta.

**Out of scope for Phase 2**: frontend rendering (tray already reads
is_background; should render automatically). Stop-all button.

### Phase 3 — Codex stop-all

**Motivation**: smallest Codex-specific stop affordance we have;
ships the Stop-all UX for Codex rows.

**Changes**:

1. `internal/provider/codex/session.go`: add
   `CleanBackgroundTerminals(ctx) error`. Sends
   `thread/backgroundTerminals/clean` with the session's thread_id
   via the existing `sendRequest` helper. Awaits the response.
2. `app_codex_background.go` (new): `CleanCodexBackgroundTerminals
   (threadID) error` Wails binding. Looks up the Codex session,
   calls `session.CleanBackgroundTerminals`.
3. Tests: session method with mock app-server (confirm RPC frame
   shape, response correlation); binding-level session-mismatch +
   session-missing error paths.

**Out of scope for Phase 3**: frontend button (lands in Phase 5).

### Phase 4 — Restart / teardown lifecycle

**Motivation**: don't leak PTYs; don't show ghost tray rows after
a restart; handle fork correctly.

**Changes**:

1. `app_codex_reconcile.go`: widen the SQL query to match the new
   projection (include `is_background=true AND status='running'`
   items regardless of whether the item is `tool_call` with
   `completion_of=''`). On every Codex session start (new OR resume),
   run the reconciler BEFORE subscribing to events; flip all matching
   rows to `errored`, `decision='lost'`, `summary + " — session
   ended"`. If Codex later replays a live `item/started` for one of
   them (rare warm-reconnect case), the existing parser dedup will
   re-upsert.
2. `app_thread_delete.go`: before teardown, if the thread is Codex,
   call `CleanCodexBackgroundTerminals` to terminate any running
   PTYs.
3. `app_thread_fork.go`: when forking, skip rows with
   `is_background=true AND status='running'` from the history copy.
   The forked thread starts clean; the parent thread is untouched.
4. Interrupt button: verify NO change needed. Write a test pinning
   that interrupting a turn leaves `is_background=true AND
   status='running'` rows alone.
5. Tests:
   - Reconcile-on-start flips ghost rows.
   - Thread-delete cleans PTYs.
   - Fork-exclusion (forked thread has no running bg rows; parent
     keeps them).
   - Interrupt-no-op on background rows.

### Phase 5 — Frontend tray UX

**Motivation**: surface all the backend capability as actual buttons.

**Changes**:

1. `frontend/src/lib/components/chat/BackgroundTaskTray.svelte`:
   - Remove `onRowClick` / click-to-scroll behavior. Rows become
     informational.
   - Add per-row "Stop" button when `row.meta.taskId` is available
     (Claude case). Button calls `StopClaudeTask(threadID, taskID)`.
   - Add "Stop all" button in the tray header. Dispatches:
     * For Claude rows: iterate, call `StopClaudeTask` per taskId.
     * For Codex rows: one `CleanCodexBackgroundTerminals(threadID)`.
   - Hide the "Stop all" button when tray contains only
     Codex-subagent rows (no kill path).
2. `frontend/src/lib/utils/backgroundTray.ts`: add `killed` to the
   `TrayTask['status']` union. Render gray "Stopped" badge separate
   from red "Failed".
3. `frontend/src/lib/components/chat/ToolCallCard.svelte` (or the
   equivalent inline rendering): render `killed` status with distinct
   styling from `errored`.
4. Tests: component tests for the new buttons, status rendering.

### Phase 6 — `TerminalInteraction` "Waited for background terminal"

**Motivation**: Codex-specific polish. When the model polls a
background terminal via `write_stdin` with empty input, Codex
emits `TerminalInteractionNotification`. Codex's TUI renders "Waited
for background terminal"; we should too.

**Changes**:

1. `internal/provider/provider.go`: add `EventTerminalInteraction`
   event kind.
2. `internal/provider/codex/protocol.go`: parse
   `TerminalInteractionNotification`; emit the event when `stdin ==
   ""` (polling case).
3. `internal/store/migrate.go` v23: add `'terminal_interaction'` to
   items.kind CHECK enum.
4. `internal/triage/router.go`: route `EventTerminalInteraction` to a
   dedicated persistence path; minimal row (kind, processID,
   timestamp).
5. `frontend/src/lib/components/chat/`: new minimal component
   rendering the waited marker inline in the timeline.
6. Tests: parser round-trip; triage persistence; store migration;
   frontend render.

### Phase 7 — Final review + integration tests

**Changes**:

1. End-to-end integration tests across phases:
   - Claude: spawn → stop per-item → stopped badge.
   - Claude: spawn → stop-all → all stopped.
   - Codex: spawn → yield → turn-continues → projected as
     background → complete → sibling row at tail.
   - Codex: spawn → stop-all → clean.
   - Codex: spawn → app restart → ghost rows flipped to errored.
2. Run `post-task-review` skill across all six lenses.
3. Fix any validated findings.
4. Final commit consolidating any cleanup.

## Test + build gates

Each phase must pass:

- `go build ./...`
- `go test ./...`
- `npm run check`
- `npm run build`

Failing any of these blocks the commit for that phase.

## Commit structure

One commit per phase. Each commit message names the phase and
summarizes the user-visible behavior change.

## Invariants added / amended during this work

- Invariant 25 amended: distinguishes wire-typed signal
  (`UnifiedExecStartup`) from heuristic classifiers.
- New invariant (TBD): sibling completion rows for Codex
  backgrounded items are synthesized by triage, not emitted by the
  wire.
- New invariant (TBD): `killed` status is terminal and distinct
  from `errored`; UI must render them differently.

## Out of scope / future

- Per-row stop for Codex background terminals (upstream protocol gap).
- Per-row stop for Codex `spawn_agent` children (no client RPC at all).
- Remote/web access to these controls (REMOTE.md deferred work).
