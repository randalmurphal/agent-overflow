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
- `projects.go` — projects table (threads carry a `project_id` FK).
- `channels.go` / `discussions.go` / `discussion_types.go` —
  multi-agent discussion persistence.
- `attachments.go` — attachment metadata (bytes on disk are the
  `internal/attachment` package's problem).
- `message_anchors.go` — per-real-user-message provider correlation
  rows (`turn_index` + Claude wire uuids) backing fork-from-message
  and revert-on-interrupt. Pure SQLite; no git side.
- `edit_diff_items.go` — read surfaces behind the review pane's Edits
  scope: `ListEditDiffItems` (metadata-only rows for items whose
  payload carries a unified diff — `tool_result` and legacy `diff`
  kinds), `ListTurnEditDiffPatches` (one turn's payload-id + diff-blob
  pairs in item order — the id lets the app attach that payload's
  persisted span seeds), and `ListTurnUserSummaries` (selector group
  labels).
- `edit_file_snapshots.go` — per-edit new-side file snapshots
  (migration v24) behind the Edits scope's snapshot-first expansion
  and priming. Keyed `(payload_id, path)`, cascade with `payloads`,
  gzip encode/decode owned by the accessors: `PutEditFileSnapshot`
  (payload-existence guarded in-statement — a deletion race surfaces
  as wrapped `sql.ErrNoRows`, not an FK violation),
  `GetEditFileSnapshot`, and `GetLatestTurnEditFileSnapshot` (the
  turn's last edit of a path in item order, matching the whole-turn
  concatenation). Misses return `found=false` — an expected state
  (pre-feature history), not an error.
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
  "rows with no wire cost." Migration v26 adds denormalized
  `work_item_id`; `QueryWorkItemUsage` supplies the raw token and
  wire-cost sum used for workflow budget checks, while
  `QueryWorkItemUsageDetail` groups the same rows by model/cost source so the
  app can add query-time `usagecost` estimates for rows without wire cost.
  `QueryWorkItemTreeUsage` / `QueryWorkItemTreeUsageDetail` are the same two
  reads over a whole run tree — a recursive walk of `work_items.parent_item_id`
  from the supplied id — which is the total a workflow budget is enforced
  against. The recursion is anchored on the id, not on a `work_items` lookup, so
  an id whose run record is gone still prices its own rows rather than silently
  reporting zero.
- `work_items.go` / `work_item_phases.go` / `work_item_units.go` /
  `work_item_effects.go` — bare workflow run-record CRUD (migration v26;
  units v35; call linkage v38). Project, thread, and item ids are
  intentionally denormalized without FKs so run history survives deletion.
  Because nothing cascades, `DeleteProjectWorkflowRecords` is what drops a
  deleted project's runs, phases, units, effects, and automations in one
  transaction — `App.DeleteProject` calls it on every deletion (D25), after the
  app layer has stopped the runs and removed the worktrees they used. The app
  layer deletes no branch there: cleaning up after a project is not a licence
  to rewrite the user's repository, and D23's per-run discard remains the only
  flow in the app that deletes one. State-machine validation and
  scheduling belong to `internal/workflow`, not this package.
  `work_item_units.go` carries one row per fan-out unit (and join) of a phase
  attempt, written `pending` at expansion and updated in place through
  start/attach/complete; `RetryWorkItemUnit` returns a settled unit to
  `pending` and `FailRunningWorkItemUnits` is the crash-sweep counterpart of
  the engine's teardown. `ReopenWorkItemPhase` (in `work_item_phases.go`) puts
  a settled attempt back to `running` for that recovery, since repairing one
  unit continues the attempt its siblings already produced results for.
  `work_items.go` also owns the call linkage (v38): `ListWorkItemChildren` reads
  a run's callees, `ListWorkItemCallChildren` narrows that to the child one call
  *attempt* created, and `WorkItemListFilter.ParentItemID` is the same edge for
  summary listings. All three pair the parameter with `parent_item_id <> ''` so
  the partial index applies.
- `automations.go` — automation definition, continuity-note, and
  per-source cursor CRUD. Cursors are dependent scheduler state and
  cascade when an automation is deleted. Migration v40 adds the fire
  record — `last_fired_at`, `last_run_item_id`, `skip_count`,
  `last_skip_at`, `last_skip_reason` — written only by
  `RecordAutomationFire` / `RecordAutomationSkip`, which deliberately
  leave `updated_at` alone: a fire is not a definition change, and the
  scheduler keys its armed occurrence on the definition's version.
  `CreateAutomation` inserts definition columns only, so the record
  starts zeroed. `ListEnabledAutomations` is the scheduler's read;
  `ActiveAutomationRun` is the skip-if-running probe — the newest
  `work_items` row with `source='automation'` and this source ref in a
  non-terminal state (`running` or `needs-human`), backed by
  `idx_work_items_automation_source_ref`.
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

## Schema notes (v1 baseline)

The migration chain was squashed into the v1 `initial_schema` baseline
(`schema_v1.go`); the chain in `migrate.go` counts up from there. Facts
older docs attributed to individual migrations that now live in the
baseline:

- `projects` is a first-class table. Each thread carries a `project_id`
  FK; a project is the user-level grouping (root dir + name + color)
  above individual threads.
- Threads carry `mode` (canonical default `"chat"`) plus the
  composer-context columns: `reasoning_effort` (provider-specific;
  Codex currently accepts none/minimal/low/medium/high/xhigh/max/ultra),
  `fast_mode` (bool), and `context_window` (any positive
  provider/model-supported token count). The per-thread row is the
  source of truth; `SessionOptions` in `thread_view.go` translates it
  for the provider. `threads` and `chat_model_profiles` also carry
  `auto_compact_standard_percent` and `auto_compact_extended_percent`
  (0 = provider default/inherited setting, otherwise 1..90).
- Raw content is canonical: there are no `highlighted_content` render
  caches. `AppendItemSummary(threadID, id, delta, updatedAt)` is the
  raw append helper, called from triage's stream persistence buffer
  rather than for every provider token. Payload bindings return raw
  data; rendering is a frontend projection based on item/payload kind.

## Recent schema changes (v24) — edit file snapshots

- `edit_file_snapshots` (see `edit_file_snapshots.go` above): the
  gzip-compressed new-side file content of each edit diff payload,
  captured at the app layer's diff persist tap when the workspace file
  still provably matched the patch. A pure cache riding the payload
  lifecycle — payload GC cascades here, an upsert that replaces a
  payload row (INSERT OR REPLACE deletes + re-inserts) drops its
  snapshots with it, and readers always re-verify content against the
  request's patch, so a stale row can never serve.

## Recent schema changes (v22) — persisted highlight spans

- `payloads` gains `preview_spans` and `spans` TEXT columns (default
  `''` = not computed). Both hold version-stamped span blobs whose
  shape is owned by the app layer (`app_highlight_diff_seed.go`);
  empty or version-stale blobs are inert — the frontend falls back to
  the highlight RPCs.
- `preview_spans` (small, size-capped) is joined into item list reads
  as `Item.PayloadPreviewSpans`; `spans` (full patch spans) is read
  only by explicit payload loads via `GetPayloadSpans`.
- `UpdatePayloadSpans` writes both columns. `ReplacePayloadData` and
  item upserts (INSERT OR REPLACE) reset them to `''`;
  `AppendPayloadData` deliberately retains them — blobs are
  content-addressed per file, so still-valid segments stay valid.

## Recent schema changes (v26-v29) — workflow persistence

- `work_items`, `work_item_phases`, and `work_item_effects` persist workflow
  run history without project/thread/item FKs; `automations` and
  `automation_cursors` persist trigger definitions and watermarks.
- `usage_ledger.work_item_id` attributes phase-thread usage to a run and is
  indexed for budget sums.
- `threads.mode` accepts `workflow` for phase threads. The v27 rebuild
  preserves every existing thread column and index.
- The v28 rebuild adds `workflow-studio` / `workflow-triage`, extends typed
  work-item reasons with `taken-over`, and persists item hand-off ownership in
  `work_items.triage_thread_id`.
- Migration v29 adds JSON-checked `work_items.disposition` receipts and
  `work_items.digest` human-facing run summaries.

## Recent schema changes (v30-v34) — direct start, read-only sessions

- Migration v30 adds the partial `idx_usage_ledger_project_work_item`; v31 adds
  the UNIQUE partial `idx_work_items_agent_source_ref`, which is what makes a
  re-entered phase's repeated `ao run start` resolve to its prior effect instead
  of racing a second identical start.
- The v33 rebuild removes the queue (workflows rev 2): `work_items.sort_position`
  is dropped, `state` no longer admits `'queued'` (any surviving row is migrated
  to `needs-human` / `interrupted` — it stopped without producing a result, which
  is exactly what that reason means), and the three `sort_position`-ordered
  indexes become `created_at`-ordered. `projects.workflow_queue_paused` /
  `workflow_concurrency` (v32) are left physically present but dead: SQLite
  refuses `DROP COLUMN` on a CHECK-bearing column and rebuilding an FK-parent
  table to delete two unread integers is not worth the blast radius.
  `projectColumns` omits them, so nothing reads or writes them.
- The v34 rebuild widens the `threads.runtime_mode` CHECK (and
  `chat_model_profiles`' in lockstep, because a thread's mode is written back
  into the profile row) with `read-only` — the mode a phase declaring
  `access: read-only` runs its provider session in.

## Recent schema changes (v35-v36) — fan-out units

- `work_item_units` (v35) persists one row per fan-out unit and join of a phase
  attempt, keyed `(item_id, phase_id, attempt, unit_id)`. Rows are born
  `pending` when the attempt expands, so an attempt's width and its units'
  sub-worktrees survive a crash; `unit_attempt` counts per-unit retries in
  place rather than adding rows.
- The v36 rebuild widens the `work_items.reason` CHECK with `unit-failed`, the
  typed park reason a fan-out attempt takes when a unit does not complete. It
  recreates every `work_items` index, like the earlier work-item rebuilds.
- Migration v37 adds `idx_work_item_units_thread` on non-empty
  `work_item_units(thread_id)`, backing `GetWorkItemUnitByThread` — the inverse
  of `AttachWorkItemUnitRun` that every thread-first entry point (a human
  steering one taken-over unit) resolves through. It is the unit-side mirror of
  `idx_work_item_phases_thread`.

## Recent schema changes (v38) — call linkage

- The v38 rebuild adds `parent_item_id`, `parent_phase_id`, `parent_attempt`,
  and `call_depth` to `work_items`, widens `source` with `'call'` and `reason`
  with `'child-failed'`, and adds the partial `idx_work_items_parent`. Three
  CHECKs make the linkage all-or-nothing: half a parent reference would make the
  tree unreadable in exactly the direction recovery walks it. Like the earlier
  work-item rebuilds it recreates every index and preserves every column.

## Recent schema changes (v39) — origin-thread binding

- The v39 rebuild adds `work_items.origin_thread_id` — the conversation thread a
  root run reports back into (D17) — plus the partial
  `idx_work_items_origin_thread` that the inverse lookup (every run bound to one
  thread) walks when a deleted thread's bindings are cleared.
- A fourth CHECK, `parent_item_id = '' OR origin_thread_id = ''`, makes "child
  runs never bind" **structural**: the two columns cannot both be set, so a
  called run cannot acquire a thread of its own however it is written. A
  descendant's park still reaches a human — it surfaces as the *root's* wake —
  and that is the app's composition step, not a second binding.
- The same rebuild widens `reason` with `'paused'`. It resumes identically to
  `'interrupted'`; the distinction exists so the morning-after view says whether
  a run stopped on purpose or because the app died.

## Recent schema changes (v40) — automation fire records

- `automations` gains five plain `ADD COLUMN`s (no rebuild): `last_fired_at`,
  `last_run_item_id`, `skip_count`, `last_skip_at`, `last_skip_reason`. They are
  the scheduler's receipt that a trigger occurrence was acted on — a run started,
  or a fire was refused with a reason. A refusal that wrote nothing would be
  indistinguishable from a schedule that never ticked, which is the failure mode
  the columns exist to remove.
- `idx_work_items_automation_source_ref` on
  `work_items(source_ref, state) WHERE source = 'automation' AND source_ref <> ''`
  backs `ActiveAutomationRun`, the skip-if-running probe. Like the other partial
  work-item indexes, the query repeats `source_ref <> ''` so the planner can
  prove the predicate.

## Recent schema changes (v41-v42) — running tool-call partial indexes

- `idx_items_running_bg_tool_calls` (v41) serves the startup orphan sweep
  (`ListRecoverableClaudeBackgroundLaunches`), the one items query with no
  thread scope — measured 12s of cold full-table I/O on a multi-GB history DB
  without it. `idx_items_live_background` can't serve it because that index
  additionally requires `parent_id = ''` and subagent-scoped launches carry a
  parent.
- `idx_items_running_fg_tool_calls` (v42) serves
  `HasRunningTopLevelForegroundToolCall`, probed at every flush-queue boundary.
- **Partial-index qualification rule** (applies to every probe in
  `items_lifecycle.go` / `thread_aggregates.go`): SQLite only uses a partial
  index when the query's predicates textually imply the index's WHERE clause.
  A correlated `c.completion_of = items.id` does NOT imply
  `completion_of <> ''`, so every completion-sibling EXISTS/NOT-EXISTS probe
  repeats `AND c.completion_of <> ''` — semantically redundant, but it is what
  lets `idx_items_completion_of` serve the probe instead of scanning the
  thread's whole items slice (seconds per call on large threads). Keep the
  term when writing new probes.

## Recent schema changes (v43) — unit call linkage

- The v43 rebuild adds `work_items.parent_unit_id`: the fan-out unit whose call
  created a run. v38's linkage identifies an invocation by (item, phase,
  attempt), which is unique for a `shape: call` phase — it makes exactly one
  call — but not for a fan-out phase, where every call-bound unit of one attempt
  would otherwise be indistinguishable from its siblings. Empty means "called by
  the phase", which is what keeps the two cases apart without a second column.
  `ListWorkItemCallChildren` now filters `parent_unit_id = ''` and
  `ListWorkItemUnitCallChildren` is the per-unit read.
- A fifth CHECK, `parent_unit_id = '' OR parent_item_id <> ''`, keeps the
  linkage all-or-nothing in the direction the other four already do: a unit id
  with no parent item would name a unit of no run.
- The rebuild text is derived from v39's, so it restates the indexes added
  *after* v39 — v40's `idx_work_items_automation_source_ref` — and extends
  v39's copy list with `origin_thread_id`, the column v39 itself created and
  therefore did not copy. Both are the failure mode this derivation style
  invites: a rebuild that silently drops what shipped between the text it was
  derived from and now. Any future work_items rebuild has to re-check the same
  two lists.

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
