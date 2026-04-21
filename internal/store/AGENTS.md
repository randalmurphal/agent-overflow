# internal/store/

SQLite access and schema. A history cache, not an event store — see
root `CLAUDE.md` principle 3.

## Layout

- `store.go` — `Store` construct, `Thread` / `Project` / `Item` /
  `Payload` shared shapes.
- `migrate.go` — forward-only migration chain with the per-version
  upgrade SQL.
- `items.go` / `payloads.go` — timeline item + heavy-payload tables.
  `items.go` is large; it owns every read/write against `items` so the
  single-table invariants stay co-located.
- `threads.go` / `thread_view.go` / `thread_forks.go` — threads table
  plus the `ThreadView` translation layer that hydrates `SessionOptions`
  for the provider packages.
- `projects.go` — projects table (v13 introduces `project_id` FK).
- `channels.go` / `discussions.go` / `discussion_types.go` —
  multi-agent discussion persistence.
- `designs.go` / `design_types.go` — design-mode artifact metadata.
- `attachments.go` — attachment metadata (bytes on disk are the
  `internal/attachment` package's problem).
- `checkpoints.go` — turn-checkpoint row/ref bookkeeping; the ref-level
  mechanics live in `internal/checkpoint`.
- `drafts.go` — composer drafts per thread.
- `search.go` — FTS across items/threads.
- `sqlutil.go` — shared SQL helpers.

## Responsibility boundary

- What BELONGS here:
  - Timeline items, payloads, thread metadata, channels / messages,
    discussion templates, design-artifact metadata, attachment
    metadata, projects.
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
  `context_window` (200000 or 1000000). The per-thread row is the
  source of truth; `SessionOptions` in `thread_view.go` translates it
  for the provider.

## Recent schema changes (v19) — server-rendered HTML

- `items.highlighted_content` and `channel_messages.highlighted_content`
  (both `TEXT NOT NULL DEFAULT ''`) store the display HTML the
  frontend paints via `{@html}`. The renderer lives in
  `internal/highlight/`; the store never imports it. Callers pass a
  pre-rendered string on insert/upsert or use the two-method streaming
  hot path described below.
- **Streaming hot-path writer** is split into two single-purpose
  methods so the render call runs outside the SQLite writer lock:
  `AppendItemSummary(id, delta, updatedAt)` (summary + thread bump in
  one TX) followed by the caller's own render then
  `UpdateItemHighlight(id, html)` (one `UPDATE`). A reader can briefly
  observe a newer summary with the prior render's HTML; that is
  benign — `AssistantMessage.svelte` falls back to the previously
  rendered HTML (not empty) until the next render catches up.
- Empty `highlighted_content` is a legitimate state — the frontend
  treats it as "render summary/content as plain text". Do NOT encode
  "no render wanted" as a marker string.

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
