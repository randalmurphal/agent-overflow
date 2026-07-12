# internal/workflow/engine/

Persisted workflow item/phase FSM coordination, queue draining, project-local
resource semaphores, and startup recovery.

## Invariants

- The command-loop goroutine is the sole owner of mutable scheduler state and
  every FSM transition. Runner callbacks enqueue commands; they never mutate
  state directly.
- `teardown` is the only path that releases resource holders. Normal phase
  exit, parks, failures, cancellation, and crash sweep all use it.
- Resource capacity comes from the live project profile at acquisition time.
  Acquisitions are sorted and all-or-nothing; names never contend across
  projects.
- SQLite is the recovery journal. Startup iterates projects, rebuilds only
  queued/running scheduler state, and parks interrupted running attempts
  rather than re-running them. Parked and terminal items are evicted from
  memory; resume loads a parked item from SQLite on demand.
- The optional process-N queue bound belongs only to `SetQueue` in memory and
  never survives restart.

## Boundaries

- Provider and app/channel implementations live behind `Runner` and `Emitter`.
- Workflow resolution and project-profile loading live behind their narrow
  sources. The frozen `Snapshot` pins definitions, never profile capacities.
- No timers, watchdogs, retry backoff, worktree setup, budget enforcement, or
  transport/app wiring belongs in this packet/package yet.
