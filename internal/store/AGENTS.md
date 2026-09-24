# `internal/store`

This package owns SQLite schema, migrations, and typed persistence accessors. It
uses `modernc.org/sqlite`, WAL mode, one writer connection, and a small
`query_only` read pool.

Start with:

- [`docs/architecture/schema.md`](../../docs/architecture/schema.md) for table
  ownership and the boundary between replaceable history and authoritative
  state.
- [`docs/architecture/sqlite-store.md`](../../docs/architecture/sqlite-store.md)
  for connections, migrations, restore, triggers, and query rules.
- [`docs/architecture/thread-replica-sync.md`](../../docs/architecture/thread-replica-sync.md)
  before changing `history_rev`, `history_epoch`, or `SyncThreadWindow`.
- [`docs/architecture/data-flow.md`](../../docs/architecture/data-flow.md) for
  persistence timing and heavy-payload loading.

## Responsibility boundary

Store provider history as a cache. Provider session files remain authoritative
for conversation recovery. Derived render data such as highlight spans and
provider cost estimates is also cache content and may be discarded when stale.

Some SQLite rows are authoritative because no provider history can reconstruct
them. This includes account and credential records, personal membership,
accepted messages awaiting dispatch, transfer ownership and recovery state,
remote command acceptance, remote completion notification ownership, and
agent thread requests with the receipts that answer them. Never
apply generic history retention or cleanup logic to these tables. Snapshot
restore must handle each authoritative family deliberately rather than treating
it as disposable provider history.

Persist data and constraints here. Keep provider lifecycle state in the provider
packages, workflow scheduling and transitions in `internal/workflow`, identity
policy and credential operations in `internal/identity`, transient presentation
state in the frontend, and logs in `internal/logging`. Store accessors may make
an atomic persistence decision; they must not become a business-logic layer.

## Schema and migrations

- `schema_v1.go` is the squashed baseline. `migrate.go` and
  `migration_v*.go` are the forward-only chain. A migration applied to persistent
  data, including by an uncommitted development build, is deployed and immutable.
  Add a migration and a test; record each new version in `migrate_freeze_test.go`.
- New rebuild migrations contain their final SQL directly. The old
  `mustReplaceOnce`, `mustReplaceEvery`, and `mustCutFrom` derivations are
  frozen compatibility code, not a pattern for new migrations.
- A table rebuild must preserve every current column, index, trigger, and child
  relationship. Rebuilds run with foreign keys disabled on the dedicated writer
  connection; restore enforcement before that connection is reused.
- Add typed accessors with a new table. Update
  [`schema.md`](../../docs/architecture/schema.md) for every table and for
  indexes or triggers whose purpose is not evident from their declaration.
- Keep enum constraints and their Go validators aligned. The runtime-mode and
  reasoning-effort sets intentionally remain provider-free production code;
  cross-package tests pin them to `internal/provider`.

## Transactions and writes

- Keep operations atomic at the ownership boundary. History clone, import,
  placement, identity revocation, transfer state, and queue handoff all require
  one transaction when partial success would create an invalid state.
- Use `sqlExecutor` and `sqlQueryer` for helpers that must work on a pool or a
  caller transaction. Reads of connection-local state stay on `s.db`.
- Return whether a row actually changed when callers emit update events.
  SQLite rows-affected proves that a row matched, not that its value changed.
  Put a null-safe `IS NOT` change predicate in the update and distinguish a
  no-op from a missing row.
- Read back changed rows inside the write transaction. Use the shared row-write
  helpers for single thread and project rows; write equivalent explicit logic
  for multi-row operations.
- Keep narrow state out of broad projections and whole-row updates. Columns
  such as import provenance, live todo, group membership, and worktree setup
  state have dedicated writers so a stale struct cannot overwrite them.
- Payload identity is `(thread_id, id)`. Turn identity is thread-scoped. Every
  accessor and join must carry the full identity.

## History and trigger contracts

- Item triggers maintain history stamps, subagent anchor cards, the thread
  row's turn-error aggregate, payload garbage collection, imported history
  integrity, and background-launch settlement. Do not duplicate or bypass
  those invariants in Go.
- A write to a payload or plan row an item renders calls
  `bumpHistoryRevForItemTx` / `bumpHistoryRevForPayloadTx` so the owning row's
  `rev` moves with the thread stamp; plain `bumpHistoryRevTx` is only for a
  window-visible write with no owning item row. `UpdatePayloadSpans` bumps the
  thread alone: spans are a client-version-checked cache. An item-coupled write
  relies on the item trigger and must not double-bump.
- `SyncThreadWindow` reads store identity, stamps, and rows in one read
  transaction so they describe one WAL snapshot.
- `history_bulk_load` may suppress stamp triggers only in a transaction that
  writes the exact aggregate revision before commit and recomputes the
  subagent cards of every subtree it changed (`subagent_aggregate_stamps.go`).
- Logical timeline reads include mutable and imported history. Ordered, limited,
  or recursive reads use `timelineArms` or `timelineIDSelection`; do not put
  `ORDER BY`, `LIMIT`, or a recursive step over `timeline_items`.
- Background tool launches remain `running`; a sibling identified by
  `completion_of` carries completion. Use the schema-maintained
  `live_background_active` projection and preserve the distinction between tray
  visibility and queue-blocking top-level work.

## Query and connection rules

- Never use `SELECT *`. Keep list projections explicit and leave heavy
  `payloads.data` and full spans for on-demand reads.
- State partial-index predicates explicitly in queries. SQLite must be able to
  prove terms such as `completion_of <> ''`, `parent_id <> ''`, and
  `source_ref <> ''` from the SQL text.
- Window sizes, run members and has-more probes use the same timeline
  selection: top-level rows for the main thread, direct children for an agent
  scope. Keep selection separate from wire page shape.
- Put connection-scoped PRAGMAs in the DSN. A post-open `Exec` does not cover
  replacement pooled connections. Keep boot verification for required PRAGMAs.
- `TruncateCheckpoint` quiesces readers and reports contention through
  `CheckpointResult.Busy`; checking only the error is insufficient. Quiescing
  stalls every read, so it stays at boot and `Close`, never on a sweep.
- Do not run a plain `VACUUM` on the live database; `VACUUM INTO`, which
  `SnapshotTo` uses, is online-safe. Free space is reclaimed by
  `ReclaimFreeSpace` in paced
  `incremental_vacuum` chunks, and an existing database is converted to
  incremental auto-vacuum by `ConvertToIncrementalVacuum`. Read
  [sqlite-store.md](../../docs/architecture/sqlite-store.md#free-space) before
  changing either, or before adding an operation that replaces or reopens the
  database file.

## Testing

Every schema change needs a migration test that proves both shape and behavior.
Query work on timeline, lifecycle, workflow, or restore paths needs result
parity and a plan assertion when index use is part of the contract. Use
`storetest` or a `t.TempDir()` database and never share a database file between
tests.
