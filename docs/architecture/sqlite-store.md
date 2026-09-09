# SQLite Store

This document describes the operating contracts of `internal/store`. The exact
schema is defined by `internal/store/schema_v1.go` and the forward-only migration
chain. See [schema.md](schema.md) for table ownership.

## Connections

The store uses one writer connection for writes, migrations, restore,
checkpoint, and `VACUUM`. A small `query_only` pool serves ordinary reads from
WAL snapshots. In-memory and non-WAL databases use the writer for reads.

Connection-scoped PRAGMAs belong in the DSN assembled by `dsn.go`. Applying
them once with `Exec` is unsafe because `database/sql` may replace a pooled
connection. `verifyConnPragmas` checks the required values at startup because
SQLite accepts unknown PRAGMA names without reporting a typo.

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
`TruncateCheckpoint` runs when reader quiescence is cheap: after migration at
boot, during `Store.Close` after the read pool closes, and after retention
`VACUUM`.

SQLite reports a blocked truncate checkpoint as a result row. Callers must
inspect `CheckpointResult.Busy` as well as the error. The store quiesces its
read pool before truncate checkpoint and `VACUUM`; new hot-path callers should
use passive checkpointing.
