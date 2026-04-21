# Turn-Lifecycle Refactor Plan

**Status**: archived execution plan. Refer to this while the work is
in flight; once shipped, the living documentation is:

- [`docs/architecture/turn-lifecycle.md`](../architecture/turn-lifecycle.md)
  — the three-lifecycle mental model
- [`docs/references/claude-wire.md`](../references/claude-wire.md) /
  [`codex-wire.md`](../references/codex-wire.md) — wire shapes
- [`docs/architecture/invariants.md`](../architecture/invariants.md)
  — authoritative guardrails

## Problem

The current architecture conflates three independent lifecycles (tool,
task, turn), resulting in:

1. **TaskOutput `tool_call` rows stuck at `running` forever** — the
   parser consumes TaskOutput's `tool_result` and emits a completion
   for the backgrounded task's `tool_use_id` instead of TaskOutput's
   own. See `parse_user.go:67-112` (old behavior).
2. **Working indicator stuck on forever** — `isTurnActive` is derived
   from `items.some(running tool_call)`; a single stuck tool pins the
   indicator. See `frontend/src/lib/stores/thread.svelte.ts:141-149`.
3. **No turn-complete signal on the wire pushed to the frontend** —
   `emitInline` in `internal/triage/router.go:790-793` is a no-op.
   Turn-complete is currently inferred rather than pushed.
4. **Backgrounded tools render identically to actively running ones**
   — `ToolCallCard.svelte` has no visual distinction for
   `is_background`.
5. **No final-message separator** in the timeline marking turn end.
6. **Codex `BackgroundClassifier` stamps `is_background` via a wrong
   heuristic** — Codex has no `run_in_background` concept, so no
   Codex tool should be marked as backgrounded.

## Solution: three independent lifecycles

See [`turn-lifecycle.md`](../architecture/turn-lifecycle.md) for the
mental model. TL;DR:

- **Tool lifecycle** — every `tool_use` gets a matching completion
  for its own id. Universal invariant. No exceptions (backgrounded
  tools' placeholders still emit completion; triage handles the
  "keep running" per-spec behavior).
- **Task lifecycle** — Claude-only, layered on top. `task_updated`
  terminal OR TaskOutput enrichment → `EventBackgroundTaskTerminal`
  → idempotent `tool_completion` sibling row. `task_notification`
  dropped.
- **Turn lifecycle** — wire-pushed. `result` / `turn/completed` →
  `EventTurnComplete` → frontend `provider:turn_completed`. New
  `turns` table. Force-close orphan running tool_calls as safety net.

## Data model changes

### New table: `turns` (migration v24)

```sql
CREATE TABLE turns (
  turn_id TEXT PRIMARY KEY,
  thread_id TEXT NOT NULL REFERENCES threads(id),
  turn_index INTEGER NOT NULL,
  started_at INTEGER NOT NULL,
  completed_at INTEGER,
  stop_reason TEXT,
  assistant_message_id TEXT,
  token_usage_json TEXT,
  error_message TEXT
);
CREATE INDEX turns_thread ON turns(thread_id, turn_index DESC);
```

- `completed_at = NULL` means in-flight or crashed mid-turn.
- `stop_reason` values: `end_turn | max_tokens | tool_use | stop_sequence | refusal | error | interrupted` (plus provider-specific extensions with forward-compat treatment).
- `turn_index` is monotonically increasing per thread.

### No `tasks` table

Model B locked — items table stays the single source of truth. The
sibling `tool_completion` row pattern handles the task lifecycle.

## New event shapes

### Internal (Go)

| Event | When | Key fields |
|---|---|---|
| `EventBackgroundTaskTerminal` (new) | `task_updated` terminal or TaskOutput | `task_id`, `tool_use_id`, `status`, `exit_code`, `output_file`, content |
| `EventTurnStart` (wire emission new) | `SendMessage` (Claude) or `turn/started` (Codex) | `turn_id`, `thread_id`, `started_at` |
| `EventTurnComplete` (wire emission new) | `result` (Claude) or `turn/completed` (Codex) | `turn_id`, `thread_id`, `completed_at`, `stop_reason`, `assistant_message_id`, `token_usage` |

### Frontend (Wails events)

| Event | Payload | Consumer |
|---|---|---|
| `provider:turn_started` | `{threadId, turnId, startedAt}` | `events.ts` → pane.setActiveTurn |
| `provider:turn_completed` | `{threadId, turnId, startedAt, completedAt, stopReason, assistantMessageId, tokenUsage}` | `events.ts` → pane.settleTurn |

## Execution: three waves

### Wave 1 — foundational (parallel worktrees)

Dispatch three agents in isolated worktrees. They don't touch each
other's files.

#### WT-schema
**Scope**: migrations + store accessors for `turns` table.
**Files**:
- `internal/store/migrate.go` — add v24 migration + constraint tests
- `internal/store/turns.go` — NEW: `InsertTurn`, `UpdateTurnCompleted`, `ListRecentTurns(threadID, limit)`, `GetActiveTurn(threadID)`
- `internal/store/turns_test.go` — NEW

**Out of scope**: anything in `internal/triage/`, `internal/provider/`,
or frontend.

**Success criteria**:
- `go test ./internal/store/...` passes
- Migration idempotent under `migrate_test.go` re-run harness
- Schema doc updated (`docs/architecture/schema.md`)

#### WT-claude-parser
**Scope**: refactor `internal/provider/claude/` so every `tool_use`
gets exactly one `EventToolComplete` for its own id, task lifecycle
is separately `EventBackgroundTaskTerminal`, and `task_notification`
is dropped.
**Files**:
- `internal/provider/claude/parse_user.go` — drop short-circuit;
  always run standard completion for own id; emit
  `EventBackgroundTaskTerminal` in addition when TaskOutput shape
  matches
- `internal/provider/claude/parse_system.go` —
  `parseTaskLifecycleEvent` emits new event type;
  `parseTaskNotificationEvent` returns nil (keeps parser map
  bookkeeping for dedup guards the adapter still needs, but emits
  nothing)
- `internal/provider/claude/parser.go` — collapse correlation state:
  keep `backgroundToolUses` (spec-facing) and `taskToolUses`
  (task_id↔tool_use_id), remove `completedTasks`,
  `taskOutputTasks`, `completedToolUseIDs` maps (dedup moves to
  triage via idempotent upsert)
- `internal/provider/claude/protocol_test.go` — update existing
  tests to the new event split; add tests using
  `/tmp/claude-bg-spike/*.ndjson` as fixtures
- `internal/provider/provider.go` — add `EventBackgroundTaskTerminal`
  kind constant

**Out of scope**: triage or frontend changes.

**Success criteria**:
- Replaying each captured sample produces the expected event
  sequence (documented in a table in the test file)
- All existing `protocol_test.go` cases pass (with updated
  expectations for the event split)
- TaskOutput test scenario: exactly 2 events emitted for a
  TaskOutput tool_result — one `EventToolComplete(TaskOutput.id)`
  and one `EventBackgroundTaskTerminal(task_id)`

#### WT-codex-parser
**Scope**: remove incorrect backgrounding stamps; fix `wait` enum;
parse `<subagent_notification>` (emit a new event, UI surface
deferred).
**Files**:
- `internal/provider/codex/protocol.go` — route `"wait"` to a
  distinct itemType (`"wait_agent"`); surface `agentsStates` in
  `enrichItemMeta`
- `internal/provider/codex/background.go` — neuter classifier for
  Codex so no Codex tool gets `is_background=true`
- `internal/provider/codex/session.go` — detect
  `<subagent_notification>` tags in user-message items, emit a new
  internal event (`EventSubagentNotification`); **rendering is
  deferred — the emission is a no-op UI-wise for now**
- Tests updated accordingly

**Out of scope**: Claude, triage, frontend.

**Success criteria**:
- Existing Codex tests pass; new tests for the wait enum fix and
  neutered classifier added
- No Codex tool appears with `is_background=true` in integration tests

### Wave 2 — wiring (sequential, depends on Wave 1)

#### WT-triage
**Depends on**: all of Wave 1 merged.
**Scope**: route new events; write `turns` rows; emit frontend
events.
**Files**:
- `internal/triage/tool_lifecycle.go` — add
  `handleBackgroundTaskTerminal` that idempotent-upserts the
  `tool_completion` sibling via `AppendCompletionItem`
- `internal/triage/turn_lifecycle.go` —
  `handleTurnStart` → insert turn row + `emitWithReplay("provider:turn_started", ...)`;
  `handleTurnComplete` → update turn row + emit + force-close orphan
  running non-background tool_calls in the current turn
- `internal/triage/router.go` — remove `emitInline` placeholder;
  wire the new emission path through the existing
  `emitWithReplay`
- Tests

**Out of scope**: frontend changes.

**Success criteria**:
- Force-close behavior: a `tool_call` row with
  `status='running' && !is_background && turn_index=current` at
  turn-complete gets status flipped to `errored` with
  synthesized completion summary "turn ended with tool unresolved"
- Idempotent sibling upsert: `task_updated` + TaskOutput for same
  task produces a single `tool_completion` row with the richer
  (TaskOutput) payload winning
- Turn events emitted for both Claude and Codex in integration
  tests

#### WT-frontend-turn-state
**Depends on**: WT-triage merged (for `provider:turn_*` events).
**Scope**: pane state + listeners + bindings.
**Files**:
- `frontend/src/lib/stores/thread.svelte.ts` — add `activeTurn`,
  `latestSettledTurn`; replace `isTurnActive` derivation with
  `activeTurn !== null`; add `setActiveTurn` / `settleTurn`
  mutations
- `frontend/src/lib/stores/events.ts` — listeners for
  `provider:turn_started` / `provider:turn_completed`
- `app_turns.go` — NEW: Wails bindings `ListRecentTurns(threadId)`
- `frontend/src/lib/stores/bindings.ts` — wrap new binding
- `thread.svelte.ts` `switchThread` — hydrate
  `latestSettledTurn` from `ListRecentTurns(threadId, 2)` taking
  the most recent `completed_at != null` row
- Tests

**Out of scope**: UI component changes (Wave 3).

**Success criteria**:
- Thread-switch hydration pulls the last settled turn
- Active-turn push events set `activeTurn`; completion pushes
  settle it and clear active
- `isTurnActive` derived from `activeTurn !== null` in tests

### Wave 3 — UI (parallel, depends on Wave 2)

#### WT-ui-working-timer
**Scope**: `ChatWorkingIndicator.svelte` reads `pane.activeTurn.startedAt`;
becomes self-ticking.

#### WT-ui-completion-divider
**Scope**: `MessageTimeline.svelte` renders divider before the item
whose id matches `pane.latestSettledTurn.assistantMessageId`. Label
`"Response • Worked for Xs · Yk tokens"`.

#### WT-ui-backgrounded-badge
**Scope**: `ToolCallCard.svelte` adds blue `"…"` badge for
`isBackground && status === 'running'`.

### Review cycle

After Wave 3, parallel review agents on:
- **Correctness** — invariant compliance, edge cases
- **Test coverage** — all new paths covered, existing not regressed
- **Docs** — claude-wire, codex-wire, turn-lifecycle, invariants up
  to date
- **UX polish** — does the working indicator + divider render
  correctly in real sessions?
- **Code hygiene** — no commented-out code, dead imports, sprawling
  files

Fix + cycle until clean.

## Test fixtures

**Authoritative captured wire samples**:

- `/tmp/claude-bg-spike/ndjson_bash.log` — backgrounded + foreground
  Bash + Read
- `/tmp/claude-bg-spike/ndjson_task.log` — backgrounded Task subagent
  + TaskOutput
- `/tmp/claude-bg-spike/ndjson_outlives.log` — backgrounded task
  outliving its turn
- `/tmp/taskoutput-multi-spike/ndjson.log` — two parallel bg Bashes
  + blocking TaskOutput on one

Use these directly in parser tests (do not copy into the repo —
reference by `/tmp/...` path so they can be refreshed from
`AGENT_OVERFLOW_DEBUG=provider` runs).

## Invariants added (to `docs/architecture/invariants.md`)

1. **Tool lifecycle**: every `tool_use` emits exactly one
   `EventToolStart` and exactly one `EventToolComplete` keyed by its
   own `tool_use_id`. No ID rewriting, no consumption by other
   handlers.
2. **`task_notification` is not a completion source**: dropped at the
   parser layer.
3. **Turn-active is wire-pushed**: `isTurnActive` derives only from
   `pane.activeTurn !== null`. Never from items.
4. **Turn-complete force-closes orphan tool_calls**: triage flips any
   `status='running' && !is_background && turn_index=current` rows
   to `errored` at turn-complete.
5. **Backgrounded work outlives turns**: a backgrounded tool_call can
   be `running` after its launching turn completes. This is expected.
6. **Codex has no backgrounded tools**: no Codex tool gets
   `is_background=true`. Sibling-row model is Claude-specific.

## Success criteria (whole refactor)

1. Real repro: run `sleep 10 + TaskOutput` scenario in the app;
   TaskOutput tool_call row flips to `completed`; turn completes;
   "Working…" indicator stops; final-message separator appears with
   "Worked for Xs · Yk tokens".
2. All captured NDJSON samples replay through parser tests producing
   the documented event sequence.
3. `go test ./...` passes; `cd frontend && npm test` passes;
   `npm run check` and `npm run build` pass.
4. Docs accurate — capture any deviations from the reference docs
   (this plan + invariants + wire refs) and update.
