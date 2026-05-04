# SQLite Schema

Source of truth for the shape lives in `internal/store/migrations/`. This
page is the human-readable summary — if it disagrees with the migrations,
the migrations win.

## Tables

| Table | Purpose |
|---|---|
| `projects` | User-defined grouping of threads rooted at a directory. `path` (UNIQUE), `name`, `color`, `sort_position`, `archived`. Each thread belongs to exactly one project. |
| `threads` | One row per conversation. Provider, session_ref, workspace/project paths, model, `mode` (chat/plan/design/discussion), `reasoning_effort`, `fast_mode`, `context_window`, per-tier auto-compact percentages (`auto_compact_standard_percent`, `auto_compact_extended_percent`), `runtime_mode`, archived flag, fork lineage (`parent_thread_id`, `pending_fork_session_ref`, `forked_from_thread_id`), discussion membership (`discussion_id`), `last_token_usage`. |
| `items` | Timeline items per thread. `turn_index`, `item_index`, `kind`, `role`, `status`, `summary` (always-loaded preview), `payload_id`, `parent_id` (subagent / nested-tool correlation), `is_background`, `completion_of` (back-reference from tool_completion to its launch), `tool_name`, `decision`, `meta`. |
| `payloads` | Heavy content. `kind`, `meta` (JSON, loaded with items), `data` (BLOB, on-demand). |
| `channels` | Deliberation channels for multi-agent discussions. Belongs to a thread. |
| `channel_messages` | Ordered messages within a channel. `sequence`, `from_type`/`from_id`/`from_role`, `content`. |
| `discussion_definitions` | Reusable discussion templates. Scoped global or per-project. `UNIQUE(name, scope, project_id)`. |
| `design_artifacts` | Design-mode HTML artifacts. `html_path` points at the on-disk file. |
| `attachments` | Message attachments. `mime_type`, `size`, `relative_path` on disk. |
| `turns` | Per-turn records (one row per user → assistant round-trip). `turn_id` PK, `thread_id` FK, `turn_index`, `started_at`, nullable `completed_at` (NULL = in-flight or crashed), `stop_reason`, `assistant_message_id`, `token_usage_json`, `error_message`. |
| `thread_checkpoints` | Git checkpoint metadata per thread. `checkpoint_turn_count` is the canonical boundary: `0` is before the first turn, `N` is after completed turn `N`. Rows also carry `status`, compact `files` JSON, `tool_paths`, `captured_at`, `ref_name`, and `workspace_path`. |
| `proposed_plans` | Per-plan state layered over proposed-plan payload items. Tracks immutable plan item id, thread id, revision parent item id, version, implementation marker, implementation thread/item ids, and timestamps. |
| `proposed_plan_comments` | Inline review comments anchored to one proposed-plan version. Tracks draft/sent/resolved status, line range, selected text, body, sent turn id, and timestamps. |
| `chat_bar_favorites` | Starred composer menu entries for models and discussion templates. Model favorites include provider + model id; discussion favorites store the discussion definition id. |
| `chat_model_profiles` | Last-used composer settings per provider/model: reasoning effort, fast mode, context window, per-tier auto-compact percentages, runtime mode, and `updated_at` for seeding new chats. |
| `pending_background_task_terminals` | Per-task stash of Claude `task_updated` terminals whose chat-side `tool_completion` sibling has not been written yet. PK `(thread_id, task_id)`; carries `tool_use_id`, `status`, `exit_code`, `output_file`, `end_time`, `source` (`task_updated`), `created_at`. The tray query `ListLiveBackgroundTasks` joins against this table to hide launches whose host process exited but whose agent observation has not arrived. Drained when `task_notification` / TaskOutput observation lands. The startup sweep for orphaned launches writes the `tool_completion` sibling directly (with `source="session_died"` recorded on the sibling's meta) and never stages a stash row. |

Plan implementation and revision source references are stored on the user
message `items.meta` as `sourceProposedPlan` and
`revisionSourceProposedPlan`. The proposed-plan tables stay as durable
per-plan state, while accepted turns reconcile those metadata references into
implementation markers and revision parent links.

## Always-Loaded vs On-Demand

- `threads` — list, always loaded for sidebar.
- `items` — loaded per visible thread. `summary` is raw always-loaded
  text or a bounded raw preview; full body lives in the linked payload.
- `payloads.meta` — loaded alongside items (JSON preview/stats).
- `payloads.data` — BLOB, fetched only when the user expands.

## Key Indexes

- `idx_threads_updated` — sidebar sort.
- `idx_threads_project` — per-project thread list.
- `idx_items_thread` — load thread timeline.
- `idx_items_parent` — group subagent / nested-tool items under a parent (partial index on non-empty `parent_id`).
- `idx_items_completion_of` — pair a `tool_completion` row with its launch (partial index on non-empty `completion_of`).
- `idx_threads_forked_from` — fork lineage walks.
- `idx_channels_thread`, `idx_design_artifacts_thread` — per-thread feature lookups.
- `turns_thread_index` on `turns(thread_id, turn_index DESC)` — backs `ListRecentTurns` for the newest-first rehydration the frontend runs on thread-switch.
- `idx_turns_thread_completed` on `turns(thread_id, completed_at DESC) WHERE completed_at IS NOT NULL` — backs sidebar read-state checks against the newest completed turn.
- `idx_thread_checkpoints_thread_count_unique` on `thread_checkpoints(thread_id, checkpoint_turn_count)` — enforces one checkpoint per thread boundary and backs checkpoint drawer listing, range diff, and revert lookups.
- `idx_proposed_plans_thread_version` on `proposed_plans(thread_id, version DESC)` — backs newest-first plan sidebar/history queries.
- `idx_proposed_plan_comments_plan` on `proposed_plan_comments(thread_id, plan_item_id, status, start_line, created_at)` — backs per-plan review comment listing and draft/sent counts.
- `idx_chat_bar_favorites_created` on `chat_bar_favorites(created_at DESC)` — backs newest-first favorite listing in the composer menu.
- `idx_chat_model_profiles_updated` on `chat_model_profiles(updated_at DESC)` — backs latest-profile seeding for new chats.
- `idx_pending_terminals_tool_use` on `pending_background_task_terminals(thread_id, tool_use_id) WHERE tool_use_id <> ''` — partial index backing the tray query's `NOT EXISTS` join. The PK on `(thread_id, task_id)` already covers thread-prefix lookups.

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
  signal that checkpointing isn't happening. See invariant 19.

## What Goes in SQLite vs What Doesn't

- **In**: timeline items, payloads, thread metadata, projects, channels/messages,
  discussion templates, design artifact metadata, attachment metadata, composer
  favorites, and last-used model profile seeds.
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
- `completed_at` INTEGER (ms, nullable) — NULL = in-flight or crashed mid-turn
- `stop_reason` TEXT — end_turn / max_tokens / tool_use / stop_sequence / refusal / error / interrupted
- `assistant_message_id` TEXT — provider-derived final assistant
  message id when available. Claude derives it from the last in-stream
  assistant `message.id`; current Codex `turn/completed` does not
  carry one.
- `token_usage_json` TEXT — snapshot of provider usage at turn-end
- `error_message` TEXT — populated when stop_reason indicates error

Indexes:
- `turns_thread_index` on (thread_id, turn_index DESC) for ListRecentTurns
- `idx_turns_thread_completed` on (thread_id, completed_at DESC) where completed_at is not NULL for sidebar read-state

Rules:
- Inserted at turn-start with `completed_at=NULL`; updated at turn-complete.
- NEVER auto-close a NULL row. Crash-interrupted turns stay NULL; the frontend treats them as "interrupted" state.
- `turn_index` is assigned by the caller (triage), not by the store.

See `docs/architecture/turn-lifecycle.md` for the full lifecycle rules.
