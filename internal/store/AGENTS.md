# internal/store/

SQLite access and schema. A history cache, not an event store — see
root `CLAUDE.md` principle 3.

## Layout

- `store.go` — `Store` construct, `Thread` / `Project` / `Item` /
  `Payload` shared shapes, and the two connection pools: a
  single-connection writer (all writes, migrations, snapshot restore,
  checkpoint, VACUUM — what makes `RestoreFrom`'s temporary
  `foreign_keys` toggle behave as global) plus a small `query_only`
  read pool so reads run against WAL snapshots instead of queuing
  behind flush transactions. Read
  accessors route through `reader()`; the pool is absent (writer
  fallback) for `:memory:` and non-WAL databases, and VACUUM quiesces
  it (`quiesceReads`) to keep its exclusive lock unstarvable. A read
  that must see connection-local state (attached snapshot DBs, PRAGMA
  probes) stays on `s.db`; so does the one write-through-QueryRow
  (`InsertChannelMessageAtomic`'s INSERT..RETURNING).
  `Store.Close` and `New` both run a TRUNCATE checkpoint — see "WAL
  hygiene" below.
- `dsn.go` — the connection-scoped PRAGMAs each pool's DSN carries
  (`writerConnPragmas` / `readerConnPragmas`), the `poolDSN` renderer,
  and `verifyConnPragmas`. These PRAGMAs live in the DSN, never in a
  post-`Open` `Exec`: they are per-connection, and database/sql
  replaces a pooled connection whenever it likes, so an `Exec`-applied
  `busy_timeout` / `foreign_keys` / `synchronous` silently reverts to
  the defaults on the replacement (measured: `fk=1/bt=5000/sync=1`
  before a recycle, `fk=0/bt=0/sync=2` after). `verifyConnPragmas` runs
  once at boot because modernc.org/sqlite executes `_pragma` values
  verbatim and SQLite ignores an unknown PRAGMA name without an error —
  a DSN typo would otherwise open cleanly and run the whole app with
  foreign keys off.
- `migrate.go` — forward-only migration chain with the per-version
  upgrade SQL. `configureDatabase` owns only `journal_mode=WAL` (a
  persistent, database-level setting) plus the `dsn.go` verification.
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
  for the provider packages. Two writers here are reached by callers
  that hold no lock and can land after someone else rewrote the row,
  and each carries its own guard rather than erroring — a lost race is
  a normal outcome:
  - `UpdateTitleIfCurrent` (auto-title vs. a manual rename) is a
    compare-and-swap on the title and returns "applied".
  - `UpdateBranchForWorkspace` (an async observed-branch persist vs. a
    worktree switch, which rewrites `workspace_path` and `branch`
    together under `threadLocks`) is keyed on the WORKSPACE, and that
    scope IS the guard: a thread that has since moved is simply not
    matched, so a stale observation cannot follow it. The branch is a
    fact about the checkout, so it lands on every thread sitting there,
    not just the observing one. The UPDATE additionally excludes rows
    that would not change (`branch IS NOT ?`, null-safe so the
    empty-string/NULL spelling behaves) and returns exactly the rows it
    wrote, read back by id inside the same transaction. Returning none
    is the ordinary answer: the caller writes on every UI attach and the
    observed branch usually already matches.
- `projects.go` — projects table (threads carry a `project_id` FK).
- `project_worktree_setup.go` — the `projects.worktree_setup` JSON column
  (migration v46): the project's worktree setup recipe, read and written
  whole. `ProjectWorktreeSetup` decodes STRICTLY and reports a corrupt blob as
  an error rather than an empty recipe — silently cutting a worktree without
  the setup its project declared is the failure mode that refusal exists for.
  `UpdateProjectWorktreeSetup` clears the column for a nil OR
  asks-for-nothing config, so "cleared" has one representation. The column is
  deliberately absent from `projectColumns`: it is needed at worktree creation
  and in Settings, never in a sidebar list that ships to every client.
- `channels.go` / `discussions.go` / `discussion_types.go` —
  multi-agent discussion persistence. `ListDiscussionDefs` drops a row
  whose `definition` blob no longer decodes (logged, named by id) instead
  of failing the whole list — one corrupt blob must not take the feature
  away. The single-row getters still return the decode error: that caller
  asked for exactly the unreadable row.
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
- `thread_worktree_setup_state.go` — the `threads.worktree_setup_state`
  column (migration v47): the enum constants, the validating writer, and the
  startup sweep. See "Recent schema changes (v47)" below.
- `thread_import_state.go` / `items_import.go` — the session-import surface
  (migration v50). The first owns `thread_import_state` CRUD and
  `ListImportedSessionRefs` (the dedup set a scan subtracts from); the
  second owns `ApplyImportBatch`, the one-transaction write of a whole
  imported session. See "Recent schema changes (v50)" below.
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
  cost_source) — the granularity the app (repo-root
  `app_usage_pricing.go`) needs to price `cost_source='none'` rows per
  model at query time from `internal/usagecost`. `cost_usd` in the table
  is wire-reported only and this package never estimates it; `QueryUsage`
  alone always reports `UnpricedRows=0` — that field is populated by the
  app after merging in the rate-table lookup, and now means
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
  `QueryWorkItemCosts` is the many-runs read behind the workflow overview, and
  it returns `[]WorkItemCostGroup` — one row per (work_item_id, model,
  cost_source) — rather than the summed `map[string]float64` it used to.
  A pre-summed dollar figure could only ever be the WIRE half, which is $0 for
  every Codex row ever written, so the overview read a Codex-heavy run as free.
  The group shape is the same one every other pricing consumer takes, so the
  app prices all of them through one fold.
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
  The phase-attempt reads are four narrow projections rather than one, and each
  is named for what it must NOT carry: `ListWorkItemPhases` (everything),
  `…PhaseContexts` (the engine's variable/loop rebuild), `…PhaseTimeline` (the
  detail views, no gate trace or cumulative input), and `…PhaseProvenance` — the
  `ao run status` read, which LEFT JOINs `threads` for the provider/model/effort
  the attempt actually ran with and carries no envelope at all, because an
  agent's context window pays for every byte it returns — but does carry
  `park_cause` (v51), which is the whole reason an engine-side park is
  diagnosable from `run status` instead of from the filesystem.
  `UpdateWorkItemSeeds` is the one writer of `work_items.seeds` after creation,
  and it exists for `agent-overflow run amend`: seeds are re-read from the row
  at every phase entry, so changing the column IS the amendment. It writes the
  whole object the engine composed rather than merging here — which keys change
  is the engine's rule, and a second merge in SQL would be a second answer to it.
  `work_items.go` also owns the call linkage (v38): `ListWorkItemChildren` reads
  a run's callees, `ListWorkItemCallChildren` narrows that to the child one call
  *attempt* created, and `WorkItemListFilter.ParentItemID` is the same edge for
  summary listings. All three pair the parameter with `parent_item_id <> ''` so
  the partial index applies. `GetWorkItemNode` / `ListWorkItemChildNodes` walk
  the SAME edge through a third projection — `WorkItemNode`, three plain columns
  (id, parent, call depth), no join — for the readers that want a tree's SHAPE
  and nothing else. It is a distinct type rather than a sparsely-filled
  `WorkItem` because a row whose Goal and State are silently blank is a trap.
  The summary projection cannot serve them: its phase-progress join makes SQLite
  parse every row's frozen snapshot to find a phase ordinal, and
  `agent-overflow run watch --tree` re-resolves its membership on every wake of a
  globally broadcast loop.
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
- `history_sync.go` / `store_meta.go` — the history invalidation
  contract (see its own section below): the per-thread `history_rev` /
  `history_epoch` stamps, the `bumpHistoryRevTx` helper every non-item
  window-visible write calls, `SyncThreadWindow` (the one-transaction
  "is the client's window still current?" read), and the store's
  `backend_id` / `replica_generation` identity.
- `proposed_plans.go` / `proposed_plan_comments.go` — the per-plan state
  row (version, revision parent, implemented stamp) and the inline review
  notes anchored to it. Neither table is read directly by the timeline:
  `decorateProposedPlanItems` projects both onto `Item.Meta` at window-read
  time, which is why every mutator in the pair advances `history_rev` —
  see the history invalidation contract below.
- `sqlutil.go` — shared SQL helpers. `sqlExecutor` and `sqlQueryer` are
  what let a private read/write helper run on either pool or inside a
  caller's transaction; `SyncThreadWindow`'s same-transaction guarantee
  is built out of the latter. `placeholders` renders an `IN (...)` bind
  list and `uniqueNonEmptyStrings` is its companion: the wire-supplied id
  lists that feed one are trimmed and de-duplicated first, because their
  length doubles as the expected rows-affected count.

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

## Recent schema changes (v44) — the soft-stop request and its park reason

- The v44 rebuild adds `work_items.soft_stop` (`INTEGER NOT NULL DEFAULT 0
  CHECK(soft_stop IN (0,1))`) and widens the `reason` CHECK with `checkpoint`
  (D36). `SetWorkItemSoftStop` is the only writer, and it is called only from
  the workflow engine's command loop — the engine owns the flag's read/clear
  pair, so a second writer would open the window that ordering closes.
- The rebuild text is derived from v43's the same way v43's was derived from
  v39's, so the same two lists had to be re-checked: the indexes added after
  v43 (none) and the copy list, which gains nothing because `soft_stop` is the
  column this migration creates and therefore does not copy. A future
  `work_items` rebuild inherits both checks.
- `work_item_units.RetryWorkItemUnit` takes the unit's new try number and the
  retry note and PERSISTS both alongside the `pending` reset (D36b). It used to
  reset the row and write neither, so a retried unit that was evicted and
  restored came back on its old try with no feedback. The signature refuses a
  try number below 1 rather than accepting a value only the engine's own
  arithmetic makes correct.

## Recent schema changes (v46) — per-project worktree setup

- `projects.worktree_setup` (`TEXT NOT NULL DEFAULT ''`) holds the project's
  worktree setup recipe as JSON — the copy globs and argv commands that used to
  live in the hand-authored `profile.yaml` and were reachable only by the
  workflow engine. Moving it onto the row is what lets chat-thread worktree
  creation run the same recipe. Shape and execution belong to
  `internal/worktreesetup`; this package only round-trips the blob.
- Empty means unconfigured. NOT nullable on purpose: NULL and `''` would be two
  indistinguishable spellings of the same state, and every reader would have to
  agree about both.
- A plain `ADD COLUMN` — no CHECK, no column-list change, so the FK-parent
  `projects` table is not rebuilt.

## Recent schema changes (v47) — chat-thread worktree setup state

- `threads.worktree_setup_state` (`TEXT NOT NULL DEFAULT ''
  CHECK(worktree_setup_state IN ('', 'running', 'failed'))`) is the durable half
  of the per-project setup recipe running over a worktree a CHAT thread had cut
  (`app_worktree_setup.go`; the recipe itself is v46's `projects.worktree_setup`).
  A plain `ADD COLUMN` — SQLite allows a CHECK on one, so the FK-parent `threads`
  table is not rebuilt.
- Three states, not four: `''` covers never-ran, succeeded, cancelled, and
  "the thread has since left that worktree", because all four are the same
  absence to every reader. `failed` is the only one a restart must preserve —
  the worktree exists and is usable, but the recipe did not finish, so the
  sidebar advertises it and Retry stays reachable.
- `thread_worktree_setup_state.go` owns both writers.
  `SetThreadWorktreeSetupState` validates against the enum before SQLite would
  report a raw CHECK failure, and deliberately leaves `updated_at` alone: a
  setup transition is system work, and the sidebar sorts by `updated_at`.
  `SweepRunningThreadWorktreeSetups` is the startup counterpart of
  `FailRunningWorkItemUnits` — a `running` row at boot means the app died with
  the recipe in flight, so the worktree's provisioning state is unknown, which
  is what `failed` means here.
- `updateThreadSetSQL` deliberately omits the column, so the whole-row
  `UpdateThread` every workspace-switch path issues cannot clobber a live run's
  state. Pinned by `TestUpdateThreadPreservesWorktreeSetupState`.

## Recent schema changes (v51) — why the engine parked an attempt

- `work_item_phases.park_cause` (`TEXT NOT NULL DEFAULT ''`) is the ENGINE's
  own diagnosis of a park: the worktree that would not cut, the phase missing
  from the frozen snapshot, the resolved fan-out width the project forbids,
  the budget that ran out. It is a separate column from `output_envelope`
  because the envelope is the AGENT's artifact — a phase that parked before
  any turn ran authored nothing, and engine prose written there is read as
  something a model said by every consumer of an envelope (the history
  binding's `envelopeStatus`, the crash rebuild's terminal check, the wake's
  detail line). Free-form text, so no CHECK; the table is nobody's FK parent,
  so a plain `ADD COLUMN` with no rebuild.
- Empty means "no engine-diagnosed cause", which is the common case: the
  attempt rested on its own envelope, or the reason already names its cause
  (`interrupted`, `paused`, `taken-over`). `CompleteWorkItemPhase` takes it as
  a parameter and writes it unconditionally, so a non-park completion clears
  any earlier value rather than inheriting it, and `ReopenWorkItemPhase`
  clears it too — a reopened attempt is being re-run, and a stale diagnosis on
  a live attempt reads as a park that already happened again.
- It rides `ListWorkItemPhases`, `…PhaseTimeline`, and `…PhaseProvenance`. The
  provenance read is what `agent-overflow run status` renders as `cause=`, and
  the timeline read is what the wake resolves its one bounded cause line from.

## Recent schema changes (v53) — the pending-guidance slot

- `work_items.pending_guidance` (`TEXT NOT NULL DEFAULT ''`) holds what
  `agent-overflow run guide` left for a run and no phase entry has consumed yet:
  a JSON array of `{text, at, by, byRun?}`, oldest first. It is live data a run
  RE-READS rather than anything it froze — the same posture as `seeds` and the
  opposite of `snapshot` — which is what lets an operator steer a run that is
  already working.
- The engine is the only writer, and the author stamp is its: `by` comes from
  the authenticated caller (an interactive session is `human`, a workflow phase
  session is `phase` plus the run it belongs to), never from the request, so an
  agent cannot leave an entry the delivered prompt attributes to a person.
- Delivery CLEARS it, and the two writes are deliberately not one transaction:
  the attempt row carrying the entries is written first and the slot cleared
  after, so the only gap a crash can open is a redelivery rather than a lost
  instruction. See `internal/workflow/engine/guidance.go` for the ordering
  rationale.
- **Deliberately absent from `workItemColumns`**, exactly like
  `wake_signature`: no listing or overlay reads it, and every row those reads
  carry would pay for it. `WorkItemPendingGuidance` and
  `SetWorkItemPendingGuidance` are the only reader and writer. A plain ADD
  COLUMN with no CHECK, so no rebuild; a future `work_items` rebuild must carry
  it, like every other column added since v39.

## Recent schema changes (v54) — the self-resume moment

- `work_items.auto_resume_at` (`INTEGER NOT NULL DEFAULT 0`, Unix milliseconds)
  is when a parked run brings itself back. It is written by exactly one park: a
  provider that refused the turn because the account's usage allowance is spent
  AND said when it returns (`app_workflow_quota.go`), which parks
  `retries-exhausted` with the reset stated in the attempt's `park_cause`. The
  wait is measured in days, so the moment has to outlive the process that
  learned it — an in-memory timer would mean a five-day stall needing a human
  alarm clock after any restart, which is the whole failure this column exists
  to end.
- `0` means "nothing armed", which is every other run. The column is the single
  source of truth and the timer is derived from it: `app_workflow_autoresume.go`
  arms one on the write, re-arms every armed row at boot
  (`ListWorkItemAutoResumes`, ordered soonest-first, a past-due row firing
  shortly after boot rather than instantly), and clears BOTH halves the moment
  the run leaves `needs-human` by any route — the timer's own resume, a manual
  one, a cancel, a discard, a rerun. Opting out clears what opting in stored,
  through the one state-transition hook rather than per verb.
- **Deliberately absent from `workItemColumns`**, like `wake_signature` and
  `pending_guidance`: no listing, overlay, or CLI projection reads it, and every
  row those reads carry would pay for it. `WorkItemAutoResumeAt`,
  `SetWorkItemAutoResumeAt`, and `ListWorkItemAutoResumes` are the only reader
  and writer. A plain ADD COLUMN with no CHECK, so no rebuild; a future
  `work_items` rebuild must carry it, like every other column added since v39.

## Recent schema changes (v52) — the last wake delivered

- `work_items.wake_signature` (`TEXT NOT NULL DEFAULT ''`) is the signature of
  the last wake DELIVERED into a run's bound thread. Wakes are deduplicated by
  what they SAY — run, resting state, typed reason, phase and attempt, detail,
  engine cause, and for a descendant park the same again for the descendant —
  never by a time window, so the column holds the content identity rather than
  a timestamp. `internal/workflow/wake` computes it; the app compares, records,
  and clears.
- It is durable rather than in-memory because a restart is one of the ways the
  same ask gets re-composed: a crash rebuild parks every interrupted run, and a
  supervising agent that already read that message should not read it again on
  every launch.
- Empty means "nothing delivered yet, or somebody has acted on the run since",
  which is the state every wake delivers from. The app clears it whenever any
  member of a run tree returns to `running` — which is what every resolve,
  answer, resume, retry, and rerun does.
- **Deliberately absent from `workItemColumns`**, like `projects.worktree_setup`
  is from `projectColumns`: no listing, overlay, or CLI projection has a use for
  it, and every row those reads carry would pay for it. `WorkItemWakeSignature`
  and `UpdateWorkItemWakeSignature` are the only reader and writer. A plain ADD
  COLUMN with no CHECK — free-form text, and the table is nobody's FK parent —
  so no rebuild; a future `work_items` rebuild must carry it, like every other
  column added since v39.

## Recent schema changes (v50) — imported provider sessions

- `threads.import_source` (`TEXT NOT NULL DEFAULT '' CHECK(import_source IN
  ('', 'claude', 'codex'))`) is write-once provenance: the provider whose
  on-disk session file a thread was imported from. It exists because
  `session_ref` cannot gate the "Check for Provider Updates" affordance —
  every thread that has run a turn has one. Never rendered as a badge.
  `claude-tui` is deliberately not in the enum: it drives claude's binary
  and has no session files of its own. A plain `ADD COLUMN` with a CHECK, so
  the FK-parent `threads` table is not rebuilt.
- `updateThreadSetSQL` omits the column (as it does `worktree_setup_state`),
  so no whole-row `UpdateThread` can rewrite provenance. `CreateThread` is
  the only writer, and `prepareThreadForCreate` validates against the enum
  before SQLite would report a raw CHECK failure. Pinned by
  `TestUpdateThreadPreservesImportSource`.
- `thread_import_state` is the per-thread cursor into the source file, one
  row per imported thread, cascading on thread deletion. `last_source_uuid`
  is written by BOTH providers — it is the provenance stamp of the last
  event consumed, a transcript uuid for Claude and `line:<byte offset>` for
  Codex, the same value that lands in `items.meta.import_source_uuid` — but
  only Claude ANCHORS a refresh on it, because its transcript is a uuid DAG
  where a byte offset says nothing about conversation position.
  `last_source_offset` is Codex's anchor and stays 0 for Claude
  (append-only JSONL, so a tail read is the cheap refresh and a SHRUNK file
  is the diverged-source signal). `leaf_uuid` records which Claude branch a
  thread was cut from, because one session file can produce several
  threads.
- `last_turn_index` + `last_item_index` are ONE cursor, and both default to
  **-1**, not 0. They are a pair because `items.item_index` restarts at 0 in
  every turn (that is what `nextItemIndexTx` allocates), so a lone item index
  names no position in a thread — item 2 of turn 1 and item 2 of turn 9 share
  it. `HasItemsAfterCursor` asks the only exact question: does any row sort
  lexicographically after `(last_turn_index, last_item_index)`? A true answer
  means the thread was resumed inside AO after the import and appending the
  source's tail would interleave duplicate history under indices the live
  session already claimed. The predicate is spelled
  `turn_index > ? OR (turn_index = ? AND item_index > ?)` rather than a tuple
  compare so `idx_items_thread` serves it as a range scan. -1 rather than 0 on
  both halves is what keeps "imported nothing yet" below every real position.
- `ListImportedSessionRefs` unions `threads.session_ref`,
  `threads.pending_fork_session_ref`, and
  `thread_import_state.source_session_id`. The middle one is load-bearing: a
  fork that has never been resumed has a session file on disk and no
  `session_ref` at all, so a `session_ref`-only check would offer it for
  import. Deleting a thread drops its entry, which is what makes the source
  session importable again.
- `ApplyImportBatch` writes turns → completions → payloads → items → usage in
  ONE transaction, and deliberately does not touch `threads.updated_at` or
  thread activity: an import replays history that already happened, and
  floating every imported thread to the top of the sidebar contradicts the
  original timestamps it just wrote. Turns and items are plain INSERTs — a
  re-applied batch must fail loudly rather than overwrite — while turn
  completions are UPDATEs, because a refresh settles a turn an earlier
  import inserted. Failing loudly here is a LAST resort, not the check:
  `PlanUpdate` has already promised the user the refresh would apply by
  the time this runs, so the import writer reads `TurnIDsForThread` up
  front and refuses a batch that would re-open a turn id the thread
  already holds.

## History invalidation contract (v55)

The frontend keeps a durable replica of a thread's window so a cold open
can paint from it before the backend answers. That is only safe if the
store can answer "is what you have still current?" cheaply and can never
answer "yes" wrongly. Design: `docs/specs/thread-replica-sync.md` (§3
counters, §4 event stamps, §5 the sync RPC).

- `threads.history_rev` counts EVERY window-visible mutation of that
  thread's history. `threads.history_epoch` counts only the mutations a
  client cannot apply incrementally — a deletion or a reposition. Every
  epoch bump also bumps rev, so rev alone answers "anything changed?"
  and epoch answers "throw your copy away".
- **Rev 0 is unreachable for any thread that predates the contract.**
  v55's SQL ends with `UPDATE threads SET history_rev = 1;`, because
  `(0, 0)` is also the JSON zero value of the sync request's stamp pair:
  a client that omits the fields asks "is rev 0 still current?", and over
  an untouched 400-item thread that would have matched and returned a
  page-less `fresh` for a replica the client does not have. After the
  lift, `(0, 0)` can only describe a thread with no item writes since
  v55 — an empty window, for which a page-less `fresh` is truthful. The
  column DEFAULT stays 0 for exactly that reason.
- The counters are maintained by three AFTER triggers on `items`
  (`trg_items_rev_insert` / `_update` / `_delete`), not by Go. Structural
  on purpose: item writes happen from triage, the importer, the rollback
  paths, and the migration chain, and a Go-side bump would be one
  forgotten call site away from a client that never re-syncs. Their DDL
  lives in ONE place — `historyRevTriggersSQL` in `history_sync.go` —
  because it has two installers: migration v55 concatenates it, and
  `RestoreFrom` recreates the triggers after dropping them for its row
  copy (see below). The UPDATE trigger's epoch term is a boolean
  addition —
  `+ (OLD.turn_index IS NOT NEW.turn_index OR OLD.item_index IS NOT NEW.item_index OR OLD.thread_id IS NOT NEW.thread_id)`
  — so a repositioning UPDATE bumps epoch and an in-place content UPDATE
  does not. `IS NOT` rather than `<>` because a null-valued comparison
  would add NULL and blank the column. The third disjunct pairs with the
  trigger's two-row scope, `WHERE id IN (OLD.thread_id, NEW.thread_id)`:
  no store code moves a row between threads, but if one ever does it is a
  delete from one ordering and an insert into another, so both threads
  take rev AND epoch.
- Writes that change a window WITHOUT touching an `items` row carry a
  `threadID` parameter and call `bumpHistoryRevTx` inside their own
  transaction. The parameter IS the enforcement: there is no way to reach
  the write without naming the thread it belongs to, so a new caller
  cannot forget. Two classes qualify:
  - the **payload mutators** (`AppendPayloadData`, `ReplacePayloadData`,
    `UpdatePayloadMeta`, `UpdatePayloadSpans`) — payload content and
    preview spans ride the item row on the wire, and `payloads` has no
    thread_id for a trigger to route on;
  - the **window-visible decoration sources**: `proposed_plans` and
    `proposed_plan_comments`, whose rows `decorateProposedPlanItems`
    projects onto `Item.Meta` on EVERY window read, `SyncThreadWindow`
    included. `EnsureProposedPlanState(WithParent)`,
    `MarkProposedPlanImplemented`, `CreateProposedPlanComment`,
    `UpdateProposedPlanComment`, `DeleteOrResolveProposedPlanComment`,
    and `MarkProposedPlanCommentsSent` all bump. Both tables reference
    `threads(id)` directly, so the id they carry is always the PLAN's
    thread — which matters because `MarkProposedPlanImplemented` is
    called cross-thread (`app_send.go` passes the source plan's thread
    while the work starts on another), and only the plan's window
    changes. Idempotent replays that write nothing bump nothing.
    **Any future read-time projection of a non-`items` table into a
    windowed row joins this class**: if a window read can render it, its
    writers bump.
  The item-coupled combos (`UpsertItemWithPayloadAppend`,
  `AppendItemSummaryAndPayloadData`) deliberately do NOT call it — their
  item write already fired a trigger, and a second bump would only make
  the counter less useful.
- There is **no exported bare payload insert or upsert**. A payload is
  only window-visible through the item that references it, and an export
  that wrote `payloads` without naming a thread would be a hole in the
  enforcement above — the `INSERT OR REPLACE` flavour additionally reset
  `preview_spans` / `spans`, which ride the item row. Production writes
  go through the item-coupled writers (`InsertItemWithPayload`,
  `AppendItemWithPayload`, `UpsertItem`) or the threadID-carrying
  mutators. Tests that need a bare row use `seedPayloadRow` in
  `store_test.go`.
- `RestoreFrom` DROPs the three triggers at the start of its transaction
  and recreates them from `historyRevTriggersSQL` after the copy. A
  whole-database replace is not a write the contract needs to catch — the
  counters it must land on are the snapshot's own, copied verbatim — and
  left installed the DELETE leg fires one `UPDATE threads` per cleared
  item row while the INSERT leg is a no-op only for as long as `items`
  keeps sorting before `threads` in `userTables`' `ORDER BY`.
- `SyncThreadWindow` reads the stamps, the store identity, and the
  window in ONE read-pool transaction. Under WAL that transaction pins
  its snapshot at the first statement, which is what makes the returned
  stamps describe EXACTLY the returned rows. Splitting it into two reads
  would let a write land between them and hand a client a newer stamp
  over older rows — a replica that is permanently wrong and never
  corrects itself, because nothing later contradicts it. Any change here
  keeps the single transaction.
- `store_meta` (one row, `CHECK(id = 1)`) holds `backend_id` and
  `replica_generation`. `backend_id` names the database and is stable for
  its lifetime — it is what a client keys its on-disk replica by, so
  re-minting it would orphan every cached window. `replica_generation`
  names the current history LINEAGE: `RestoreFrom` re-mints it inside the
  restore transaction because a restore rewinds every counter, so stamps
  a client holds from the replaced future would compare as "ahead" and
  read as fresh forever. The counters cannot express that; the generation
  is what does. `store_meta` is an ordinary user table, so the restore's
  row copy would otherwise hand this store the SNAPSHOT's backend id
  (harness recordings come from other databases): `RestoreFrom` reads the
  live id before the copy and `remintStoreIdentityTx` takes it as a
  required parameter and writes BOTH columns back.
- Clients may only ever UNDERSTATE what they hold. `provider:turn_completed`
  and `user_message:reverted` carry stamps read from the store at build
  time (`internal/triage/turn_lifecycle.go`, `app_conversation_rollback.go`);
  `provider:item_event` frames deliberately carry none, because an
  item-level stamp would be a promise about rows the frame does not
  contain.

## WAL hygiene

`PASSIVE` is the only checkpoint mode safe on a hot path — it returns
immediately without waiting for readers — but it never shrinks the `-wal`
file. It recycles WAL pages so the file stops GROWING; whatever
high-water mark a burst pushed it to stays on disk. A user's live
database reached a 300MB WAL that way.

`TruncateCheckpoint` is the counterweight, and it runs at exactly three
moments, all of them chosen because exclusivity is cheap there:

- **Boot** (`New`, after migrations, before the read pool opens). The one
  place a WAL stranded by an ungraceful exit — a Windows Job Object kill,
  an OOM, a crash — gets reclaimed, and the one place no reader can
  exist. This is why the Windows launcher's hard teardown does not have
  to be graceful for WAL size to recover.
- **`Store.Close`**, after the read pool closes and before the writer
  does. Order is load-bearing: TRUNCATE cannot reset a WAL any connection
  holds a read mark on, and the checkpoint runs on the writer.
- **The retention sweep's trailing pass** (`app_retention_cleanup.go`),
  after its VACUUM — which appends the entire rebuilt database to the
  WAL. Skipped while shutting down, since `Close` runs it anyway.

Two properties to keep in mind when adding a caller: TRUNCATE waits out
an open read transaction for the full `busy_timeout` and then reports
`CheckpointResult.Busy` having reclaimed nothing (SQLite answers a
blocked checkpoint with a result ROW, never an error — a caller that only
checks `err` cannot tell a reclaimed WAL from an abandoned one), and it
quiesces the read pool internally, exactly like VACUUM.

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
- Do NOT configure a connection-scoped PRAGMA with an `Exec` after
  `sql.Open`. It configures one connection instance, not the pool, and
  the replacement comes up with the SQLite defaults. Add it to the
  pool's DSN list in `dsn.go` — which also gets it verified at boot.

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
