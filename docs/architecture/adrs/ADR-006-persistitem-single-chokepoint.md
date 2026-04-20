# ADR-006: `persistItem` Is the Single Write+Emit Chokepoint

Status: accepted
Date: 2026-04-18

## Context

Every timeline row lands in SQLite and fires a
`provider:item_upsert` frontend event. Before the chokepoint rule was
formalized, handlers called `store.UpsertItem` directly and then
(usually) also called `emitItemUpsert`. Two failure modes recurred:

- **Persisted but not emitted.** A handler wrote the row and
  forgot the emit call. The row was visible on refresh but not on
  the live stream, producing "item appears only after reload" bugs.
- **Emitted but not persisted.** A handler emitted an optimistic
  update, then the store write failed silently (logged, not
  surfaced). The frontend saw a ghost row that vanished on reload.

Both bugs were hard to diagnose because the split meant the "wrong"
half of the path could succeed while the other failed.

## Decision

All timeline-row persistence goes through `Router.persistItem` in
`internal/triage/router.go`. The function:

1. Runs the `parent_id` cycle / dangling-reference guard (invariant
   #7).
2. Calls `store.UpsertItem`.
3. Emits `provider:item_upsert` via `emitItemUpsert`.
4. Bumps the `items.persisted` metric.
5. Bumps the `payloads.persisted` metric if a payload was attached.

No caller inside `internal/triage/` calls `store.UpsertItem`
directly. New code reviews enforce this.

## Rationale

- **Atomicity of intent.** A row that's persisted is a row that's
  emitted; a row that's emitted is a row that's persisted. The two
  happen in one function call or not at all.
- **Single place for cross-cutting concerns.** The parent_id guard,
  the metric bumps, future observability hooks — all land in
  `persistItem` rather than being repeated at every caller.
- **Makes the shape of the system visible.** Grepping for
  `persistItem` shows every writer; you don't have to chase
  `store.UpsertItem` callers that may or may not also emit.

Considered alternatives:

- **Lint rule.** We don't have one. A hypothetical rule that
  forbids `store.UpsertItem` calls from within `triage/` would be a
  cleaner enforcement, but code review has been sufficient.
- **Separate `emit` observer on the store.** Rejected: the store
  would need to know about the app-level event emitter, breaking
  the "store has no opinions about what downstream does" property.

## Consequences

- Invariant #12 ("`persistItem` is the single write+emit
  chokepoint") is this ADR promoted to invariant.
- Provider adapters don't call `persistItem` (they have no store
  reference — invariant #13). They produce events; triage calls
  `persistItem`.
- Tests that want to synchronize on persistence use
  `SetEventHook` (see ADR-007) rather than post-hoc SQLite reads.
- Future work: if we add a bulk-persist path for crash-recovery
  backfill, it should also route through `persistItem` or a
  deliberate sibling with the same contract.
