# SQLite Schema

Source of truth for the shape lives in `internal/store/schema_v1.go` (the
squashed v1 baseline) plus the forward-only chain in
`internal/store/migrate.go`. This page is the human-readable summary — if
it disagrees with the migrations, the migrations win.

## Tables

| Table | Purpose |
|---|---|
| `projects` | User-defined grouping of threads rooted at a directory. `path` (UNIQUE), `name`, `color`, `sort_position`, `archived`. Each thread belongs to exactly one project. |
| `threads` | One row per conversation. Provider, session_ref, workspace/project paths, model, `mode` (chat/plan/design/discussion), `reasoning_effort`, `fast_mode`, `context_window`, per-tier auto-compact percentages (`auto_compact_standard_percent`, `auto_compact_extended_percent`), `runtime_mode`, archived flag, fork lineage (`parent_thread_id`, `pending_fork_session_ref`, `forked_from_thread_id`), discussion membership (`discussion_id`), `last_token_usage`. |
| `items` | Timeline items per thread. `turn_index`, `item_index`, `kind`, `role`, `status`, `summary` (always-loaded preview), `payload_id`, `parent_id` (subagent / nested-tool correlation), `is_background`, `completion_of` (back-reference from tool_completion to its launch), `tool_name`, `decision`, `meta`. |
| `payloads` | Heavy content. `kind`, `meta` (JSON, loaded with items), `data` (base BLOB, on-demand), plus persisted highlight-span blobs (migration v22): `preview_spans` (small, size-capped; joined into item list reads) and `spans` (full patch spans; read only by explicit payload loads). Span blobs are version-stamped and content-addressed — empty or stale means the frontend recomputes via the highlight RPCs. |
| `payload_chunks` | Append-only payload data for live streaming payloads. Rows are keyed by `(payload_id, chunk_index)` and carry `start_offset` so chunk reads can jump to the requested byte range. `ON DELETE CASCADE` keeps lifecycle owned by `payloads`. |
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
| `chat_model_profiles` | Last-used composer settings per provider/model: reasoning effort, fast mode, context window, per-tier auto-compact percentages, runtime mode, and `updated_at` for seeding new chats. |
| `new_thread_mcp_defaults` | Provider/workspace-scoped MCP disabled-server snapshots for future chat/plan thread creation. Existing threads keep their own `threads.disabled_mcp_servers` snapshot. |
| `pending_background_task_terminals` | Per-task stash of Claude `task_updated` terminals whose chat-side `tool_completion` sibling has not been written yet. PK `(thread_id, task_id)`; carries `tool_use_id`, `status`, `exit_code`, `output_file`, `end_time`, `source` (`task_updated`), `created_at`. The tray query `ListLiveBackgroundTasks` joins against this table to hide launches whose host process exited but whose agent observation has not arrived. Drained when `task_notification` / TaskOutput observation lands. The startup sweep for recoverable Claude launches writes the `tool_completion` sibling directly (with `source="session_died"` recorded on the sibling's meta) and never stages a stash row. |
| `usage_ledger` | Append-only per-turn per-model token/cost accounting (migration v14). One row per (settled turn, model): `created_at`, attribution columns `thread_id` / `project_id` / `turn_id` / `provider` / `model` (denormalized, DELIBERATELY no FKs so lifetime totals survive thread/project deletion), token columns (`input_tokens` = non-cached input for both providers, `output_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`, `reasoning_output_tokens`), `cost_usd` (wire-reported only), `cost_source` (`wire`\|`none`). Values are per-turn deltas computed by the provider parsers — summing any slice of rows is safe. Written by `triage/usage_ledger.go`; aggregated by `store.QueryUsage` behind the `GetUsageStats` binding. |
| `ui_state` | Persisted per-client UI view state (migration v15). PK `(scope, key)`; `scope` is an opaque namespace — `client:<uuid>` today, `user:<id>` reserved for when identities exist — and `value` is an opaque string (frontend JSON-encodes structured values). Exists because webview localStorage is not durable (the transport's ephemeral port changes the origin every launch). Backs the frontend `appStorage` module via the `GetUIState`/`SetUIState`/`DeleteUIState` bindings; hydrated once at boot. |

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
