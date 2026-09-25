# SQLite Store

This document describes the operating contracts of `internal/store`. The exact
schema is defined by `internal/store/schema_v1.go` and the forward-only migration
chain. See [schema.md](schema.md) for table ownership.

## Connections

The store uses one writer connection for writes, migrations, restore, and
truncating checkpoints. A small `query_only` pool serves ordinary reads from
WAL snapshots and passive checkpoints. In-memory and non-WAL databases use the
writer for both.

Connection-scoped PRAGMAs belong in the DSN assembled by `dsn.go`. Applying
them once with `Exec` is unsafe because `database/sql` may replace a pooled
connection. `verifyConnPragmas` checks the required values at startup because
SQLite accepts unknown PRAGMA names without reporting a typo.

Both pools open through `conngate.go`, which interposes on the driver's
`Connect`. The conversion swap holds that gate so no replacement connection can
attach to a file it is about to rename away.

The writer pool holds one connection (`SetMaxOpenConns(1)`) and the read pool
four (`readPoolConns`). `modernc.org/sqlite` compiles every `Exec` and `Query` that is
not a prepared statement and finalizes it after the call, and SQLite compiles
every trigger a write can fire and every view a read names into the
statement. The gate therefore opens each connection behind a statement cache
(`stmt_cache.go`): the connection keeps its `stmtCacheSize` most recently used
statements that read or write rows compiled, keyed by SQL text, and runs them
again with new bindings. DDL, PRAGMA and transaction control compile per call.
SQLite recompiles a cached statement on its next run after a schema change
from any connection, such as a migration, a deferred phase or a replaced view,
and after a pragma that changes code generation, so a cached statement never
runs against an old schema. A statement whose rows are still open is busy, and
the same text run meanwhile on that connection compiles for the call.
`Conn.Raw` callbacks receive the cache, not the modernc connection.

A statement repeated with a different number of placeholders is a different
cache entry. Where a hot or bulk path repeats one with lists of varying
length, bind the list as one JSON array and read it with `json_each`, as the
history repair's unseal statements do. A compiled `items` insert holds about
165 KB because it carries the table's triggers; other statements hold 1 to
65 KB.

Reads that depend on connection-local state, including attached restore
databases and PRAGMA probes, use `s.db`. Helpers that may run either directly or
inside a caller transaction accept `sqlExecutor` or `sqlQueryer`.

## Migration model

`schema_v1.go` is a squashed baseline. `migrate.go` and `migration_v*.go` append
changes in version order. A shipped migration is immutable: changing its SQL
would give two databases the same version with different schemas. An unshipped
migration may be amended only with a deliberate update of its frozen hash; a
development database that already applied it keeps the earlier SQL.

New rebuild migrations state their complete SQL directly. The remaining
`mustReplaceOnce`, `mustReplaceEvery`, and `mustCutFrom` derivations are frozen
legacy migrations. Do not extend that pattern. `migrate_freeze_test.go` pins the
evaluated SQL of every migration, including referenced schema and trigger text.
Record new versions before deployment; repair deployed versions with a forward
migration.

The chain only moves forward, so an older build cannot run on a database a
newer one migrated. Before it configures or migrates anything, `runMigrations`
refuses a database whose recorded migration version or deferred watermark is
above this build's latest migration, with `SchemaTooNewError`: "database is at
schema v121; this build knows v119; install the newer version". `Store.New`
and the app's boot return it unwrapped, so the boot failure shows that
sentence, and `RestoreFrom` refuses such a snapshot through the same check. The
refusal leaves the file byte-for-byte as it was.

An open with `Options.RefusePendingMigrations` also refuses an existing
database with migrations to apply, with `MigrationsPendingError`, before
anything writes it. A database without an applied migration is created as
usual. The Windows launcher's backend and the macOS and Linux desktop boot
open this way, so a database is migrated only by an update trial that
snapshots it first
([no live migration](../specs/app-update.md#no-live-migration)).

A rebuild must carry forward every column, index, trigger, and relationship
added since the source definition. Rebuild migrations temporarily disable
foreign keys on the dedicated writer connection so dropping a parent table does
not cascade into retained children. They restore and verify the connection
policy before the connection returns to ordinary use. A rebuild runs its
statements one at a time in its transaction, split where `sqlite3_complete`
ends a statement, and reports each index build and the closing foreign key
check to the boot's migration step.

Each schema change has a migration test. Tests should prove constraints and
query behavior, not only that a column or index name exists. A new table also
gets typed accessors and an entry in [schema.md](schema.md).

### Deferred phases

A one-time data fix is a migration ([decisions](../decisions.md#background-maintenance)).
When its work is too long to run while the store opens, the migration carries
a `Deferred` phase (`migrate_deferred.go`), an ordered list of named steps.
The chain applies the migration's SQL and records its version as usual. The
app starts `RunDeferredMigrations` at boot, outside the activation gate, and
joins it at shutdown; the phase runs as paced transactions inside the
background-maintenance budget. A step that replaces the database file waits
for the app's `DeferredHost.AwaitFileSwap`, which holds it behind the
activation gate. A step that applies a rule the store does not own takes it
from the host too: v124's settle of ended agents' rows uses
`DeferredHost.AgentEndRule`, triage's rule, and fails without it.

The gate is `PRAGMA user_version`, the deferred watermark: every phase of a
migration at or below it has finished. The chain's version rows cannot carry
it, because the chain treats `MAX(version)` as applied and later migrations
record before a long phase finishes. A database the chain creates starts at the
latest phase. With nothing pending, boot reads the header value and starts
nothing. `RestoreFrom` takes the snapshot's watermark with its rows, and a
phase that ran across a restore is not recorded and runs again on the restored
rows. `migrate_freeze_test.go` pins the phase's name with the SQL.

A phase is idempotent and its progress is the data. Within a run, a thread,
chunk or batch whose write fails is logged with its id and skipped, and a step
that returns an error counts as one failed item; the run goes on, so one bad
item cannot stall it. A run that finished every item moves the watermark past
the phase and clears its row in `deferred_migration_failures`. A run that left
failed items leaves the watermark and records their count and first error in
that table, which lives in the same file as the watermark and moves with it
on restore. The app raises the notice "History repair incomplete: N items
failed; retrying on next start" (the phase's title, the `KindAppUpdate`
toggle), and the next open runs the phase again: finished work is found done
and only the failed items are retried. There is no attempt cap. A clean run
after a failed one retracts the notice. A quit records nothing and the next
open resumes. A phase is live code: it runs against the current schema and
must keep working as the schema moves.

## History invalidation

The frontend may retain a bounded thread window. `threads.history_rev` advances
for every window-visible mutation. `history_epoch` advances for deletions and
repositioning that a client cannot apply incrementally; every epoch advance also
advances the revision.

Three `items` triggers in `historyRevTriggersSQL` maintain those counters and
the per-row `items.rev` stamp (the thread revision as of the last write that
changed the row's read result, plus the rows a page decorates from it: its
completion sibling and the anchors walked from its parent chain, `stampedRowIDsSQL`).
They depend on `recursive_triggers` being OFF, which `dsn.go` pins and boot
verifies. The update trigger fires on every column but `rev`, so a stamping
write, which writes `rev` alone, does not fire it; no stamp writes another
column. Go never names `rev` in a column list; its touch writes `updated_at`
to itself.

A window-visible mutation outside `items`, such as payload content or a plan
decoration projected onto `Item.Meta`, calls `bumpHistoryRevTx` in its own
transaction. Payload and plan writers instead call
`bumpHistoryRevForPayloadTx` / `bumpHistoryRevForItemTx`, which touch the
owning item rows so the row stamps move with the thread counter and fall back
to the plain bump when the owner is imported history. `UpdatePayloadSpans` is
deliberately excluded: it advances the counters of the holder and of the forks
that show the payload without touching `rev`, because spans are a derived cache
the client version-checks.
Item-coupled writes do not call any of them because their item mutation
already fires the trigger.

`history_bulk_load` is reserved for a transaction that suppresses per-row
trigger work and writes the equivalent aggregate counter change before commit.
It is not a general performance switch.

`SyncThreadWindow` reads the store identity, counters, and returned rows in one
read transaction. The first read pins the WAL snapshot, making the counters and
rows describe the same database state. Splitting the operation permits a newer
counter to be returned with older rows.

`store_meta.backend_id` identifies the database for its lifetime.
`replica_generation` identifies the current history lineage. `RestoreFrom`
preserves the live backend ID and remints the generation because restore can
rewind thread counters below values held by clients.

## Logical history

Imported history has an immutable chunk base and a mutable local overlay.
`timeline_items`, `timeline_payloads`, `timeline_payload_chunks`,
`timeline_edit_file_snapshots` and `timeline_turns` expose the logical union,
including the rows a pointer fork inherits ([Pointer forks](#pointer-forks)).
Triggers reject chunk gaps, overlapping identities or positions, and a local
row that would shadow imported history without an explicit override.

The compound views are suitable for unordered set reads and thread-level
existence probes. Ordered, limited, and recursive reads must render the
physical arms with `timelineArms` or `timelineIDSelection`. SQLite otherwise
materializes and sorts the full logical thread before applying the bound.

The views' imported arm starts from the thread's chunk references, so a lookup
through them probes every chunk the thread references. Lookups by id, by
another indexed key, or by turn render the arms instead. `KeyFirst` starts the
imported arm from an index that leads with the key and ends with `chunk_id`,
then checks the chunk's membership in the thread. `Turn` reads only the
references whose copied turn range can hold the turn
(`idx_thread_import_chunks_turns`); `FromTurn` does the same for a turn and
every later one. `TestImportedLookupsDoNotEnumerateChunks` pins these plans,
including the item and chunk-admission trigger probes.

Payload keys are `(thread_id, id)`. Provider item IDs can repeat between
branches and threads. Payload accessors and joins always use both columns.
`payloads.data`, payload chunks, and full highlight spans load on demand; list
reads carry summaries, metadata, and capped preview spans.

Revert of imported history adds deletion overrides and detaches empty chunks
without materializing the retained prefix.

Attachment ownership is separate from its canonical storage path. An owner
keeps the file available after the thread that stored it is deleted. The final
owner releases the metadata and bytes.

## Pointer forks

A pointer fork stores no copy of the history it inherits. `CreatePointerFork`
records the source and the cut on the thread row (`fork_source_thread_id`,
`fork_cut_turn_index`, `fork_cut_item_index`, `fork_source_title`) and writes
one `thread_fork_lineage` row per ancestor level: the ancestor and the cut the
fork reads it through, at most `forkLineageMaxDepth` levels. A cut is the first
`(turn_index, item_index)` the fork does not inherit. The fork shows an
ancestor's rows below that level's cut unless the fork or a nearer ancestor
lists the id in `thread_fork_hidden`.

Creation writes the lineage, the ids the fork hides (live background launches,
rows an item cut excludes, and everything that hangs off either), copies of the
rows still running in the source settled as interrupted, the source's turn rows
from the one the cut falls in, the question state of the prompts the fork
shows, and a divider row at the cut. Its cost follows the source's unsettled
rows, not its length (`TestPointerForkOfALongThreadIsConstantTime`). The fork's own rows sit at or
after its cut; a row that replaces an inherited one is hidden first
(`trg_items_fork_position`).

### Reads

Each timeline view has lineage arms that apply the cut and hidden-id rules
(`inheritedItemVisibleSQL`). Go-rendered reads (`timelineArms`, item
hydration, payload arms) add lineage arms only for a thread with lineage rows,
one pair per level, so a non-fork read's SQL is unchanged. A correlated read,
whose thread is not known when the SQL is written, reads every level through
one pair. Inherited rows read `rev = -1`. A payload, its append chunks and its
edit snapshots resolve in the nearest thread of the lineage that holds the
payload id: the thread that owns the row referencing it. A search hit on an
ancestor row is a hit in every fork that shows the row, so creation writes no
search rows.

### Copies

A fork takes its own copy of an inherited row only when it must own it
(`fork_lineage.go`):

- copy-on-write, before the fork mutates an inherited row or payload
  (`requireMutableItemTx`, `requireMutablePayloadTx`);
- hand-off, before any thread updates, moves, deletes or hides a row a fork
  reads through it (`handOffIDsTx`, `handOffRemovedIDsTx`,
  `handOffPayloadTx`), so the fork's history stays what it was when the fork
  was made;
- materialization, when the fork's history leaves this database
  (`MaterializeForkHistory`, run by the transfer export). It copies in bounded
  transactions and records each batch's copies in `thread_fork_copied`, with
  the ancestor that held each row. The last transaction copies the rows that
  list attachments, with their ownership, and the turn rows, drops the
  lineage and clears the records, so an unfinished materialization owns no
  attachment through its copies. A transaction that finds an ancestor being
  deleted copies nothing and returns `ErrForkSourceDeleting`, which the
  transfer attempt reports as a public error and retries. A write of the
  fork to a row, or of a row under it, makes the recorded copies on the
  row's parent chain its own (`settleForkCopiesTx`), as a card's write makes
  the inherited anchors on its chain its own; a delete, hide or revert
  settles none.

A copy keeps the row's id, position and content and hides the ancestor's row
from the copier, so the fork reads the same timeline before and after. A copy
also makes the copier an owner of the attachments the row references and its
previous owner owned (`ownCopiedAttachmentsTx`). A writer that changes a row
another thread can read runs the hand-off first;
`TestPointerForkSourceRewritesHandOff` lists the writers.

Copies are bulk writes: they carry no subagent card. An inherited anchor is
read with revision -1 and walked at read time; its copy is served from a stamp.
A copy changes no card a read serves, since the walk and the recompute read
every arm, but only a local anchor holds a stamp and a round counts in a stamp
only when its prompt is local. So a copy recomputes the stamps it changes, as a
localized imported row does (`recomputeLocalizedCardsTx`): a copied anchor with
a visible child, a copied carrier or root a local carrier names, and the root
of a copied resume prompt. A card opened under an inherited anchor copies it
first, so every anchor a card keeps is local.

A fork's revert of inherited rows lowers its cut to the last surviving row and
hides any reverted row still below the new cut (`retractInheritedTx`). The
ancestor's rows stay, and a fork made from this one keeps reading them through
its own lineage.

The turn-error triggers' recompute reads a fork's inherited rows through the
lineage arms, so a write to the fork's own rows or turns keeps the errors it
inherits. A write that changes which inherited rows a fork shows writes no row
those triggers count and recomputes the pair itself (`recomputeTurnErrorsTx`):
its creation, and through `forkViewChangedTx` a revert, a delete of an
inherited row and a source deletion, which also recompute the stamps of the
fork's copied anchors, whose subtrees can hold the rows that leave.

### Triggers and stamps

`forkTriggersSQL` holds the fork triggers; `RestoreFrom` drops them for its row
copy and reinstalls them.

- `trg_items_fork_snapshot` and `trg_items_fork_snapshot_move` hide a row an
  ancestor inserts or moves below a fork's cut after the fork was made, such as
  a late background completion. A row that replaces one the ancestor already
  showed under the same id (its copy of an inherited row, or its own imported
  row localized) stays visible.
- `trg_items_fork_position` and `trg_items_fork_position_update` keep a fork's
  own rows at or after its cut.
- `trg_items_fork_reader_stamp` advances the stamps of the forks that show an
  ancestor row updated in place.
- `trg_threads_fork_source_delete` refuses to delete a thread that forks still
  read through.

A fork's `history_rev` and `history_epoch` are its own counters. A write moves
them only when it changes a row the fork shows, so a source continuing past a
fork's cut leaves the fork's stamps unchanged, and a source write past every
cut costs the same for any number of forks. The rule and its write paths are in
[thread-replica-sync.md](thread-replica-sync.md#pointer-fork-stamps).

### Source deletion

Deleting a thread first marks it `deleting` (`beginThreadDelete`), which
stops every materialization that reads through it. It then rolls back, in
paced transactions and in timeline order, the copies those unfinished
materializations recorded of rows held by the thread or by a thread the
fork reads through it (`rollBackForkCopiesThrough`), so a fork keeps no part
of that history whether its export is running, was stopped or was cut short
by a crash, and reads the same rows meanwhile. Copies of rows a nearer
ancestor holds stay recorded: the fork still reads them after the detach.
One transaction then detaches the forks that read through the thread,
before any item is drained (`detachThreadForks`, `detachForkDescendantsTx`,
which rolls back any such copies still recorded, as the draft and
import-rollback deletes that call it directly need). Each fork drops the lineage
levels at and beyond the deleted thread and keeps its nearer levels, so the
rows the deleted thread owned leave its timeline and its epoch advances. The
divider of every fork made from the deleted thread, including the copies
materialized forks and their forks hold, records `sourceDeleted` and the
source title. From that commit `owned_threads` leaves the thread out, so
`CreatePointerFork` refuses it as a source (`ErrForkSourceDeleted`), the
detach covers every fork it will have, and no fork reads rows the paced drain
is removing. The row goes in the delete's last transaction. A delete that a
crash or an error stopped leaves the mark, and the app completes it at boot
after the first catalog reads (`ListPendingThreadDeletes`).

### Attachments

A fork may read an attachment an ancestor owns while it shows a row that
references it (`OwnsAttachment`). The check runs only for an attachment the
fork does not own itself and reads the ancestors' attachment-bearing rows below
the cut through the partial indexes `idx_items_attachment_refs` and
`idx_import_history_items_attachment_refs`.

### Migration v120

v120 creates the fork schema and retires payload snapshots, the previous way
forks shared payloads. It copies each borrowed payload graph back into the
payload rows that referenced it and empties the snapshot tables, at a cost that
follows the borrowed payloads rather than the size of the payload tables.

## History repair

Removed background sealing moved settled local rows into import chunks whose
ids start with `sealed:`. Before v119, repointing an item at another payload
left the old payload row behind, and a Claude background agent's completion
payload held a copy of the agent's whole transcript. v119's deferred phase,
"History repair", runs three steps:

1. `repairStoredHistory` (`history_repair.go`) folds the sealed rows back
   and prunes payload rows nothing references. A thread whose fold fails keeps
   the rest of its sealed rows, which read as imported history, until the next
   open retries it.
2. `blankLegacyTranscriptCopies` (`transcript_blank.go`) empties those
   transcript copies, as described below.
3. `convertToIncrementalVacuumStep` converts a pre-incremental file
   ([Converting an existing database](#converting-an-existing-database)).

`UnsealThreadHistory` moves a thread's sealed rows into `items` and
`payloads` under `history_bulk_load`, keeping ids, positions, timestamps,
payload bytes, highlight spans and search rowids. Each moved row's insert
probes every sealed reference covering its turn, so chunks go latest turn
first and smallest first within a turn, and rows move in pieces of at most
16. A transaction stops taking pieces after 10 ms, 256 rows or 4 MiB. Between
the transactions of a split chunk, each moved row has an override, the state
`localizeImportedItemTx` leaves; the last piece releases the reference, and
`trg_thread_import_chunks_gc` deletes the chunk with its last reference. The
phase runs after the whole chain, where v120 has already deleted every chunk
no thread references. `pruneOrphanPayloads` deletes payload rows that no
logical timeline row names, at most 256 rows and 4 MiB per transaction,
re-checking the references inside each one. Every repair transaction is
followed by a passive checkpoint on a read-pool connection, which does not
hold the writer. The writer keeps SQLite's default `wal_autocheckpoint` of
1000 pages: the commit that grows the WAL past it checkpoints inside that
commit, whichever write it is. The repair's own checkpoints keep its frames
from becoming that backlog.

Measured on a 5.86 GB copy with 178,267 sealed rows in 15,204 chunks: 9,730
transactions, p50 15 ms, p99 27 ms, max 74 ms; with the processors
oversubscribed and a second repair writing the same disk, p50 17 ms, p99 49 ms,
max 126 ms. A later run of that setup, with the load average between 26 and
59, held the writer p50 15 ms, p99 60 ms, max 406 ms per transaction, against
p50 27 ms, p99 96 ms, max 1.77 s with the checkpoint on the writer. The released pages (72 MB there) stay on the freelist for later
writes; that is below `ReclaimFreeSpace`'s 20% threshold, so the file keeps its
size.

Current builds project a background agent's sidechain transcript into the
thread and write its completion payload with empty data, keeping the output
file, `outputFileState` "loaded" and the report preview in meta. The blank
step selects a `tool_call_result` payload with data and
`meta.outputFileState = 'loaded'` that a background `tool_completion` row
(`completion_of <> ''`) of `Agent`, `Task` or `SendMessage` in a Claude thread
names. It sets `data` to an empty blob and clears `spans`, keeping the row,
meta, preview spans and creation time; item revisions do not move. Monitor and
command output, payloads only a notification names, and foreground results
keep their data. A batch is at most 64 payloads and 4 MiB, re-checks the
selection inside its transaction, and is followed by a passive checkpoint when
it emptied anything.
On the measured copy this is 1,041 `Agent` payloads (1.19 GB) and 141
`SendMessage` payloads (551 MB). The freed pages go to the freelist, and
`ReclaimFreeSpace` returns them to the filesystem.

## Schema-owned invariants

Five trigger families ride `items`:

- History triggers maintain revision and epoch counters and the per-row
  `items.rev` stamp. They do no subagent card work: the cards in
  `subagent_aggregates` are kept in Go (`subagent_card.go`). A live write
  carries its parent's card, which the store feeds in memory and writes to
  the stamps at a flush; a write the card rules do not follow, and every
  bulk writer, recompute the cards whose subtree changed before they
  commit.
- Payload-GC triggers collect a payload once no item in the thread references
  it: after an item is deleted, and after an update repoints an item's
  `payload_id` or `input_payload_id`.
- Imported-history triggers enforce the immutable-base and mutable-overlay
  rules.
- Background-settlement triggers maintain
  `items.meta.live_background_active` as completion siblings arrive, disappear,
  or race with launch materialization.
- Turn-error triggers, also on `turns`, `thread_import_chunks` and
  `thread_import_item_overrides`, keep the thread row's Failed-pill aggregate
  (`thread_turn_error_aggregate.go`). They do not consult
  `history_bulk_load`: each arm is a keyed probe, so bulk paths keep them.

A background `tool_call` remains `status = 'running'`; its terminal state is a
sibling row whose `completion_of` names the launch. The stored liveness flag
allows partial indexes to select genuinely live launches without a correlated
subquery. Tray display includes nested work by background ancestry, while queue
and reaper gates intentionally consider top-level work. These are separate
queries with separate semantics.

## Writes and events

Thread and project mutations that feed update events return the changed row and
a `changed` flag. SQLite counts a matched no-op update as affected, so the
update includes a null-safe `IS NOT` change predicate. When no row is returned,
the accessor probes under the eligibility predicate to distinguish an existing
no-op from a missing row.

Changed rows are read inside the write transaction. Single-row thread and
project writers use `applyThreadRowWrite` and `applyProjectRowWrite`; multi-row
operations retain the same `RETURNING id` and in-transaction readback rule.

State with a narrower lifecycle stays out of broad projections and
`UpdateThread`. Dedicated accessors own import provenance, live todo, group
membership, worktree setup state, and similar columns. This prevents a stale
whole-row value from clobbering a concurrent lifecycle transition.

## Restore boundary

`RestoreFrom` replaces the history dataset. It refuses restore while a remote
command or transfer phase makes replacement unsafe, and it rejects snapshots
that predate current incoming ownership. During the copy it drops and recreates
history, background-settlement, pointer-fork, attachment-ownership,
chunk-admission and turn-error triggers. It restores the complete reference graph before
reinstating them in the same transaction, preserving recorded counters and
derived flags.

The following state is authoritative and must never be treated as disposable
provider history:

- users, devices, sessions, signing keys, recovery codes, passkeys, pairing
  links, refresh secrets, and authentication audit;
- personal device membership and its session attribution;
- queued accepted messages;
- transfer journals, reserved native sessions, and ownership epochs;
- remote command acceptance and retained receipts;
- remote completion watches and notification ownership;
- agent thread requests and the receipts that answer them.

A full snapshot contains identity, membership, queue, and application records,
so `RestoreFrom` replaces those rows from the snapshot. It preserves the live
transfer journal, transfer-session reservations, remote command receipts,
remote watches, and thread requests and receipts instead, because restoring an
older copy could revoke a transfer commit, permit command re-execution, repeat
notification delivery, or re-run a request another computer already accepted.
The `thread_search*` family is neither replaced nor preserved: the index is
derived from history, so restore recreates it empty and the background build
re-derives it. History
retention and generic table sweeps still use the broader authoritative
classification.

## Query plans

SQLite uses a partial index only when the query text implies its predicate.
Queries must repeat qualifying terms such as `completion_of <> ''`,
`parent_id <> ''`, `parent_item_id <> ''`, and `source_ref <> ''`, even when a
correlated equality appears logically sufficient.

Timeline windows and has-more probes count top-level rows only. Subagent child
rows load on demand so a child-heavy turn cannot consume a window budget or
produce a load-more action with no visible rows.

For plan-sensitive changes, pair a result-parity test with `EXPLAIN QUERY PLAN`.
The plan assertion should reject full timeline scans or temporary sorts and
include a negative control when practical.

## WAL maintenance

Passive checkpoints recycle WAL pages but do not shrink the file.
`PassiveCheckpoint` runs on a read-pool connection: a checkpoint takes the
checkpointer lock, not the write lock, so commits continue while it copies. It
uses the writer when there is no read pool or reads are quiesced.
`TruncateCheckpoint` needs every reader gone, so it quiesces the read pool and
routes reads onto the writer for its duration. That makes one stalled reader a
stall for every reader, so it runs only where quiescence is structurally free:
after migration at boot, and during `Store.Close` after the read pool closes.
It does not belong on a user-facing path or in a periodic sweep.

SQLite reports a blocked truncate checkpoint as a result row. Callers must
inspect `CheckpointResult.Busy` as well as the error.

## Free space

Deleted rows leave pages on the freelist. Later writes reuse them, so a
database with a large freelist does not keep growing, but nothing returns the
space to the filesystem on its own.

Under WAL, `VACUUM` blocks no readers, but it blocks every write for as long as
it takes to rewrite the database (measured: 10-17 s on 4.4 GB), grows the WAL by
about the size of the database, and needs roughly twice the live size on disk.
It is not run by the application.

`ReclaimFreeSpace` shrinks the file instead by looping
`PRAGMA incremental_vacuum(128)` on the writer with a pause between chunks.
Each chunk is its own implicit transaction, so a concurrent writer waits at
most one chunk and readers are unaffected. Measured on 4.4 GB: 97 ms per chunk
worst case, 79 ms worst-case writer wait, 8 MB WAL peak, no extra disk. Larger
chunks push writers past 100 ms; do not raise the chunk size. The freelist
thresholds (64 MB and 20%) gate the work, and cancelling the context stops it
at a chunk boundary because reclamation has no deadline. Under WAL the
shortened database lands in the WAL, so the file itself shrinks at the next
checkpoint.

`incremental_vacuum` needs `auto_vacuum=incremental`, which is a property of the
file, fixed when its first table is created. `configureDatabase` sets it before
the schema exists, so every database this build creates has it; on an existing
database the same statement is a silent no-op, and so is `incremental_vacuum`
itself. `ReclaimFreeSpace` therefore checks the mode and reports an
unconvertible database once rather than appearing to work.

## Converting an existing database

`ConvertToIncrementalVacuum` rebuilds a pre-incremental file as a snapshot swap
rather than with `VACUUM`:

1. `VACUUM INTO` a `.incremental.tmp` beside the database, from a dedicated
   connection carrying `auto_vacuum=INCREMENTAL`. Readers and writers are
   unaffected. The snapshot is then given WAL mode and checked for schema
   version and mode.
2. Take the writer connection, quiesce and drain the read pool, and re-read
   `PRAGMA data_version` on the snapshot's connection. `VACUUM INTO` copies the
   database as of the start of its read transaction, so any commit during it is
   missing from the copy; a changed value means the result is `ConvertNotQuiet`
   and the snapshot is deleted.
3. Truncate-checkpoint on the held writer, retire it with
   `Conn.Raw` returning `driver.ErrBadConn`, drop the read pool's connections
   with `SetMaxIdleConns(0)`, and confirm the database's `-wal` and `-shm` are
   gone. SQLite deletes both when the last connection closes, so their absence
   is the proof that no connection, in this process or another, still holds the
   file. Then rename the old file aside, rename the snapshot into place, and
   release the gate. Both pools reopen lazily against the same path.

The outgoing file is unlinked after the swap is visible, because unlinking a
multi-gigabyte file costs about as much as the swap itself. The measured
blocked window is a few milliseconds.

The conversion runs as the last step of v119's deferred phase. A file that is
already incremental finishes the step on its first check. Otherwise the step
calls the app's wait (`storeFileSwapWait`) before each attempt. The wait holds
until the activation gate opens, because a supervisor trial's rollback does
not cover a replaced file; then it samples every 15 s until no turn is live,
nothing has committed for 60 s, and the run's previous attempt is at least an
hour old, and closes its `CommitWatcher` before returning. `ConvertNotQuiet`
waits again; `ConvertAlreadyIncremental` and `ConvertUnsupported` finish the
step. An error, or a wait that fails, is a failed item the next open retries.

Crash safety is by file state, repaired in `Store.New` before anything opens the
database: a leftover `.incremental.tmp` is always stale and is removed, a
`.replaced` beside a live database is removed, and a `.replaced` with no live
database is the whole database and is renamed back.

`RestoreFrom` and the conversion share `fileMu`. Restore depends on a sequence
of statements landing on one writer connection, and the swap retires that
connection.

`CommitWatcher` owns a connection of its own because `PRAGMA data_version` only
reports other connections' commits and is comparable only against an earlier
read from the same connection. Close it before converting; an open connection
refuses the swap.
