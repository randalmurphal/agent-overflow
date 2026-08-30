# internal/store/

SQLite access, schema, and the forward-only migration chain. Pure-Go
driver (`modernc.org/sqlite`, no CGO), WAL mode.

This is a history cache, not an event store (root `AGENTS.md` principle 3).
Derived, version-stamped render metadata (span blobs, provider cost
estimates) is cache content too: a stale row is dropped and recomputed,
never migrated.

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
- **Connection-scoped PRAGMAs live in the DSN (`dsn.go`), never in a
  post-`Open` `Exec`.** database/sql replaces a pooled connection whenever
  it likes and the replacement comes up with SQLite's defaults (measured:
  `fk=1/bt=5000/sync=1` before a recycle, `fk=0/bt=0/sync=2` after). Add
  the setting to `writerConnPragmas` / `readerConnPragmas`, which also
  gets it verified at boot.
- `verifyConnPragmas` reads each pragma back at boot because
  modernc.org/sqlite passes `_pragma` values through verbatim and SQLite
  ignores an unknown PRAGMA name without an error. A DSN typo would
  otherwise open cleanly and run the whole app with foreign keys off.
- Do NOT work around WAL single-writer semantics with in-Go locks.
  Structure writes so they do not contend.

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
  `docs/specs/agent-visibility.md` Q8). It is the DISPLAY query only: the
  reaper and the flush-queue gates keep `parent_id = ''`, because whether
  the tray SHOWS a nested background Bash and whether that Bash blocks the
  queue are different questions.
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
definitions and cursors, migrations, indices, CHECK constraints, and query
helpers returning typed rows.

Does not belong here: live per-turn provider state (the provider process
owns it), transient UI state (frontend `$state`), logs
(`internal/logging`), and business logic. If a tempting SELECT grows a
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
