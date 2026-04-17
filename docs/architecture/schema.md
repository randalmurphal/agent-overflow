# SQLite Schema

Source of truth for the shape lives in `internal/store/migrations/`. This
page is the human-readable summary — if it disagrees with the migrations,
the migrations win.

## Tables

| Table | Purpose |
|---|---|
| `threads` | One row per conversation. Provider, session_ref, workspace/project paths, model, interaction mode, archived flag, fork lineage (`parent_thread_id`, `pending_fork_session_ref`, `forked_from_thread_id`), discussion membership (`discussion_id`). |
| `items` | Timeline items per thread. `turn_index`, `item_index`, `kind`, `role`, `summary` (always-loaded preview), `payload_id`, `parent_tool_use_id` (subagent correlation). |
| `payloads` | Heavy content. `kind`, `meta` (JSON, loaded with items), `data` (BLOB, on-demand). |
| `channels` | Deliberation channels for multi-agent discussions. Belongs to a thread. |
| `channel_messages` | Ordered messages within a channel. `sequence`, `from_type`/`from_id`/`from_role`, `content`. |
| `discussion_definitions` | Reusable discussion templates. Scoped global or per-project. `UNIQUE(name, scope, project_id)`. |
| `design_artifacts` | Design-mode HTML artifacts. `html_path` points at the on-disk file. |
| `attachments` | Message attachments. `mime_type`, `size`, `relative_path` on disk. |

## Always-Loaded vs On-Demand

- `threads` — list, always loaded for sidebar.
- `items` — loaded per visible thread. `summary` is the rendered preview;
  full body lives in the linked payload.
- `payloads.meta` — loaded alongside items (JSON preview/stats).
- `payloads.data` — BLOB, fetched only when the user expands.

## Key Indexes

- `idx_threads_updated` — sidebar sort.
- `idx_items_thread` — load thread timeline.
- `idx_items_parent_tool_use` — group subagent items under a parent Task.
- `idx_threads_forked_from_thread` — lineage walks.
- `idx_channels_thread`, `idx_design_artifacts_thread` — per-thread feature lookups.

## Migration Policy

- Migrations are numbered, forward-only, append-only. Never edit a migration
  that has shipped; add a new one.
- SQLite check constraints (see `CHECK(interaction_mode IN ...)`,
  `CHECK(provider IN ...)`) are the recommended way to enforce enums.
- Test every migration: each migration must have a corresponding test under
  `internal/store/` that proves the expected schema state.
- WAL mode is verified on startup (not just requested). If `journal_mode=WAL`
  didn't take, boot fails loudly.

## What Goes in SQLite vs What Doesn't

- **In**: timeline items, payloads, thread metadata, channels/messages,
  discussion templates, design artifact metadata, attachment metadata.
- **Not in**: live per-turn provider state (the provider owns it),
  transient UI state (frontend $state), logs (observability package has
  its own NDJSON logger).

If you find yourself reaching for a new table, first ask whether the
provider process already owns the answer.
