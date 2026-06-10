# internal/store/

SQLite access and schema. A history cache, not an event store — see
root `CLAUDE.md` principle 3.

## Layout

- `store.go` — `Store` construct, `Thread` / `Project` / `Item` /
  `Payload` shared shapes.
- `migrate.go` — forward-only migration chain with the per-version
  upgrade SQL.
- `items.go` / `items_read.go` / `items_write.go` /
  `items_lifecycle.go` / `payloads.go` — timeline item + heavy-payload
  tables. `items.go` carries the shared core (constants, scanners,
  `applyItemDefaults`, the private tx insert helpers); reads,
  writes/upserts/deletes, and the live-state surface (Has/Count/Mark/
  Force/Flip) live in the matching `items_*.go` siblings. All four
  are in package `store` so the single-table invariants stay
  co-located — split by responsibility, not visibility.
- `threads.go` / `thread_view.go` / `thread_forks.go` — threads table
  plus the `ThreadView` translation layer that hydrates `SessionOptions`
  for the provider packages.
- `projects.go` — projects table (v13 introduces `project_id` FK).
- `channels.go` / `discussions.go` / `discussion_types.go` —
  multi-agent discussion persistence.
- `attachments.go` — attachment metadata (bytes on disk are the
  `internal/attachment` package's problem).
- `checkpoints.go` — message-checkpoint row/ref bookkeeping plus
  `thread_tracked_files`; the ref-level mechanics live in
  `internal/checkpoint`.
- `drafts.go` — composer drafts per thread.
- `chat_bar.go` — composer favorites and last-used model profile seeds.
- `search.go` — case-insensitive substring search across thread titles
  and item summaries via `LOWER + LIKE`. A future migration can swap
  to an FTS5 virtual table without changing the return shape.
- `paging.go` / `turns.go` — windowed history loads, item-budget
  pagers, and has-more probes. Every window, budget, and probe counts
  visible **top-level** rows only (`parent_id = ''`); subagent children
  are excluded so one subagent-heavy turn can't eat the window budget
  or flash a "Load older" button that loads nothing.
- `subagent_items.go` — the two read surfaces that replace in-window
  subagent children: `decorateSubagentAnchors` stamps each windowed
  launch anchor with its descendant count + latest-child summary
  (collapsed-card aggregates), and `ListSubagentDescendants` loads the
  full child subtree on demand when a group card expands.
- `migrate_fixups.go` — Go-side data fixups referenced by `Fix`
  migrations in `migrate.go`, built on the shared `rewriteItemMetas`
  scan/rewrite helper (v8 trims persisted tool_result echo, v9 trims
  collab agentsStates messages out of `items.meta`).
- `sqlutil.go` — shared SQL helpers.

## Responsibility boundary

- What BELONGS here:
  - Timeline items, payloads, thread metadata, channels / messages,
    discussion templates, attachment metadata, projects, composer
    favorites, last-used model profile seeds.
  - Migrations, indices, CHECK constraints.
  - Query helpers that return typed rows.
- What does NOT belong here:
  - Live per-turn provider state — the provider process owns it.
  - Transient UI state — frontend `$state` owns it.
  - Logs — `internal/logging` owns those.
  - Business logic. If a tempting SELECT grows a WHEN/CASE, the
    behavior belongs in Go.

If you're tempted to add a new table, first check whether the provider
session already has the answer.

## Recent schema changes (v13)

- `projects` is a first-class table. Each thread carries a `project_id`
  FK; a project is the user-level grouping (root dir + name + color)
  above individual threads.
- `interaction_mode` was renamed to `mode` with a new canonical default
  of `"chat"` (was `"default"`). The CHECK constraint was rewritten in
  the same migration; older values are normalised in place.
- Composer-context columns landed on the threads table:
  `reasoning_effort` (low/medium/high/xhigh/max), `fast_mode` (bool),
  `context_window`. The per-thread row is the source of truth;
  `SessionOptions` in `thread_view.go` translates it for the provider.

## Recent schema changes (v34) — context settings

- `context_window` now accepts any positive provider/model-supported token
  count instead of a fixed 200k/1m check.
- `threads` and `chat_model_profiles` carry
  `auto_compact_standard_percent` and `auto_compact_extended_percent`
  (0 = provider default/inherited setting, otherwise 1..90).

## Recent schema changes (v25) — raw chat content

- `items.highlighted_content` and `channel_messages.highlighted_content`
  were removed. Store raw `summary`, channel `content`, and payload `data`
  only.
- `AppendItemSummary(threadID, id, delta, updatedAt)` remains the raw
  append helper, but triage calls it from the stream persistence buffer
  rather than for every provider token. No render cache is written.
- Payload bindings return raw data only. Rendering is a frontend projection
  based on item/payload kind.

## Extension points

- To add a new column / index / CHECK: write a new migration — never
  edit a shipped one — and add a test that asserts both the schema and
  the constraint behavior. See
  `docs/architecture/how-to.md#add-a-migration`.
- To add a new table: confirm the provider session doesn't already own
  the data; if it doesn't, add the table + migration + a companion
  `<name>.go` with typed accessors. Update `docs/architecture/schema.md`.
- To add a payload kind: extend `payloads.go` + the triage emitter;
  keep `data` as BLOB and `meta` as JSON.

## Anti-patterns

- Do NOT use `SELECT *`. Index every `WHERE` column. No business logic
  in SQL — just persist + query.
- Do NOT edit a migration that has shipped. Append a new one.
- Do NOT work around SQLite+WAL single-writer semantics with in-Go
  locks; structure writes so they don't contend.
- Do NOT load `payload.data` eagerly alongside list reads. `meta` is
  cheap, `data` loads on explicit expand.
- Do NOT leave `items.summary` empty. The frontend renders it by
  default.

## Testing

- Every new column, index, or constraint: add a test.
- Fixtures: use `t.TempDir()`-scoped DBs. Never share a DB file across
  tests.
- WAL mode is verified at startup, not just requested. If it didn't
  take, the app warns and proceeds (rollback journaling keeps the store
  correct); keep the verification + log line alive. See invariant 19.

## References

- `docs/architecture/schema.md` — authoritative schema summary.
- `docs/architecture/data-flow.md` — when/why rows are written.
- Root `CLAUDE.md` principle 3 ("SQLite is a history cache").
