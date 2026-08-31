# internal/store/

SQLite access, schema, and the forward-only migration chain. Pure-Go
driver (`modernc.org/sqlite`, no CGO), WAL mode.

This is a history cache, not an event store (root `AGENTS.md` principle 3).
Derived, version-stamped render metadata (span blobs, provider cost
estimates) is cache content too: a stale row is dropped and recomputed,
never migrated.

**One family is exempt and it is the only one**: the identity tables
(`users`, `devices`, `sessions`, `signing_keys`, `recovery_codes`,
`auth_audit`, migration v75; `pairing_links` and `refresh_secrets`,
migration v76) are authoritative. They cannot be rebuilt from a provider
session file, and dropping a stale row means someone is locked out. See
"Recent schema changes (v75)" and "(v76)" below before touching any
sweep, prune, or restore path that walks tables generically.

- `docs/architecture/schema.md`: table-by-table reference and the index
  list. Update it when you add a table, column, or index.
- `docs/architecture/thread-replica-sync.md`: the design behind
  `history_rev` / `history_epoch` / `SyncThreadWindow`.
- `docs/architecture/data-flow.md`: when and why rows are written.
- `internal/store/storetest/` builds one migrated template DB per package
  and byte-copies it per test, so a store-backed test never replays the
  chain.

## Connections and PRAGMAs

Two pools. A single-connection WRITER carries every write, migration,
snapshot restore, checkpoint, and VACUUM, which is what makes
`RestoreFrom`'s temporary `foreign_keys` toggle behave as a global one. A
small `query_only` READ pool serves accessors through `reader()`, so
reads run against WAL snapshots instead of queuing behind flush
transactions.

- The read pool is absent (writer fallback) for `:memory:` and non-WAL
  databases.
- A read that must see connection-local state (attached snapshot DBs,
  PRAGMA probes) stays on `s.db`, as does the one write-through-QueryRow
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
- `migrate.go` — forward-only migration chain: the `migrations` slice with
  the per-version upgrade SQL, the apply/rebuild driver, version
  bookkeeping, and the deprecated derivation helpers
  (`mustReplaceOnce` / `mustReplaceEvery` / `mustCutFrom`).
  `configureDatabase` owns only `journal_mode=WAL` (a
  persistent, database-level setting) plus the `dsn.go` verification.
- `migrate_sql_threads.go` / `migrate_sql_items.go` /
  `migrate_sql_diff_review.go` / `migrate_sql_workitems.go` — the
  table-rebuild DDL the chain references, grouped by the table each rebuild
  recreates (threads carries chat_model_profiles, chat_bar_favorites, and
  new_thread_mcp_defaults, which ride the same migrations). A derivation
  and the shipped text it derives from stay in ONE file, so the chain is
  readable end to end. Declarations only — nothing here applies anything.
  `migrate_freeze_test.go`'s completeness scan globs the whole package
  directory, so the freeze is unaffected by which file a derivation sits in.
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
  for the provider packages. `thread_forks.go` also owns
  `SettleForkedThreadAsInterrupted`, applied to a FORK after its clone:
  stranded running/streaming rows flip to errored and open turn rows
  close with `stop_reason='interrupted'`. It shares
  `settleStrandedItemsTx` and that stop_reason with
  `RecoverCrashedTurns` — a mid-turn fork holds rows no process will
  ever finish, which is the crash sweep's situation exactly — and is a
  no-op on an idle clone, so the fork saga runs it unconditionally. The
  shared clause exempting background `tool_call` rows (invariant 24) is
  load-bearing on BOTH paths: the clone keeps every SETTLED background
  launch — permanently `running` with its completion sibling, the
  designed terminal shape — and the exemption is what stops the fork
  settle from rewriting that finished work as errored. Truly-live
  (siblingless running/streaming) background launches never reach the
  settle — `cloneThreadItemsTx` drops them transitively, sibling
  presence judged inside the clone's cut. `CloneThreadHistoryThroughTurn`
  is the fork pipeline's clone and runs the item half and the turn half
  in ONE transaction (as does `CloneThreadHistoryBeforeItem`): split
  apart, a turn completing between them gives the fork a settled
  `end_turn` row over items snapshotted mid-stream and then flipped to
  interrupted.
  Three writers here are reached by callers
  that hold no lock and can land after someone else rewrote the row,
  and each carries its own guard rather than erroring — a lost race is
  a normal outcome:
  - `UpdateTitleIfCurrent` (auto-title vs. a manual rename) is a
    compare-and-swap on the title and returns "applied".
  - `CompareAndSwapModelProfile` (an import-profile repair vs. a newer user
    selection) guards all four model fields and returns "applied". A lost
    race is success with no write: the user's current choice owns the row.
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
  and priming. Keyed `(thread_id, payload_id, path)`, cascade with the
  matching thread-owned `payloads` row,
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
- `thread_live_todo.go` — the `threads.live_todo` column (migration v65): the
  `ThreadLiveTodo` blob shape and its three accessors. Read and written whole,
  like `project_worktree_setup.go`, and refusing an empty list for the same
  reason it clears the column for an asks-for-nothing recipe — "cleared" gets
  one representation. See "Recent schema changes (v65)" below.
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
  or flash a "Load older" button that loads nothing. The background
  tray is the one read that does NOT share that filter — see
  `thread_aggregates.go` below.
- `thread_aggregates.go` — thread-wide reads backing dedicated frontend
  bindings (plan sidebar, background tray). `ListLiveBackgroundTasks` is
  the tray's item set and lists by BACKGROUNDED ANCESTRY, not by
  top-level-ness (`docs/specs/agent-visibility.md` Q8): every live
  `is_background = 1` launch at ANY depth, every live launch that
  DESCENDS from one and is itself a subagent launch
  (`subagentLaunchFilterFor`), and the recent completion siblings of
  that set. Foreground plain tool calls under a background agent stay
  out — they are the agent's own work, rendered inside its card. Because
  only a launch can be a parent, the second class also supplies every
  intermediate ancestor between a background root and a nested launch,
  so the frontend indents by walking `parentId` WITHIN the result rather
  than asking for rows it was not given. This is the DISPLAY query only:
  the reaper and queue gates in `items_lifecycle.go` and `paging.go`'s
  `topLevelItemsFilter` keep `parent_id = ''` (invariant 24), because
  whether the tray SHOWS a nested background Bash and whether that Bash
  blocks the flush queue are different questions.
- `subagent_items.go` — the two read surfaces that replace in-window
  subagent children: `decorateSubagentAnchors` stamps each windowed
  launch anchor with its descendant count + latest-child summary
  (collapsed-card aggregates), and `ListSubagentDescendants` loads the
  full child subtree on demand when a group card expands. It also owns
  the two shared SQL fragments both this file and `thread_aggregates.go`
  build their recursive reads from:
  - `descendantsCTE(table, rootSet)` — the `rel(root, id)` recursive walk
    down `parent_id` from a root set, `CROSS JOIN`ed as a planner
    directive and repeating `parent_id <> ''` so the partial index
    applies. `descendantsCTEFromRoots(n)` is the `timeline_items`
    bind-list form the descendants read uses; the tray passes plain
    `items` and a subquery.
  - `subagentLaunchFilterFor(alias)` — what makes a `tool_call` row a
    subagent LAUNCH. It is **structural** (a `tool_call` that has at
    least one visible child attributed to it), never a tool-name list,
    which is what keeps it provider-neutral across Claude
    `Agent`/`Task`, a forked Skill, a SendMessage resume, and Codex
    `spawn_agent`. The alias is MANDATORY: unqualified `thread_id` / `id`
    inside the `EXISTS` would bind to the inner `items child` copy and
    make the predicate vacuously true. `Store.IsSubagentLaunch` is the
    single-row exported form, used by triage to tell a launch terminal
    from an ordinary tool completion.
- `usage_ledger.go` — append-only per-turn per-model token/cost
  accounting (`usage_ledger` table, migration v14). DELIBERATELY no
  foreign keys: lifetime aggregates must survive thread/project
  deletion, so thread/project/turn ids are plain attribution columns
  and provider/model are denormalized at write time. `AppendUsage`
  inserts; `QueryUsage` aggregates with time-range/thread/project/
  provider/model filters and day/week/month (timezone-shifted) or
  model/provider/thread/project grouping; `QueryUsageDetail` runs the
  same filters/bucketing but additionally splits by (model,
  cost_source) — the granularity the app
  (`internal/usageledger`) needs to price `cost_source='none'` rows per
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
  app prices all of them through one fold — and rolls each group up the run's
  parent chain (via `ListProjectWorkItemNodes`), so an overview entry is the
  run's TREE total, the same number the detail spend and budget check report.
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
  the partial index applies. `GetWorkItemNode` / `ListWorkItemChildNodes` /
  `ListProjectWorkItemNodes` (the whole project's linkage in one read, for the
  overview cost rollup) walk
  the SAME edge through a third projection — `WorkItemNode`, three plain columns
  (id, parent, call depth), no join — for the readers that want a tree's SHAPE
  and nothing else. It is a distinct type rather than a sparsely-filled
  `WorkItem` because a row whose Goal and State are silently blank is a trap.
  The summary projection cannot serve them: its phase-progress join makes SQLite
  parse every row's frozen snapshot to find a phase ordinal, and
  `agent-overflow run watch --tree` re-resolves its membership on every wake of a
  globally broadcast loop.
- `work_item_tree.go` — run-tree SHAPE, resolved in SQLite. Two recursive CTEs
  over `work_items.parent_item_id`: `workItemTreeCTE` (downward, id-only, the
  membership subquery every whole-tree read in this package shares — the run
  map's rows and the budget check's dollars cannot describe different trees)
  and `workItemTreeDepthCTE` (the same walk carrying each member's distance
  from the root). `WorkItemTreeRoot` is the UPWARD resolution — any run to its
  tree's root — and `scanWorkItemTreeRuns` is the downward walk, root-first and
  parent-before-child, through `WorkItemTreeRun`: linkage, project, state,
  timing, the frozen snapshot, and the budget, with goal/seeds/digest/
  disposition dropped because a tree read decodes every member and none of that
  is drawable. The downward walk STREAMS to a visitor rather than returning a
  slice, and that is a retention decision: every member carries a frozen
  snapshot capped at 4MiB, so a materialised 4096-member tree would retain
  gigabytes for one fetch. Peak retention is one blob, which holds only while
  the visitor projects each snapshot and keeps no reference to it.
  Both directions are BOUNDED and REFUSE rather than truncate — the caller
  supplies the depth cap (`engine.MaxCallDepth`) and the member cap — and every
  refusal is TYPED so the app can say which one happened and that it is
  permanent: `ErrWorkItemTreeTooLarge`, `ErrWorkItemTreeTooDeep`, and
  `ErrWorkItemTreeCyclicLinkage`. The bounds are not paranoia: the schema's
  CHECKs make a parent reference all-or-nothing, not acyclic, so a cycle is
  writable and a read that walks one must terminate.
  The id-only CTE terminates on a cycle structurally (set semantics on a bare
  id); the depth-carrying one needs the cap, and collapses a cycle's several
  depths per id with `MIN` so corrupt linkage never duplicates a run.
  **The parent-before-child promise is CHECKED, not assumed.** MIN(depth) can
  order a cycle's members only arbitrarily — with `a.parent = b` and
  `b.parent = a`, anchoring on `a` emits it before its own parent — so the scan
  tracks which ids it has handed over and refuses
  (`ErrWorkItemTreeCyclicLinkage`) the moment a parent arrives after its child.
  A run that names ITSELF is refused on its own row, since the arrival check
  cannot see a parent that arrives as its own child.
  A parent that never arrives is the ORPHAN case and stays legitimate. The app
  never reaches this refusal — its anchor comes from the upward walk, which
  refuses an in-cycle run first, and `parent_item_id` being one column means no
  acyclic anchor can walk INTO a cycle — but the promise is made to every
  caller of an exported read, not to that one.
  The named run must EXIST for the upward read (an unknown id is
  `sql.ErrNoRows`, not a one-run tree), while a run whose named parent's row is
  GONE resolves to itself with the dangling reference left on the answer, which
  is how the caller tells an orphan from a true root.
- `work_item_run_map.go` — the batched WHOLE-TREE read behind the run map
  (`app_workflow_runmap.go`). `ReadWorkItemTree` is the ONE exported entry and
  the only caller of the pieces: the run scan plus
  `listWorkItemTreeAutoResumes`, `listWorkItemTreePhaseStatuses`,
  `listWorkItemTreeUnitStatuses`, and the two tree ledger reads from
  `usage_ledger.go`. They live together because what they share is the caller's
  shape — a tree root, one round trip for a whole campaign — rather than a
  table; each is the `WHERE item_id IN (SELECT id FROM tree)` form of a read
  whose single-run sibling stays with its CRUD, and each orders by item first
  so a caller groups without sorting.
  **All six run in ONE read-pool transaction**, the `SyncThreadWindow` rule
  applied to a tree: under WAL a read transaction pins its snapshot at the
  first statement, so the runs, their attempts, their units, their armed
  resumes and their dollars are facts about one instant. Six independent reads
  are six snapshots, and a run created between the first and the third
  contributes attempt rows belonging to no run in the answer — a half-drawn
  tree the caller can only discard in silence. Sharing the CTE gives shared
  membership DEFINITION; only the transaction gives snapshot ISOLATION. The
  caps are checked by the run scan, which is what keeps the other five
  statements off a tree the read refuses to answer for. The pieces are
  unexported so a caller cannot reassemble them into six snapshots again.
  Narrowing through the CTE rather than a bind array is what keeps a forty-wave
  campaign's id list off the `SQLITE_MAX_VARIABLE_NUMBER` limit nothing here
  checks. The two status projections carry no envelope, gate
  trace, or feedback: the map draws a campaign's every attempt and unit at once,
  and a payload column would make the answer's size a function of how much the
  models wrote. `intervention` is projected to its `kind` in SQLite for the same
  reason — a human gate's note is unbounded and the map wants the
  discriminator.
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
- `ui_state.go` — persisted per-device UI view state (`ui_state`
  table, migration v15). `(scope, key) → value` where scope is a
  namespace `internal/app` derives from the calling connection
  (`device:<id>` for a paired device, `client:<uuid>` for a screen on
  the local page channel) plus `user:default`, which `internal/settings`
  writes the USER tier into rather than a connection naming it, and
  values are opaque strings. A row this table holds may therefore be a
  frontend view-state value OR a settings value; the difference is
  `internal/settings`' business, and nothing here inspects a key. `DeleteUIStateScope` drops a whole bucket, which is how
  revoking a device drops its state. The justified carve-out from "transient
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
  Codex currently accepts none/minimal/low/medium/high/xhigh/max/ultra
  — the v1 baseline stopped at `xhigh`, migration v19
  `codex_max_ultra_reasoning` rebuilt both tables to widen it),
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
  lifecycle — payload GC cascades here, a payload upsert explicitly drops
  its stale snapshots, and readers always re-verify content against the
  request's patch, so a stale row can never serve.

## Recent schema changes (v22) — persisted highlight spans

- `payloads` gains `preview_spans` and `spans` TEXT columns (default
  `''` = not computed). Both hold version-stamped span blobs whose
  shape is owned by the app layer (`app_highlight.go` / `internal/highlightapp`);
  empty or version-stale blobs are inert — the frontend falls back to
  the highlight RPCs.
- `preview_spans` (small, size-capped) is joined into item list reads
  as `Item.PayloadPreviewSpans`; `spans` (full patch spans) is read
  only by explicit payload loads via `GetPayloadSpans`.
- `UpdatePayloadSpans` writes both columns. `ReplacePayloadData` and
  item-coupled payload upserts reset them to `''`;
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

## Recent schema changes (v56) — distinct exhaustion reasons

- v56 rebuilds `work_items` to widen its typed `reason` CHECK with
  `provider-retries-exhausted` and `loop-limit-exhausted`. The former is a dead
  provider turn a bare resume continues; the latter is a workflow bound that
  needs an earlier phase entry to refill. Existing `retries-exhausted` values
  are preserved and remain accepted because their source cannot be recovered
  reliably from the optional free-form park cause.
- The rebuild derives from v44 and must copy every later column explicitly:
  `soft_stop`, `wake_signature`, `pending_guidance`, and `auto_resume_at`.

## Recent schema changes (v57) — provider usage provenance and attention

- `provider-usage-limited` is the typed, continuable reason for a provider
  refusal normalized as `FailureReasonUsageLimit`. The phase or unit that
  failed records `provider_usage_scope_id`; zero is every other failure, and a
  retry/reopen clears it so stale provenance cannot make a later mixed park
  look usage-limit-only.
- `workflow_provider_usage_scopes` keys provider + account id + credential
  generation. It is failure provenance and correlation, not an availability
  cache: no send/admission path reads it. `workflow_provider_usage_attention`
  keys a scope + watching conversation and tracks action, queued, and delivered
  generations. Claim/promote/release are compare-and-set shaped; returning a
  run tree to `running` advances the generation. Queued claims lost at process
  restart are transferred in place before the new engine starts emitting, then
  re-surfaced after its rows are rebuilt by scope + watching conversation; the
  durable claim is never absent, even across a second crash during recovery.
  The recorded source is preferred but is not authoritative: if it resolved
  before restart, recovery selects another currently affected run, including a
  parked descendant whose root is still running. Delivery uncertainty therefore
  costs a duplicate rather than a permanently silent park.

## Recent schema changes (v58) — thread-owned payload graphs

`items` and provider transcripts have always treated item IDs as local to a
thread, but `payloads` used to key on bare `id`. Claude sibling branches reuse
their shared prefix, and live thinking coordinates repeat in every thread, so
the mismatch caused import constraint failures and live `INSERT OR REPLACE`
overwrites. Migration v58 keys `payloads` by `(thread_id, id)`, makes both item
payload references composite FKs, and carries the same scope into
`payload_chunks` and `edit_file_snapshots`.

Every payload accessor and mutator takes `threadID`; joins include both key
columns. Forks copy referenced payloads, chunks, and snapshots into the target
thread in the same transaction as cloned items, so later writes to either
timeline cannot alter the other. The migration duplicates legacy payload
graphs once per referencing thread. It cannot recover bytes that the former
global live upsert had already overwritten.

## Recent schema changes (v61) — shared immutable import history

Provider forks and copied sessions can repeat large stretches of history.
Imported rows therefore live in content-addressed, complete-turn
chunks (`import_history_chunks` plus item/payload children) and threads attach
them through `thread_import_chunks`. The mutable `items` / `payloads` tables
remain the thread-owned overlay. All logical history reads use
`timeline_items` / `timeline_payloads`; an explicit item override or local
payload shadows only that thread's immutable base.

Mutation is copy-on-write. Targeted item and payload changes localize only the
row graph they touch; structural cuts materialize the active shared base in the
same transaction before existing delete/FK machinery runs. Representation-only
copies suppress item triggers and do not change history stamps; the requested
mutation advances them exactly as it did for a fully local thread. Raw local
shadowing, chunk-order gaps, and overlapping imported identities/positions are
rejected by triggers, so safety does not depend on every caller remembering the
layout.

`ApplyImportBatch` hashes thread-neutral chunk content and reuses an existing
chunk only after verifying its recorded shape. Turns and usage stay
thread-owned. Deleting the final thread reference collects the chunk.

## Recent schema changes (v62) — thread-scoped turn identity

`turns.turn_id` is a global primary key, but Codex turn ids are only provider
thread-local in practice: the real import corpus contains the same wire UUID
in distinct sessions. Every live and imported turn now mints its durable id
through `ScopedTurnID`, while `provider_turn_id` preserves the exact wire value
used for provider fork/revert RPCs. Migration v62 prefixes legacy bare Codex
row ids with their thread id and rewrites matching `usage_ledger.turn_id`
attribution; already-scoped Claude/fork rows and orphaned lifetime usage remain
unchanged. Inferred rollout turns are not provider anchors and therefore store
an empty `provider_turn_id`.

## Recent schema changes (v64) — feedback that is still owed a turn

- `work_item_phases.feedback_delivered_at` (`INTEGER NOT NULL DEFAULT 0`) is when
  this attempt's persisted `input_envelope.feedback` stopped being owed to a
  turn: a prompt rendering the note was dispatched to a live provider session
  (the app's send door acks it back through `AckFeedbackRendered` — a session
  merely starting proves nothing about its opening send), or a later attempt of
  the same phase took the note over. `0` means still owed, and it is the state
  an attempt is born in exactly when it carries a feedback NOTE for a phase
  that renders prompts; a noteless or promptless attempt is born settled, so it
  can never accumulate a debt nothing could discharge.
- It exists because feedback was write-only durable state. An operator answered a
  parked question, the engine persisted the answer as the new attempt's feedback
  and dispatched the continuation, the runner start wedged — and nothing ever
  re-read a parked or superseded attempt's feedback, so the fresh entry the
  operator recovered with carried none of it. The answer was on disk and lost all
  the same.
- **The backfill is the point of the migration's second statement.** A bare `ADD
  COLUMN` leaves every historical row at 0, which reads as "owed", so the next
  attempt of every phase any run ever entered would redeliver ancient feedback.
  Existing rows are stamped `MAX(started_at, ended_at, 1)` — their own time, with
  the trailing `1` making "delivered" structural for a row whose clocks were
  never written.
- `MarkWorkItemPhaseFeedbackDelivered` and `ListUndeliveredWorkItemPhaseFeedback`
  are the only writer and reader; the ordering rule they serve lives in
  `internal/workflow/engine/feedback.go`. The read carries `input_envelope` —
  the heaviest column on the table, which is why it is a projection of its own
  rather than a field on an existing one — and narrows in SQL to rows below a
  caller-supplied attempt with a non-empty envelope, so an attempt cannot
  redeliver its own note to itself and a `parkOnNewAttempt` row with no input
  never crosses the boundary. `ReopenWorkItemPhase` deliberately leaves the stamp
  alone: reopening does not rewrite the attempt's input, so re-owing feedback a
  session already rendered would deliver it twice to the turn that read it.
- A plain `ADD COLUMN` with no CHECK, so no rebuild. It rides
  `workItemPhaseColumns` (one integer) rather than a narrow projection of its
  own, because "is this attempt's instruction still owed" is diagnosable state a
  reader of a whole attempt row wants.

## Recent schema changes (v65) — the thread's todo list

- `threads.live_todo` (`TEXT NOT NULL DEFAULT '' CHECK(live_todo = '' OR
  json_valid(live_todo))`) is the activity rail's todo list as the provider last
  reported it — Claude TodoWrite and the Task\* family, Codex update_plan — as
  `{steps:[{step,status,id?,owner?}],updatedAt}`, where `updatedAt` is the
  producing event's time rather than the write's, because the reader ages the
  list against it.
- It exists because the list used to live only in a triage map, cleared by
  `CleanupThread`. An app restart or a session teardown therefore erased a list
  the user was still working through: the work was not finished, only the
  process holding the note. SQLite is now its single source of truth — triage
  keeps no copy, which is also what its own area guide requires ("do NOT cache
  store data here").
- `SetThreadLiveTodo` REFUSES an empty step array (`ErrEmptyThreadLiveTodo`);
  `ClearThreadLiveTodo` is the one way to express a clear, so `''` is the only
  spelling of "no list" and no reader has to treat `{"steps":[]}` as a second
  one. The clear returns whether it cleared anything, and that return is
  load-bearing: it gates the live `provider:todo_update` push, which is only
  meaningful when a pane has a list to drop (the 2026-06-14 delete-to-empty
  incident). Zero rows affected on a SET is an error naming the thread — a
  write that silently matched nothing is indistinguishable from a stored list.
- `ThreadLiveTodo` decodes STRICTLY and reports a corrupt blob as an error, the
  same refusal as `ProjectWorktreeSetup`: substituting "no todos" for an
  unreadable blob would hide the corruption for the thread's lifetime. The one
  caller (`app_live_state.go`) logs it and hydrates without the todo leg,
  because a corrupt list must not take the queue and pending approvals down
  with it.
- Neither writer touches `updated_at` (a todo tick is the provider narrating
  its own work, not user activity, and the sidebar sorts by it — the
  `SetThreadWorktreeSetupState` rule) nor `history_rev` (the column is not
  window-visible; it rides `GetThreadLiveState`, not `SyncThreadWindow`).
- `SetThreadLiveTodo` also refuses a blob over `maxThreadLiveTodoBytes`
  (`ErrThreadLiveTodoTooLarge`) — the triage producers bound step count
  (`maxTodoSteps` on both the TodoWrite decode and the Task\* projection) and
  every per-field rune count, so the cap is the accessor-owned backstop that
  keeps the row's size out of caller discipline. Refused, never truncated.
- Retention is deliberately lazy: an all-completed list the reader ages out
  (the 5s filter in `app_live_state.go`) stays on the row until the next
  report overwrites or clears it — and the app paths that discard the
  conversation a list came from (rollback, provider switch) clear it through
  `triage.ResetThreadTodo`, so a lingering list always describes the thread's
  live conversation. Reads are cheap by construction: `live_todo` is the last
  column in the record, listings select explicit `threadColumns`, and SQLite
  never reads past the selected prefix into a trailing column's overflow
  pages. Writes are not free the same way — SQLite rewrites the whole record
  on ANY `UPDATE threads`, lingering blob included — which is one more reason
  the producers' step caps, not just the 1 MiB backstop, bound the blob.
- **Deliberately absent from `threadColumns`, `updateThreadSetSQL`, and the
  `Thread` struct**, like `projects.worktree_setup` is from `projectColumns`:
  every sidebar row would otherwise carry a list only one screen reads, and a
  whole-row `UpdateThread` from a rename or a workspace switch could clobber a
  live session's list. Pinned by `TestUpdateThreadPreservesLiveTodo`. A plain
  ADD COLUMN with a CHECK — SQLite permits one on an added column — so the
  FK-parent `threads` table is not rebuilt.

## Recent schema changes (v69) — the pinned lazy-fork cut

- `threads.pending_fork_resume_at` (`TEXT NOT NULL DEFAULT ''`) pins the
  transcript cut for a lazy Claude fork taken off a LIVE source: the
  source session's canonical leaf uuid, captured when Fork was clicked.
  The fork's first session start repairs it against the CLI's resume
  filters (`claude.ResolveForkResumeCursor`) and passes
  `--resume-session-at <cursor> --fork-session`, so the CLI's own fork
  cuts where the timeline was cloned instead of wherever the source has
  grown to by first send (the 2026-08-22 skew incident). Empty on an
  idle-source lazy fork — the source's tail IS the cut there — and on
  every non-fork thread.
- One-shot like `pending_fork_session_ref`, and consumed with it: BOTH
  session-ref writers (`UpdateSessionRef`,
  `UpdateSessionRefAndRemapProviderIDs`) clear the pair in the same
  UPDATE, so a committed fork cannot re-pin a later restart.
  `BuildForkedThread`'s explicit field list means a fork of a fork never
  inherits a stale pin. A plain `ADD COLUMN`, no CHECK, no rebuild.
- BOTH pin columns are deliberately omitted from `updateThreadSetSQL`, as
  `worktree_setup_state` and `live_todo` are. One-shot state plus a
  whole-row write from a `Thread` struct a caller read some time ago is
  the clobber shape: a rename or a model change could resurrect a pin the
  first session start had already consumed, or blank one a concurrent
  fork had just set. `SetThreadForkResume` is the narrow writer the fork
  saga uses instead, writing `session_ref` and the pin pair together
  (they describe one resume state) and leaving `updated_at` alone.
  Pinned by `TestUpdateThreadPreservesPendingForkPin`.

## Recent schema changes (v54) — the explicitly scheduled resume moment

- `work_items.auto_resume_at` (`INTEGER NOT NULL DEFAULT 0`, Unix milliseconds)
  is when a parked run explicitly scheduled with `run resume --at` brings
  itself back. Provider usage-limit handling does not write it (D75): a typed
  refusal parks immediately and waits for an ordinary explicit action. The
  requested schedule is durable because it may outlive the process that armed
  it; an in-memory-only timer would silently lose the operator's command.
- `0` means "nothing armed", which is every other run. The column is the single
  source of truth and the timer is derived from it: `internal/workflowapp/autoresume.go`
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

## Recent schema changes (v76) — pairing links and rotating refresh

Two tables plus two columns (`migration_v76_pairing.go`, accessors in
`pairing.go` and `identity.go`): `pairing_links`, `refresh_secrets`,
`devices.channel`, `sessions.activated_at`. Spec:
`docs/specs/remote-access.md` §4. Same authoritative-not-cache rule as
v75 — every bullet there applies here unchanged.

- **Single-use is one statement, twice more.** `RedeemPairingLink` and
  `ConsumeRefreshSecret` are each a single `UPDATE … WHERE <unspent
  predicate> RETURNING <columns>`. The predicate IS the rule: SQLite
  picks the winner and every loser reads `sql.ErrNoRows`, so no caller
  can open a check-then-write window by forgetting a guard. Both take a
  HASH; neither table can hold a token or a secret.
- **`refresh_secrets.session_id` IS the family key.** A rotation keeps
  the session row and replaces the secret, so no `family_id` column
  exists to drift out of step with it. Rotating the SESSION id instead
  would strand every open socket the live-connection registry keys under
  the old one.
- **A spent refresh secret stays readable inside its window.** It is the
  reuse detector's evidence: `ConsumeRefreshSecret` refuses it, and
  `GetRefreshSecretByHash` still returns it so the caller can tell "this
  was issued and already spent" from "this was never issued". The prune
  bound is the EXPIRY, never consumption.
- **`SpendRefreshSecretsForSession` is the family revocation half.** One
  statement marks every unspent secret of a session consumed, stamped
  with why. Splitting it per row would leave a partially-revoked family
  behind a failed loop.
- **`sessions.activated_at` is the confirmation gate, and it is a
  PREDICATE, not a flag someone must remember to read.** `Session.Live`
  requires it, so an unconfirmed session refuses on every presentation
  path — verification, renewal, ticket, upgrade — without any of them
  knowing pairing exists. `ActivateSession` also requires
  `expires_at > ?`: the pending window IS the deadline on the
  confirmation, so accepting one after it lapsed would make the deadline
  decorative. The v76 migration backfills existing rows to `created_at`,
  because a session that predates the column was already live.
- **`devices.channel` is a partial unique index, not a kv row.**
  `idx_devices_channel` covers `channel <> ''`, so paired devices (empty
  channel) are unconstrained while the local page channel resolves to
  exactly one row across every boot. `EnsureChannelDevice` races against
  that index the way `EnsureOwnerUser` races against
  `idx_users_single_owner`: a loser re-reads the winner.
- **`devices.key_thumbprint` uniqueness makes re-pairing an ADOPTION.**
  A device that pairs twice matches its existing row rather than
  accumulating one per pairing; `internal/identity` refuses the match
  when that row is revoked, belongs to another user, or would change the
  row's `proof_kind`.
- **`devices.proof_kind` (v77) is what makes `key_thumbprint` mean one
  thing.** The column held two different values under one name until this
  migration: the RFC 7638 thumbprint of a real key, and an opaque
  identifier minted by a page that has no WebCrypto. `key` accepts only a
  signed proof over the request; `bearer` compares the string. The
  DEFAULT is `bearer`, which is what makes the migration a no-op for every
  device paired before it — and why the ADD COLUMN needs no rebuild.
- **Both new tables cascade from their owner** (`users` for links,
  `sessions` for secrets) because neither is a record of what happened —
  `auth_audit` is, and it deliberately does not cascade. Deleting a
  session must not leave secrets that name a session id nothing resolves.

## Recent schema changes (v75) — the identity core

Six tables in one migration (`migration_v75_identity.go`, accessors in
`identity.go`): `users`, `devices`, `sessions`, `signing_keys`,
`recovery_codes`, `auth_audit`. Spec: `docs/specs/remote-access.md` §3.

- **These rows are NOT cache.** Every other table in this database can be
  rebuilt from provider session files; identity cannot. The
  "stale means drop and recompute" rule that governs span blobs and cost
  estimates does not apply to any of them — a dropped session row is a
  person locked out, and the only recovery is re-pairing from a host-local
  surface or a recovery code (spec §12). Nothing here may be pruned by a
  cache sweep.
- **Plural from the start, with one bootstrap exception.** `users` holds N
  rows, and every device, session, and audit row names its user
  explicitly. `EnsureOwnerUser` is the ONLY accessor that resolves a user
  by role, exists so the first pairing has something to bind to, and says
  so in its doc comment. Adding a second such read re-introduces the
  single-owner assumption the schema exists to avoid; take an explicit
  user id instead. `idx_users_single_owner` makes a second owner
  unrepresentable, and `EnsureOwnerUser` races against it deliberately —
  a loser re-reads the winner's row rather than reporting a conflict.
- **Revoking a device is ONE write.** `RevokeDevice` flips
  `devices.revoked_at` and every live `sessions.revoked_at` in a single
  transaction and returns the session ids that moved. Splitting it would
  let a device be flagged revoked while its credentials still worked, one
  forgotten call site away. The returned ids are what the caller
  force-closes; a session already revoked is deliberately NOT returned,
  because whoever revoked it already closed it.
- **Recovery-code consumption is one statement.** `UPDATE … WHERE
  code_hash = ? AND consumed_at IS NULL RETURNING user_id`. The predicate
  IS the single-use rule and SQLite picks the winner, so no caller-side
  check-then-write window exists. A replayed code matches nothing and
  answers `sql.ErrNoRows`, the same answer a code that never existed
  gets. The store never sees a code, only its hash.
- **`auth_audit` is append-only by trigger** (`trg_auth_audit_immutable`
  aborts UPDATE) and bounded by insert-order pruning inside
  `AppendAuthAudit`, every `authAuditPruneEvery`-th append. There is no
  DELETE trigger precisely so that bound can be enforced; immutability is
  about rewriting a record, not about keeping every row forever. The
  prune keys on the AUTOINCREMENT id, never on `at`, so a backwards clock
  jump cannot decide which history survives.
- **`auth_audit`'s attribution columns are not foreign keys.** The record
  that a device was revoked is worth most after that device row is gone; a
  cascade would delete exactly the history someone is reading.
  `TestAuthAuditOutlivesWhatItDescribes` pins it.
- **Value sets live in two places on purpose.** The `class`,
  `binding_class`, `role`, and `outcome` CHECKs restate the sets that
  `internal/identity` declares as Go types, because this package stays
  identity-free the same way it stays provider-free.
  `TestDeclaredValueSetsMatchTheSchemaChecks` (in `internal/identity`,
  which can import this package while the reverse would cycle) drives
  every declared value through a real store and pins both directions.
- **A scope blob that does not decode is an error**, never an empty
  grant. Reading a corrupt set as "no scopes" would turn a storage fault
  into a permissions answer the caller cannot distinguish from a real
  one. `[]` is the only spelling of "granted nothing".

## Recent schema changes (v73, v74) — where a thread came from

- `threads.created_by_device` (v73, `TEXT NOT NULL DEFAULT ''`) names the
  screen that started a thread: the durable per-browser-profile device id the
  connection carries (`transport.ClientIdentity`, parsed off the WebSocket
  upgrade query). Empty means the backend created the thread itself — a
  workflow phase, the harness RPC, a session import — which is a normal
  answer, not a missing one.
- It is **creation** attribution, not last-touched attribution, and that is a
  decision rather than a shortcut. A column holds one answer: re-stamping it
  on every mutation would overwrite the provenance it exists to keep and still
  not produce a history, so a real "who changed what" record would be a log
  table, not a column. Recording the DEVICE rather than the connection is the
  matching choice — a connection id dies with the page load, which would make
  the attribution expire on reload.
- `threads.created_branch`, `threads.created_remote_url`,
  `threads.created_head_commit` (v74, all `TEXT NOT NULL DEFAULT ''`) are the
  workspace's git coordinates at the moment the thread was created, surfaced
  on `store.Thread` as the `Origin` sub-struct. They exist so a thread can be
  reproduced elsewhere later: by the time anyone asks, the branch has moved,
  the commit may have been rebased away, and the workspace may hold something
  else. `threads.branch` is a different question — the live checkout, which
  moves with the working tree.
- Empty is always "not known", never "none" and never an error. A workspace
  outside a repository, a detached HEAD, a repo with no remote, and every row
  created before v74 all read the same, and a consumer that needs the values
  has to say so itself.
- All four are write-once by the same mechanism as `import_source`: absent
  from `updateThreadSetSQL`, classified in
  `threadColumnsNotWrittenByUpdateThread`, and written only by `CreateThread`.
  Plain `ADD COLUMN`s with no CHECK, so the FK-parent `threads` table is not
  rebuilt.
- The values are observed at the one moment they are true, by
  `(*App).stampThreadCreation` / `(*App).observeThreadOrigin` and by the
  `threadapp.Workspace` port's `ObserveOrigin`. Forgetting is structural, not
  remembered: `TestEveryNewThreadRecordsWhereItCameFrom` (`internal/app`) scans
  every thread-creating package for new-thread literals and fails any that
  neither sets `Origin` nor carries a written reason.

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
  is the diverged-source signal). `leaf_uuid` records the active Claude
  branch selected for the provider session; one transcript may retain many
  inactive alternatives, but current import materializes only the branch
  Claude resumes. `source_parent_session_id` (v63) is the provider's durable
  explicit-fork parent id. `ReconcileImportedForkLineage` resolves it to
  `threads.forked_from_thread_id` independently of import order; unresolved
  parents remain durable, deletion clears the FK, and re-import relinks it.
  Only explicit Claude `forkedFrom.sessionId` / Codex `forked_from_id` values
  land here — Codex `parent_thread_id` is spawned-child provenance.
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
  compare so `idx_items_thread_turn_item_unique` serves it as a range scan. -1
  rather than 0 on both halves is what keeps "imported nothing yet" below every
  real position.
- `ListImportedSessionRefs` unions `threads.session_ref`,
  `threads.pending_fork_session_ref`, and
  `thread_import_state.source_session_id`. The middle one is load-bearing: a
  fork that has never been resumed has a session file on disk and no
  `session_ref` at all, so a `session_ref`-only check would offer it for
  import. Deleting a thread drops its entry, which is what makes the source
  session importable again. Keys are `(normalized provider, session id)`, so
  the same id under Claude and Codex remains two independent conversations;
  `claude-tui` normalizes to `claude` because it drives Claude's session files.
  Migration v63 prevents NEW duplicate imported source claims with triggers
  while preserving legacy duplicates so an upgrade cannot make the store
  unopenable.
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
  Payloads and items are emitted in bounded multi-row INSERTs. While those
  item rows load, the transaction sets the private `history_bulk_load` flag so
  the item triggers skip their per-row thread UPDATE; the writer adds the
  number of inserted items to the revision and clears the flag before commit.
  The flag is never visible outside the writer transaction, and rollback
  restores it together with the rows.
- `DeleteThread` keeps its bounded 500-item transactions so a large deletion
  cannot hold the only SQLite writer past `busy_timeout`. Each chunk uses the
  same transaction-private flag to replace 500 delete-trigger updates with one
  exact aggregate `history_rev` / `history_epoch` advance before commit. A
  reader between chunks therefore still sees a changed stamp, and a failed
  chunk cannot strand the flag or hide later ordinary writes.

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
  because it has two latest-schema installers: migration v59 concatenates it,
  and `RestoreFrom` recreates the triggers after dropping them for its row copy
  (see below). Migrations v55 and v58 use the pre-flag legacy body while the
  chain is replayed. Each latest trigger updates only when
  `history_bulk_load = 0`;
  that condition is what lets `ApplyImportBatch` preserve one-revision-per-item
  semantics with two thread updates instead of millions, without weakening
  ordinary item writes. The UPDATE trigger's epoch term is a boolean
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
    preview spans ride the item row on the wire; their explicit thread-scoped
    transaction also advances that thread's history revision;
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
  enforcement above — the private upsert additionally resets the derived
  chunks, snapshots, and span blobs that ride the item row. Production writes
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

`PASSIVE` is the only checkpoint mode safe on a hot path, and it never
shrinks the `-wal` file. It recycles pages so the file stops GROWING.
Whatever high-water mark a burst pushed it to stays on disk. A user's
live database reached a 300MB WAL that way.

`TruncateCheckpoint` is the counterweight, and it runs at exactly three
moments, all chosen because exclusivity is cheap there:

- **Boot** (`New`, after migrations, before the read pool opens). The one
  place no reader can exist, and the one place a WAL stranded by an
  ungraceful exit gets reclaimed. This is why the Windows launcher's hard
  teardown does not have to be graceful for WAL size to recover.
- **`Store.Close`**, after the read pool closes and before the writer
  does. Order is load-bearing: TRUNCATE cannot reset a WAL any connection
  holds a read mark on, and the checkpoint runs on the writer.
- **The retention sweep's trailing pass** (`app_retention_cleanup.go`),
  after its VACUUM, which appends the whole rebuilt database to the WAL.

Two properties to keep in mind when adding a caller. TRUNCATE waits out
an open read transaction for the full `busy_timeout` and then reports
`CheckpointResult.Busy` having reclaimed nothing: SQLite answers a blocked
checkpoint with a result ROW, never an error, so a caller that only checks
`err` cannot tell a reclaimed WAL from an abandoned one. And it quiesces
the read pool internally (`quiesceReads`), exactly like VACUUM.

## Migrations

The chain in `migrate.go` counts up from the squashed v1 baseline
(`schema_v1.go`). Rebuild DDL lives in `migrate_sql_*.go`, grouped by the
table each rebuild recreates. The threads family is `threads` +
`chat_model_profiles` + `chat_bar_favorites`, which ride the same
rebuilds. `configureDatabase` owns only `journal_mode=WAL`.

- **Never edit a migration that has shipped.** Append a new one. A new
  column, index, or CHECK means a new migration plus a test asserting both
  the schema and the constraint behavior
  (`docs/architecture/how-to.md#add-a-migration`). A new table also needs a
  companion `<name>.go` with typed accessors and a `schema.md` entry.
- **New rebuild-style migrations restate their SQL in full**, as a const of
  their own. The derivation helpers (`mustReplaceOnce` /
  `mustReplaceEvery` / `mustCutFrom`) are deprecated for new work: a
  derivation makes an earlier migration's text live source code, so editing
  that source rewrites every migration downstream of it, including ones
  that already ran on user databases. Two stores at the same version number
  then hold different schemas and nothing can detect it, because the
  version row says the migration ran.
- **`migrate_freeze_test.go` enforces that.** It pins the sha256 of the
  final SQL of every migration whose text is derived, and
  `TestEveryDerivedMigrationIsFrozen` parses the package to find a
  derivation the map is missing, printing the line to paste. When it fails
  the answer is almost always "add a new migration". Update a frozen hash
  only when you are certain that migration never shipped.
- **A rebuild carries two lists forward.** It runs with foreign keys OFF
  (`applyRebuildMigration`) so `DROP TABLE` does not cascade-delete the
  children that reference it, and it has to recreate every index the table
  gained since the text it copies from AND copy every column added since
  then. Both have been silently dropped before.

## Triggers, and what Go must not duplicate

These carry invariants Go code cannot be trusted to remember. Read them
before changing a write path.

- **History stamps** (`historyRevTriggersSQL` in `history_sync.go`). Three
  AFTER triggers on `items` maintain `threads.history_rev` (every
  window-visible mutation) and `history_epoch` (only the ones a client
  cannot apply incrementally: a deletion or a reposition). Structural
  because item writes come from triage, the importer, the rollback paths,
  and the migration chain, so a Go-side bump would be one forgotten call
  site away from a client that never re-syncs. The DDL is one const with
  two installers: migration v59, and `RestoreFrom`, which drops the
  triggers for its row copy and recreates them after.
- **A write that changes a window without touching an `items` row calls
  `bumpHistoryRevTx`** inside its own transaction. The `threadID` parameter
  every such mutator carries IS the enforcement. Two classes qualify today:
  the payload mutators, and the window-visible decoration sources
  (`proposed_plans` / `proposed_plan_comments`, which
  `decorateProposedPlanItems` projects onto `Item.Meta` on every window
  read). **Any future read-time projection of a non-`items` table into a
  windowed row joins that second class.** The item-coupled combos
  deliberately do NOT bump: their item write already fired a trigger.
- **`SyncThreadWindow` reads stamps, store identity, and the window in ONE
  read-pool transaction.** Under WAL that pins the snapshot at the first
  statement, which is what makes the returned stamps describe exactly the
  returned rows. Split it and a write landing between the two reads hands a
  client a newer stamp over older rows: a replica that is permanently wrong
  and never corrects itself, because nothing later contradicts it. Any
  change here keeps the single transaction.
- **Payload GC fires on DELETE of an `items` row only**
  (`trg_items_gc_payload` / `trg_items_gc_input_payload`), dropping the
  payload when no sibling item in that thread still references it as either
  `payload_id` or `input_payload_id`. An UPDATE that re-points a payload
  reference collects nothing, so a rewrite path has to delete or reuse
  deliberately. `payload_chunks` and `edit_file_snapshots` cascade from the
  payload row.
- **Imported history is guarded by triggers, not by caller discipline**
  (v61, `migration_v61_shared_import_history.go`). A local `items` INSERT
  that would shadow an imported row is ABORTED unless a
  `thread_import_item_overrides` row exists. Chunk-order gaps and
  overlapping imported identities or timeline positions are rejected the
  same way. Dropping a thread's last chunk reference collects the chunk.
  Logical reads go through `timeline_items` / `timeline_payloads`, never
  raw `items` / `payloads`, or they miss the immutable base.
- **`threads.history_bulk_load` is the only sanctioned way to silence the
  stamp triggers**, and only inside one transaction that writes the exact
  aggregate advance itself before commit. `ApplyImportBatch` uses it to
  replace millions of per-row thread UPDATEs. `DeleteThread` uses it per
  500-item chunk, which is also what keeps a large deletion from holding
  the only writer past `busy_timeout`. A rollback restores the flag with
  the rows.

## Row writes report what they changed

Every persisted thread row and project row is broadcast (`thread:updated`,
`project:updated`) so a second attached client converges without a refresh, and
a write that changed nothing is not broadcast. That makes "did this row
actually move" a value the write itself must return, so those mutators go
through `applyThreadRowWrite` / `applyProjectRowWrite` (`rowwrite.go`) and
return `(row, changed bool, error)`. Both entry points wrap one generic
`applyRowWrite`, and each names its own table AND its own read-back projection,
because those two always have to agree.

- **Rows-affected cannot answer it.** SQLite counts a row as affected when
  the SET restates the value the row already held, so `requireRowsAffected`
  proves the row exists, never that it moved. The `Change` predicate on the
  write (`archived IS NOT 1`, `reasoning_effort IS NOT ?`) is what makes the
  UPDATE match nothing when there is nothing to do.
- **Use `IS NOT`, not `<>`, on a nullable column.** `NULL <> 0` is `NULL`,
  which is not true, so a `<>` change predicate silently skips every row
  whose column is NULL and reports the write as a no-op.
- **A no-op and a missing row are different answers.** With a `Change`
  predicate the UPDATE returns no row for both, so the miss path re-probes
  for the row under the eligibility half of the WHERE (`id`, plus any
  `Match` clause such as `pinned_at IS NOT NULL`). Present means
  `(zero, false, nil)`; absent means `sql.ErrNoRows`, which preserves the
  refusals callers already depend on.
- **The row comes back from the write's own transaction.** `RETURNING id`
  then `listThreadsByIDTx` inside the same tx: `threadColumns` carries
  correlated subqueries, which `RETURNING` cannot evaluate, and a second
  round trip could read a row a concurrent write had already moved. The
  projection is paid only when something changed.
- **A write that is not one row states its rules by hand.**
  `UpdateProjectSortPositions` writes N rows in one transaction, which
  `applyRowWrite` cannot do without giving up that transaction, so it carries
  its own `RETURNING id` + in-transaction read-back. It deliberately has NO
  change predicate: the `updated_at` bump is the point of the write (a reorder
  counts as project activity), so every matched row really did move.
- **`CreateProject` returns the row it inserted, not its argument.** The slug
  is generated inside the insert, so a caller holding its own copy has an empty
  one — and would broadcast it.

## Reads that are easy to get wrong

- **Partial indexes need textual qualification.** SQLite uses one only when
  the query's predicates textually imply the index's WHERE clause. A
  correlated `c.completion_of = items.id` does NOT imply
  `completion_of <> ''`, so every completion-sibling EXISTS / NOT EXISTS
  probe repeats `AND c.completion_of <> ''`. It is semantically redundant
  and it is what keeps the probe off a full scan of the thread's items
  (seconds per call on large threads). Same rule for the `parent_id <> ''`
  term in the subagent descendant CTEs and the `parent_item_id <> ''` /
  `source_ref <> ''` terms on the work-item reads. Keep the term when
  writing a new probe.
- **Windows, budgets, and has-more probes count top-level rows only**
  (`parent_id = ''`), so one subagent-heavy turn cannot eat a window budget
  or flash a "Load older" button that loads nothing.
- **`ListLiveBackgroundTasks` is the one read that does not share that
  filter**, because it lists by backgrounded ANCESTRY
  ([invariant 24](../../docs/architecture/invariants.md#24-backgrounded-work-outlives-its-launching-turn),
  `docs/specs/agent-visibility.md` Q8). The reaper and the flush-queue
  gates keep `parent_id = ''`, because whether a nested background Bash is
  LISTED and whether it blocks the queue are different questions. Its
  callers are the tray and `App.ListRunningBackgroundWork`, the
  cross-thread inventory; both go through `App.ListLiveBackgroundTasks`,
  which unions this query with two sources that are not in any table, so a
  new cross-thread reader must not call this one directly.
- **`subagentLaunchFilterFor(alias)` is structural, never a tool-name
  list**: a `tool_call` with at least one visible child attributed to it,
  which is what keeps it provider-neutral. The alias argument is MANDATORY,
  because an unqualified `thread_id` / `id` inside the `EXISTS` binds to the
  inner `items child` copy and makes the predicate vacuously true.
- **Payload identity is `(thread_id, id)`, not `id`** (v58). Claude
  transcript branches deliberately reuse provider item ids across threads,
  and live thinking coordinates repeat in every thread. A global key turned
  that valid reuse into an import constraint failure or a cross-thread
  overwrite. Every payload accessor takes `threadID` and every join
  includes both key columns. There is no exported bare payload insert or
  upsert. Tests that need a raw row use `seedPayloadRow`.
- **Turn ids are thread-scoped** (`ScopedTurnID`, v62). Codex wire turn ids
  repeat across sessions. `provider_turn_id` keeps the verbatim wire value
  for provider fork and revert RPCs.
- **`provider_thread_cost` (v66, provider-thread identity in v68) is not a
  `usage_ledger` row.** It holds the provider's own CUMULATIVE per-thread
  estimate, one row rewritten in place, while the ledger's contract is
  per-turn deltas any slice of which can be summed. Reads require its
  `session_ref` to match the thread's, so a row naming a provider thread
  this thread no longer points at is ignored rather than served.

## Responsibility boundary

Belongs here: timeline items and payloads, thread and project metadata,
channels and messages, discussion templates, attachment metadata, composer
favorites and model-profile seeds, workflow run records, automation
definitions and cursors, identity rows (accounts, devices, sessions,
signing keys, recovery codes, the credential audit log), migrations,
indices, CHECK constraints, and query helpers returning typed rows.

Does not belong here: live per-turn provider state (the provider process
owns it), transient UI state (frontend `$state`), logs
(`internal/logging`), and business logic. Credential minting, claims
signing and verification, and what a scope MEANS belong to
`internal/identity`; this package persists the rows and enforces what
SQLite can state about them. If a tempting SELECT grows a
WHEN/CASE, the behavior belongs in Go. Workflow state-machine validation
and scheduling belong to `internal/workflow`. This package holds bare
run-record CRUD. `ui_state` is the one justified carve-out from the
transient-UI rule: webview localStorage resets every launch, because the
ephemeral transport port makes a new origin. Before adding a table, check
whether the provider session already has the answer.

## Anti-patterns

- Do NOT use `SELECT *`. Index every `WHERE` column.
- Do NOT load `payload.data` eagerly alongside list reads. `meta` is cheap.
  `data` loads on explicit expand.
- Do NOT leave `items.summary` empty. The frontend renders it by default.
- Do NOT add a column to `threadColumns` / `projectColumns` /
  `workItemColumns` that only one screen reads. Several are deliberately
  absent from those projections and from `updateThreadSetSQL`, so a
  whole-row `UpdateThread` from a rename cannot clobber one-shot or live
  state. Give it narrow accessors and a `TestUpdateThreadPreserves*`-style
  pin instead.

## Testing

- Every new column, index, or constraint gets a test.
- Two value sets here are hand-written copies of `internal/provider`'s,
  because this package stays provider-free: `normalizeRuntimeMode` plus the
  `runtime_mode` CHECK, and `legalEffortForProvider` / `legalEfforts` plus
  the provider/effort coupling CHECK. `TestRuntimeModeCheckMatchesProvider`
  and `TestReasoningEffortSetsMatchProvider` (`migrate_test.go`, test-only
  import of `internal/provider`) keep them honest in both directions. A tier
  added to the provider package with no migration would otherwise surface as
  a raw CHECK violation on a value the picker had already offered.
- Fixtures use `t.TempDir()`-scoped DBs. Never share a DB file across tests.
- WAL mode is verified at startup, not just requested. If it did not take,
  the app warns and proceeds (rollback journaling keeps the store correct).
  Keep the verification and its log line alive
  ([invariant 19](../../docs/architecture/invariants.md#19-wal-mode-is-verified-at-startup)).
