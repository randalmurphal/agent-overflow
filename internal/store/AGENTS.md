# internal/store/

SQLite access and schema. The store is a history cache, not an event
store — see `/AGENTS.md` principle 3.

Schema summary: `/docs/architecture/schema.md`. Data-flow context:
`/docs/architecture/data-flow.md`.

## Rules

- **Migrations are forward-only and append-only.** Never edit a
  migration that has shipped; add a new one.
- **Every migration has a test** that asserts the resulting schema
  state. CHECK constraints belong in SQL; the test proves they fire.
- **WAL mode is verified at startup**, not just requested. If it didn't
  take, boot fails.
- **No business logic in SQL.** Joins for views are fine; behavior
  lives in Go.
- **Payload data is BLOB, loaded on demand.** `payload.meta` (JSON) is
  loaded with items; `payload.data` only on explicit expand.
- **Summary is the preview.** `items.summary` is what the frontend
  renders by default — keep it short and always populated.

## Recent schema changes (v13)

- `projects` is a first-class table. Each thread carries a `project_id`
  FK; a project is the user-level grouping (root dir + name + color)
  above individual threads.
- `interaction_mode` was renamed to `mode` with a new canonical
  default of `"chat"` (was `"default"`). The CHECK constraint was
  rewritten in the same migration; older values are normalised in
  place.
- Composer-context columns landed on the threads table:
  `reasoning_effort` (low/medium/high/xhigh/max), `fast_mode` (bool),
  `context_window` (200000 or 1000000). The per-thread row is the
  source of truth; the `SessionOptions` helper in `thread_view.go`
  translates it into the provider Config.

## What Goes In

- Timeline items, payloads, thread metadata, channels/messages,
  discussion templates, design-artifact metadata, attachment metadata,
  projects.

## What Doesn't Go In

- Live per-turn provider state — the provider process owns it.
- Transient UI state — frontend `$state` owns it.
- Logs — `internal/logging` owns those.

If you're tempted to add a new table, first check whether the provider
session already has the answer.

## Testing

- Every new column, index, or constraint: add a test.
- Concurrency: SQLite+WAL gives you single-writer semantics. Don't
  work around it with in-Go locks; structure writes so they don't
  contend.
- Fixtures: use `t.TempDir()`-scoped DBs. Never share a DB file across
  tests.
