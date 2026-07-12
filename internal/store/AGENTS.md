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
- `usage_ledger.go` — append-only per-turn per-model token/cost
  accounting (`usage_ledger` table, migration v14). DELIBERATELY no
  foreign keys: lifetime aggregates must survive thread/project
  deletion, so thread/project/turn ids are plain attribution columns
  and provider/model are denormalized at write time. `AppendUsage`
  inserts; `QueryUsage` aggregates with time-range/thread/project/
  provider/model filters and day/week/month (timezone-shifted) or
  model/provider/thread/project grouping; `QueryUsageDetail` runs the
  same filters/bucketing but additionally splits by (model,
  cost_source) — the granularity `GetUsageStats` (repo-root
  `app_usage.go`) needs to price `cost_source='none'` rows per model at
  query time from `internal/usagecost`. `cost_usd` in the table is
  wire-reported only and this package never estimates it; `QueryUsage`
  alone always reports `UnpricedRows=0` — that field is populated by
  `GetUsageStats` after merging in the rate-table lookup, and now means
  "rows whose model the rate table doesn't recognize" rather than
  "rows with no wire cost." Migration v23 adds denormalized
  `work_item_id`; `QueryWorkItemUsage` supplies the raw token and
  wire-cost sum used for workflow budget checks, while
  `QueryWorkItemUsageDetail` groups the same rows by model/cost source so the
  app can add query-time `usagecost` estimates for rows without wire cost.
- `work_items.go` / `work_item_phases.go` / `work_item_effects.go` — bare
  workflow run-record CRUD (migration v23). Project, thread, and item ids
  are intentionally denormalized without FKs so run history survives
  deletion. State-machine validation and scheduling belong to
  `internal/workflow`, not this package.
- `automations.go` — automation definition, continuity-note, and
  per-source cursor CRUD. Cursors are dependent scheduler state and
  cascade when an automation is deleted.
- `ui_state.go` — persisted per-client UI view state (`ui_state`
  table, migration v15). `(scope, key) → value` where scope is an
  opaque namespace (`client:<uuid>` now, `user:<id>` reserved) and
  values are opaque strings. The justified carve-out from "transient
  UI state belongs to frontend `$state`": these rows are the
  restart-surviving copy behind the frontend `appStorage` module,
  needed because webview localStorage resets every launch (ephemeral
  transport port = new origin). `GetUIState` returns a whole scope;
  `SetUIState` batch-upserts; `DeleteUIState` is idempotent.
- `migrate_fixups.go` — Go-side data fixups referenced by `Fix`
  migrations in `migrate.go`, built on the shared `rewriteItemMetas`
  scan/rewrite helper (v8 trims persisted tool_result echo, v9 trims
  collab agentsStates messages out of `items.meta`, v21 removes encrypted
  MultiAgentV2 prompt blobs and repairs their summaries).
- `sqlutil.go` — shared SQL helpers.

## Responsibility boundary

- What BELONGS here:
  - Timeline items, payloads, thread metadata, channels / messages,
    discussion templates, attachment metadata, projects, composer
    favorites, last-used model profile seeds, workflow run records, and
    automation definitions/cursors.
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
  `reasoning_effort` (provider-specific; Codex currently accepts
  none/minimal/low/medium/high/xhigh/max/ultra), `fast_mode` (bool),
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

## Recent schema changes (v23-v25) — workflow persistence

- `work_items`, `work_item_phases`, and `work_item_effects` persist workflow
  run history without project/thread/item FKs; `automations` and
  `automation_cursors` persist trigger definitions and watermarks.
- `usage_ledger.work_item_id` attributes phase-thread usage to a run and is
  indexed for budget sums.
- `threads.mode` accepts `workflow` for phase threads. The v24 rebuild
  preserves every existing thread column and index.
- The v25 rebuild adds `workflow-studio` / `workflow-triage`, extends typed
  work-item reasons with `taken-over`, and persists item hand-off ownership in
  `work_items.triage_thread_id`.

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
