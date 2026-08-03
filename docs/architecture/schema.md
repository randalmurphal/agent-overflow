# SQLite Schema

Source of truth for the shape lives in `internal/store/schema_v1.go` (the
squashed v1 baseline) plus the forward-only chain in
`internal/store/migrate.go`. This page is the human-readable summary — if
it disagrees with the migrations, the migrations win.

## Tables

| Table | Purpose |
|---|---|
| `projects` | User-defined grouping of threads rooted at a directory. `path` (UNIQUE), `name`, immutable `slug` (UNIQUE, filesystem-safe; keys per-project app config under `<config-root>/projects/<slug>/`), `color`, `sort_position`, `archived`, `worktree_setup` (v46 — the project's worktree setup recipe as JSON: files copied from the main checkout and argv commands run at worktree creation; `''` means unconfigured, and a non-empty blob that does not strictly decode is an error rather than an empty recipe — see `internal/worktreesetup`). Each thread belongs to exactly one project. **Dead columns:** `workflow_queue_paused` and `workflow_concurrency` are leftovers from the removed workflow queue (workflows rev 2). SQLite refuses `DROP COLUMN` on a CHECK-bearing column, and rebuilding this FK-parent table to delete two unread integers is not worth the blast radius, so they stay physically present with their defaults. No Go code reads or writes them; `projectColumns` in `internal/store/projects.go` omits them. |
| `threads` | One row per conversation. Provider, session_ref, workspace/project paths, model, `mode` (chat/plan/design/discussion/terminal/workflow/workflow-studio/workflow-triage), `reasoning_effort`, `fast_mode`, `context_window`, per-tier auto-compact percentages (`auto_compact_standard_percent`, `auto_compact_extended_percent`), `runtime_mode` (`read-only` / `approval-required` / `auto-accept-edits` / `auto` / `full-access`; `read-only` added in v34 for workflow phases that declare `access: read-only`, `auto` — the AI-reviewed approval tier — in v45), archived flag, fork lineage (`parent_thread_id`, `pending_fork_session_ref`, `forked_from_thread_id`), discussion membership (`discussion_id`), `last_token_usage`, `worktree_setup_state` (v47 — `''` / `running` / `failed`, CHECK'd; the durable half of the per-project worktree setup run the thread's worktree was cut with, written by `SetThreadWorktreeSetupState` only, swept `running` → `failed` at startup by `SweepRunningThreadWorktreeSetups` because a run exists only inside a live process). Workflow-owned modes identify phase, studio, and triage threads so normal thread listings can exclude them without a naming convention. |
| `items` | Timeline items per thread. `turn_index`, `item_index`, `kind`, `role`, `status`, `summary` (always-loaded preview), `payload_id`, `parent_id` (subagent / nested-tool correlation), `is_background`, `completion_of` (back-reference from tool_completion to its launch), `tool_name`, `decision`, `meta`. |
| `payloads` | Heavy content. `kind`, `meta` (JSON, loaded with items), `data` (base BLOB, on-demand), plus persisted highlight-span blobs (migration v22): `preview_spans` (small, size-capped; joined into item list reads) and `spans` (full patch spans; read only by explicit payload loads). Span blobs are version-stamped and content-addressed — empty or stale means the frontend recomputes via the highlight RPCs. |
| `payload_chunks` | Append-only payload data for live streaming payloads. Rows are keyed by `(payload_id, chunk_index)` and carry `start_offset` so chunk reads can jump to the requested byte range. `ON DELETE CASCADE` keeps lifecycle owned by `payloads`. |
| `edit_file_snapshots` | Per-edit new-side file snapshots backing the review pane's Edits scope (migration v24). PK `(payload_id, path)`, `ON DELETE CASCADE` with `payloads` (the payload GC triggers cascade here). `content` is the gzip-compressed full file as it stood right after the edit's diff applied, written by the diff persist tap only when the just-edited workspace file provably matched the patch. Hunk-gap expansion and span priming resolve snapshot-first (`app_diff_context.go`), so historical edits stay expandable after the workspace drifts; absent rows (pre-feature history, size-capped writes) degrade to workspace verification. No backfill. |
| `channels` | Deliberation channels for multi-agent discussions. Belongs to a thread. |
| `channel_messages` | Ordered messages within a channel. `sequence`, `from_type`/`from_id`/`from_role`, `content`. |
| `discussion_definitions` | Reusable discussion templates. Scoped global or per-project. `UNIQUE(name, scope, project_id)`. |
| `design_artifacts` | Design-mode HTML artifacts. `html_path` points at the on-disk file. |
| `attachments` | Message attachments. `mime_type`, `size`, `relative_path` on disk. |
| `turns` | Per-turn records (one row per user → assistant round-trip). `turn_id` PK, `thread_id` FK, `turn_index`, `started_at`, nullable `completed_at` (NULL = in-flight; crash leftovers are boot-swept to `interrupted`), `stop_reason`, `assistant_message_id`, `token_usage_json`, `error_message`. |
| `message_anchors` | Per-real-user-message provider correlation written right after the message's `items` row persists (migration v23, replacing the removed `thread_checkpoints`). PK `(thread_id, user_item_id)`, cascade with `items`. `turn_index` plus Claude wire uuids (`provider_user_message_id`, `provider_parent_uuid`) let fork-from-message and revert-on-interrupt slice provider history at the message boundary. Pure SQLite — no git snapshots. Leftover `refs/agent-overflow/*` refs from the removed checkpoint machinery are not cleaned automatically; drain a repo with `git for-each-ref --format='%(refname)' refs/agent-overflow/ \| xargs -n1 git update-ref -d`. |
| `proposed_plans` | Per-plan state layered over proposed-plan payload items. Tracks immutable plan item id, thread id, revision parent item id, version, implementation marker, implementation thread/item ids, and timestamps. |
| `proposed_plan_comments` | Inline review comments anchored to one proposed-plan version. Tracks draft/sent/resolved status, line range, selected text, body, sent turn id, and timestamps. |
| `chat_bar_favorites` | Starred composer menu entries for models and discussion templates. Model favorites include provider + model id; discussion favorites store the discussion definition id. |
| `chat_model_profiles` | Last-used composer settings per provider/model: reasoning effort, fast mode, context window, per-tier auto-compact percentages, runtime mode, and `updated_at` for seeding new chats. Its `runtime_mode` CHECK is widened in lockstep with `threads` (v34 added `read-only`) because `rememberChatModelProfile` writes a thread's runtime mode back into the profile row. |
| `new_thread_mcp_defaults` | Provider/workspace-scoped MCP disabled-server snapshots for future chat/plan thread creation. Existing threads keep their own `threads.disabled_mcp_servers` snapshot. |
| `pending_background_task_terminals` | Per-task stash of Claude `task_updated` terminals whose chat-side `tool_completion` sibling has not been written yet. PK `(thread_id, task_id)`; carries `tool_use_id`, `status`, `exit_code`, `output_file`, `end_time`, `source` (`task_updated`), `created_at`. The tray query `ListLiveBackgroundTasks` joins against this table to hide launches whose host process exited but whose agent observation has not arrived. Drained when `task_notification` / TaskOutput observation lands. The startup sweep for recoverable Claude launches writes the `tool_completion` sibling directly (with `source="session_died"` recorded on the sibling's meta) and never stages a stash row. |
| `usage_ledger` | Append-only per-turn per-model token/cost accounting (migration v14; workflow attribution in v26). One row per (settled turn, model): `created_at`, attribution columns `thread_id` / `project_id` / `work_item_id` / `turn_id` / `provider` / `model` (denormalized, DELIBERATELY no FKs so lifetime totals survive thread/project deletion), token columns (`input_tokens` = non-cached input for both providers, `output_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`, `reasoning_output_tokens`), `cost_usd` (wire-reported only), `cost_source` (`wire`\|`none`). Values are per-turn deltas computed by the provider parsers — summing any slice of rows is safe. Written by `triage/usage_ledger.go`; aggregated by `store.QueryUsage`, with per-workflow-run totals from `store.QueryWorkItemUsage` and per-run-*tree* totals (a run plus every run it called, through a recursive walk of `work_items.parent_item_id`) from `store.QueryWorkItemTreeUsage` — the latter is what a budget ceiling is compared against. |
| `ui_state` | Persisted per-client UI view state (migration v15). PK `(scope, key)`; `scope` is an opaque namespace — `client:<uuid>` today, `user:<id>` reserved for when identities exist — and `value` is an opaque string (frontend JSON-encodes structured values). Exists because webview localStorage is not durable (the transport's ephemeral port changes the origin every launch). Backs the frontend `appStorage` module via the `GetUIState`/`SetUIState`/`DeleteUIState` bindings; hydrated once at boot. |
| `work_items` | Durable workflow run records (migration v26, extended v28/v29, rebuilt v33/v36/v38/v39/v43/v44): denormalized project/workflow identity, goal and seeds, frozen workflow snapshot, lifecycle state + typed reason, step mode, worktree/branch metadata, budget, source attribution, optional `triage_thread_id`, JSON disposition receipt + digest, call linkage, and timestamps. No project/thread FK: history outlives either row. v33 removed the queue: `sort_position` is gone, `state` no longer admits `'queued'` (surviving queued rows became `needs-human` / `interrupted`), and the three `sort_position`-ordered indexes were replaced by `created_at`-ordered ones — a run list has no manual order to preserve any more. v36 widened `reason` with `'unit-failed'`, the typed park a fan-out attempt takes when a unit does not complete. v38 added the call linkage a run tree is read through — `parent_item_id` / `parent_phase_id` / `parent_attempt` / `call_depth`, all empty/zero on a root run and constrained all-or-nothing by three CHECKs — widened `source` with `'call'` (a run a call phase created; nobody enqueued it) and `reason` with `'child-failed'`. A called run carries its caller's worktree/branch and never its own budget: the workspace flows down the call stack and the budget is enforced against the tree's root. v39 added `origin_thread_id` — the conversation thread a root run reports back into (D17): every resting transition composes one compact message and injects it there through the queued-user-message path. A fourth CHECK (`parent_item_id = '' OR origin_thread_id = ''`) makes "child runs never bind" structural rather than conventional, and the same rebuild widened `reason` with `'paused'`, the typed park reason a per-run pause and the graceful-quit pause-all take (D23). v43 added `parent_unit_id`, which narrows the linkage to one fan-out unit when the call was declared on a unit rather than on the phase (D35). It is empty for a phase call — a `shape: call` phase makes exactly one call per attempt, so (item, phase, attempt) already identifies it — and non-empty for a call-bound unit, whose siblings share that key. A fifth CHECK (`parent_unit_id = '' OR parent_item_id <> ''`) keeps it all-or-nothing like the rest of the linkage. A unit-called run carries *no* stamped workspace: isolation is introduced by fan-out, so the runner resolves that unit's sub-worktree through this column. v44 added `soft_stop` (0/1) and widened `reason` with `'checkpoint'` (D36): the standing request to stop a run tree at its next call boundary, and the typed park a boundary takes when it honours one. Only a root run ever carries the flag — a tree is stopped as a tree, like pause — and the engine's command loop is its only writer, because the boundary that fires also clears it. |
| `work_item_phases` | One row per workflow phase attempt, unique on `(item_id, phase_id, attempt)`. Stores denormalized phase-thread id, input/output envelopes, evaluated gate trace, intervention record, narrative path, status, and timestamps. No run/thread FK; loop counts derive from attempt rows. |
| `work_item_units` | One row per fan-out unit (and per join) of one phase attempt (migration v35), unique on `(item_id, phase_id, attempt, unit_id)`. Rows are written when the attempt expands — pending — not when a unit finishes, so a sub-worktree is discoverable the moment it exists and an attempt's width survives a crash. Carries `unit_index` (launch order), `kind` (`unit`\|`join`), the unit's own provider/model, the thread / branch / sub-worktree / narrative path the runner registered, `status` (`pending`\|`running`\|`done`\|`failed`\|`dropped`\|`taken-over`), `unit_attempt` (per-unit retry counter — a retried unit reuses its row), the unit's control envelope, a feedback note, and timestamps. No run/thread FK, like the rest of the workflow tables. The runner registers `branch` + `worktree_path` before the unit's session starts, and clears `worktree_path` (keeping `branch`) when a done join retires the checkout it consumed. A join's row carries the same thread the phase attempt row does, because the join's envelope is the phase's. |
| `work_item_effects` | Surface-and-skip ledger for the side effects a phase's first-party CLI grants fire (spec §5): one row per `(item_id, phase_id, tool, payload_hash)` — a table-level `UNIQUE` — with the JSON payload and creation time. Loop-back, take-over re-run, and crash-recovery all re-enter a phase, so the second identical call finds its own prior row and returns that effect instead of firing again (`agent-overflow run start` exits 0 with the run it already started). No run FK so history remains independently durable. |
| `automations` | Scheduled/internal-event workflow definitions: project and workflow identity/scope, name, enable flag, trigger/condition JSON, seed template, continuity notes, and timestamps. Migration v40 adds the fire record the scheduler writes: `last_fired_at` + `last_run_item_id` when a fire started a run, and `skip_count` + `last_skip_at` + `last_skip_reason` when one was refused (already running, condition false or unevaluable, self-chain, start failure). Both writes deliberately leave `updated_at` untouched — a fire is not a definition change. The trigger JSON is one of `{"kind":"cron","expr":"<5 fields>"}` or `{"kind":"event","on":"item-done"\|"item-failed"\|"item-needs-human","workflowId":"<optional>"}`, parsed on write *and* on every scheduler load so a row that stopped parsing surfaces as broken instead of as a schedule that never fires; the condition is a `def.Predicate` evaluated against the run's own seed context. |
| `automation_cursors` | Per-automation source watermarks keyed by `(automation_id, source_key)`. Cursors cascade with their owning automation; they are scheduler state, not run history. |

Plan implementation and revision source references are stored on the user
message `items.meta` as `sourceProposedPlan` and
`revisionSourceProposedPlan`. The proposed-plan tables stay as durable
per-plan state, while accepted turns reconcile those metadata references into
implementation markers and revision parent links.

## Always-Loaded vs On-Demand

- `threads` — list, always loaded for sidebar.
- `items` — loaded per visible thread. `summary` is raw always-loaded
  text, except for deliberately collapsed heavy rows such as thinking.
- `payloads.meta` — loaded alongside items (JSON preview/stats).
- `payloads.preview_spans` — loaded alongside items (small span blobs
  for inline diff previews; capped at write time).
- `payloads.data` + `payload_chunks.data` — composed on demand when the
  user expands, copies, or saves a heavy payload. `payloads.spans` rides
  along with those explicit payload reads.

## Key Indexes

- `idx_threads_updated` — sidebar sort.
- `idx_projects_slug` — enforces the stable, unique filesystem-safe project slug used by per-project app config directories.
- `idx_threads_project` — per-project thread list.
- `idx_items_thread` — load thread timeline.
- `idx_items_parent` — group subagent / nested-tool items under a parent (partial index on non-empty `parent_id`). The subagent descendant CTE writes an explicit `parent_id <> ''` term so the planner can prove the index predicate — see `descendantsCTEFromRoots`.
- `idx_items_completion_of` — pair a `tool_completion` row with its launch (partial index on non-empty `completion_of`).
- `idx_payload_chunks_payload_start` — seek chunk-backed payload reads by payload id and byte offset.
- `idx_threads_forked_from` — fork lineage walks.
- `idx_channels_thread`, `idx_design_artifacts_thread` — per-thread feature lookups.
- `turns_thread_index` on `turns(thread_id, turn_index DESC)` — backs `ListRecentTurns` for the newest-first rehydration the frontend runs on thread-switch.
- `idx_turns_thread_completed` on `turns(thread_id, completed_at DESC) WHERE completed_at IS NOT NULL` — backs sidebar read-state checks against the newest completed turn.
- `idx_turns_inflight` on `turns(thread_id, turn_index) WHERE completed_at IS NULL` — keeps the boot-time crashed-turn sweep (`RecoverCrashedTurns`) O(in-flight rows) instead of a full `turns` scan.
- `message_anchors PRIMARY KEY(thread_id, user_item_id)` — one anchor per real user message; backs message-keyed fork/rollback lookups.
- `idx_message_anchors_thread_turn` on `message_anchors(thread_id, turn_index)` — backs turn-boundary anchor resolution for provider rollback.
- `idx_proposed_plans_thread_version` on `proposed_plans(thread_id, version DESC)` — backs newest-first plan sidebar/history queries.
- `idx_proposed_plan_comments_plan` on `proposed_plan_comments(thread_id, plan_item_id, status, start_line, created_at)` — backs per-plan review comment listing and draft/sent counts.
- `idx_chat_bar_favorites_created` on `chat_bar_favorites(created_at DESC)` — backs newest-first favorite listing in the composer menu.
- `idx_chat_model_profiles_updated` on `chat_model_profiles(updated_at DESC)` — backs latest-profile seeding for new chats.
- `idx_pending_terminals_tool_use` on `pending_background_task_terminals(thread_id, tool_use_id) WHERE tool_use_id <> ''` — partial index backing the tray query's `NOT EXISTS` join. The PK on `(thread_id, task_id)` already covers thread-prefix lookups.
- `idx_usage_ledger_created` on `usage_ledger(created_at)` — time-range usage aggregation.
- `idx_usage_ledger_thread` on `usage_ledger(thread_id, created_at)` — per-thread usage aggregation.
- `idx_usage_ledger_work_item` on `usage_ledger(work_item_id, created_at)` — per-run token and wire-cost budget aggregation. A budget is enforced against the whole run *tree*, so the recursive `parent_item_id` walk drives one lookup per run through this index.
- `idx_usage_ledger_project_work_item` on `usage_ledger(project_id, work_item_id) WHERE work_item_id <> ''` (migration v30) — project-scoped workflow usage without scanning ordinary-thread rows.
- `idx_work_items_project_state_created` on `work_items(project_id, state, created_at)` — filtered run listings in chronological order.
- `idx_work_items_project_created` on `work_items(project_id, created_at)` — full project run listings when no state filter is applied.
- `idx_work_items_state_created` on `work_items(state, created_at, id)` — global unresolved-run counts without scanning retained history.
- `idx_work_items_agent_source_ref` — UNIQUE on `work_items(source_ref) WHERE source = 'agent' AND source_ref <> ''` (migration v31): one run per agent-originated source reference, enforced by the database rather than by the start path checking first. It is what makes a re-entered phase's repeated `agent-overflow run start` a surfaced prior effect instead of a race between two identical starts.
- `idx_work_items_triage_thread` on non-empty `work_items(triage_thread_id)` — item hand-off identity and project triage-shell exclusion.
- `idx_work_items_parent` on `work_items(parent_item_id, parent_phase_id, parent_attempt) WHERE parent_item_id <> ''` (migration v38) — the run tree read downward: the children of one run, and the child one call attempt created. Queries pair `parent_item_id = ?` with `parent_item_id <> ''` so the partial index is usable; a bound parameter alone cannot prove the predicate.
- `idx_work_items_origin_thread` on non-empty `work_items(origin_thread_id)` (migration v39) — the inverse of the wake lookup: every run bound to one thread, which is what clearing a deleted thread's bindings walks.
- `idx_work_item_phases_thread` on non-empty `work_item_phases(thread_id, started_at DESC, attempt DESC)` — phase-thread ownership lookup for takeover sends.
- `idx_work_item_phases_item_started` on `work_item_phases(item_id, started_at, phase_id, attempt)` — chronological run-detail and phase-attempt reads.
- `idx_work_item_units_attempt` on `work_item_units(item_id, phase_id, attempt, unit_index)` — one fan-out attempt's units in launch order (scheduling, join results, recovery).
- `idx_work_item_units_worktree` on non-empty `work_item_units(item_id, worktree_path)` — sub-worktree ownership lookup for a run's fan-out units.
- `idx_work_item_units_thread` on non-empty `work_item_units(thread_id)` (migration v37) — resolves the unit that owns an AO thread, which is what a send into a fan-out unit's thread (human steering of one unit) starts from. It mirrors `idx_work_item_phases_thread` for the unit half of the same lookup.
- `idx_automations_project` on `automations(project_id, created_at)` — project automation listings.
- `idx_work_items_automation_source_ref` on `work_items(source_ref, state) WHERE source = 'automation' AND source_ref <> ''` (migration v40) — the skip-if-running probe: does this automation already have a run that is `running` or `needs-human`. Queries repeat `source_ref <> ''` so the partial index is usable.

## Migration Policy

- Migrations are numbered, forward-only, append-only. Never edit a migration
  that has shipped; add a new one.
- SQLite check constraints (see `CHECK(mode IN ...)`, `CHECK(provider IN ...)`,
  `CHECK(runtime_mode IN ...)`) are the recommended way to enforce enums.
- Test every migration: each migration must have a corresponding test under
  `internal/store/` that proves the expected schema state.
- WAL mode is verified on startup (not just requested). If `journal_mode=WAL`
  didn't take, the app logs a visible warning and continues — the store is
  still correct under rollback journaling, but the warning is the only
  signal that SQLite concurrency has degraded. See invariant 19.

## What Goes in SQLite vs What Doesn't

- **In**: timeline items, payloads, thread metadata, projects, channels/messages,
  discussion templates, design artifact metadata, attachment metadata, composer
  favorites, last-used model profile seeds, workflow run records, and automation
  definitions/cursors.
- **Not in**: live per-turn provider state (the provider owns it),
  transient UI state (frontend $state), logs (observability package has
  its own NDJSON logger).

If you find yourself reaching for a new table, first ask whether the
provider process already owns the answer.

## `turns`

Per-turn records. One row per user → assistant round-trip.

Columns:
- `turn_id` TEXT PK — provider-assigned opaque id (Claude's session-scoped id / Codex's turnId)
- `thread_id` FK threads(id)
- `turn_index` INTEGER — monotonic per thread, caller-assigned
- `started_at` INTEGER (ms) — turn-start wire event timestamp
- `completed_at` INTEGER (ms, nullable) — NULL = in-flight right now (crash
  leftovers are settled as `interrupted` by the boot sweep, so NULL never
  survives an app restart)
- `stop_reason` TEXT — end_turn / max_tokens / tool_use / stop_sequence / refusal / error / interrupted
- `assistant_message_id` TEXT — provider-derived final assistant
  message id when available. Claude derives it from the last in-stream
  assistant `message.id`; current Codex `turn/completed` does not
  carry one.
- `token_usage_json` TEXT — the turn's PER-TURN usage delta (aggregate
  across models; JSON shape of `provider.TokenUsage`). First non-empty
  write wins across multi-result settles; the per-model split lands in
  `usage_ledger` instead.
- `error_message` TEXT — populated when stop_reason indicates error

Indexes:
- `turns_thread_index` on (thread_id, turn_index DESC) for ListRecentTurns
- `idx_turns_thread_completed` on (thread_id, completed_at DESC) where completed_at is not NULL for sidebar read-state
- `idx_turns_inflight` on (thread_id, turn_index) where completed_at IS NULL — keeps the boot-time crashed-turn sweep O(in-flight rows)

Rules:
- Inserted at turn-start with `completed_at=NULL`; updated at turn-complete.
- NEVER auto-close a NULL row while the owning session may be alive. The
  only sanctioned settle-without-wire-event paths are triage's synthesized
  truncated turn-complete (in-app session death) and the boot sweep
  `RecoverCrashedTurns` (app died mid-turn; runs before any session can
  spawn). Both settle with `stop_reason='interrupted'`, which is the durable
  signal behind the sidebar's Interrupted pill.
- `turn_index` is assigned by the caller (triage), not by the store.

See `docs/architecture/turn-lifecycle.md` for the full lifecycle rules.
