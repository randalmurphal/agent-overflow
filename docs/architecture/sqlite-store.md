# SQLite Store

This document describes the operating contracts of `internal/store`. The exact
schema is defined by `internal/store/schema_v1.go` and the forward-only migration
chain. See [schema.md](schema.md) for table ownership.

## Connections

The store uses one writer connection for writes, migrations, restore, and
checkpoints. A small `query_only` pool serves ordinary reads from WAL
snapshots. In-memory and non-WAL databases use the writer for reads.

Connection-scoped PRAGMAs belong in the DSN assembled by `dsn.go`. Applying
them once with `Exec` is unsafe because `database/sql` may replace a pooled
connection. `verifyConnPragmas` checks the required values at startup because
SQLite accepts unknown PRAGMA names without reporting a typo.

Both pools open through `conngate.go`, which interposes on the driver's
`Connect`. The conversion swap holds that gate so no replacement connection can
attach to a file it is about to rename away.

Reads that depend on connection-local state, including attached restore
databases and PRAGMA probes, use `s.db`. Helpers that may run either directly or
inside a caller transaction accept `sqlExecutor` or `sqlQueryer`.

## Migration model

`schema_v1.go` is a squashed baseline. `migrate.go` and `migration_v*.go` append
changes in version order. A shipped migration is immutable. Changing its SQL
would give two databases the same version with different schemas.

New rebuild migrations state their complete SQL directly. The remaining
`mustReplaceOnce`, `mustReplaceEvery`, and `mustCutFrom` derivations are frozen
legacy migrations guarded by `migrate_freeze_test.go`. Do not extend that
pattern.

A rebuild must carry forward every column, index, trigger, and relationship
added since the source definition. Rebuild migrations temporarily disable
foreign keys on the dedicated writer connection so dropping a parent table does
not cascade into retained children. They restore and verify the connection
policy before the connection returns to ordinary use.

Each schema change has a migration test. Tests should prove constraints and
query behavior, not only that a column or index name exists. A new table also
gets typed accessors and an entry in [schema.md](schema.md).

## History invalidation

The frontend may retain a bounded thread window. `threads.history_rev` advances
for every window-visible mutation. `history_epoch` advances for deletions and
repositioning that a client cannot apply incrementally; every epoch advance also
advances the revision.

Three `items` triggers in `historyRevTriggersSQL` maintain those counters. A
window-visible mutation outside `items`, such as payload content or a plan
decoration projected onto `Item.Meta`, calls `bumpHistoryRevTx` in its own
transaction. Item-coupled writes do not call it because their item mutation
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
`timeline_items` and `timeline_payloads` expose the logical union. Triggers
reject chunk gaps, overlapping identities or positions, and a local row that
would shadow imported history without an explicit override.

The compound views are suitable for unordered set reads, existence probes,
single-turn reads, and single-row lookups. Ordered, limited, and recursive reads
must render the physical arms with `timelineArms` or
`timelineIDSelection`. SQLite otherwise materializes and sorts the full logical
thread before applying the bound.

Payload keys are `(thread_id, id)`. Provider item IDs can repeat between
branches and threads. Payload accessors and joins always use both columns.
`payloads.data`, payload chunks, and full highlight spans load on demand; list
reads carry summaries, metadata, and capped preview spans.

## Schema-owned invariants

Four trigger families ride `items`:

- History triggers maintain revision and epoch counters.
- Payload-GC triggers collect payloads after item deletion when no item in the
  thread references them. Repointing an item does not collect the old payload.
- Imported-history triggers enforce the immutable-base and mutable-overlay
  rules.
- Background-settlement triggers maintain
  `items.meta.live_background_active` as completion siblings arrive, disappear,
  or race with launch materialization.

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
history and background-settlement triggers so the snapshot's counters and
derived flags land as recorded.

The following state is authoritative and must never be treated as disposable
provider history:

- users, devices, sessions, signing keys, recovery codes, passkeys, pairing
  links, refresh secrets, and authentication audit;
- personal device membership and its session attribution;
- queued accepted messages;
- transfer journals, reserved native sessions, and ownership epochs;
- remote command acceptance and retained receipts;
- remote completion watches and notification ownership.

A full snapshot contains identity, membership, queue, and application records,
so `RestoreFrom` replaces those rows from the snapshot. It preserves the live
transfer journal, transfer-session reservations, remote command receipts, and
remote watches instead, because restoring an older copy could revoke a transfer
commit, permit command re-execution, or repeat notification delivery. History
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
