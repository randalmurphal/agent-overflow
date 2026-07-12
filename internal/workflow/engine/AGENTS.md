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
- `Answer` is valid only for `needs-human(question)`. It persists a new phase
  attempt whose feedback carries the answer and sets `RunRequest.PriorThreadID`
  from the parked attempt so the runner continues the same provider session.
- `TakeOver` parks a live or parked attempt as `needs-human(taken-over)` through
  teardown, releasing resources and runner timers without touching its
  worktree/provider history. `CompleteTakeover` creates one finalize attempt on
  that same thread; validation exhaustion re-parks as `taken-over`.
- `SetQueue` explicitly replaces the transient process-N budget. Settings-only
  changes use `UpdateQueueSettings`, which preserves that live budget.
- Queued removal and done-item disposition parking are item lifecycle actions
  in `item_actions.go`; both stay serialized through the command loop.
- Per-item budgets are checked before every phase attempt. Item overrides win
  over live profile defaults; token/USD spend comes through `SpendSource`, and
  wall clock uses the engine clock against the persisted item start time.
- Worktree provisioning, setup hooks, artifact copying, and cleanup execution
  stay runner/app-owned. The engine only maps typed setup failures and parks
  step-mode automatic decisions without rewriting their persisted gate trace.
- `Runner.Start` runs on a worker goroutine. Its keyed result re-enters the
  command loop before mutating FSM state, while the initiating API caller waits
  outside the owner loop so cancellation and unrelated commands remain live.
- Cleanup policy is plumbing only until disposition lands: read-only workflows
  have no worktree, and writing workflows retain theirs in every terminal state.

## Boundaries

- Provider and app/channel implementations live behind `Runner` and `Emitter`.
- Workflow resolution and project-profile loading live behind their narrow
  sources. The frozen `Snapshot` pins definitions, never profile capacities.
- No timers, watchdogs, retry backoff, worktree setup, or transport/app wiring
  belongs in this package. Reliability timers and sub-attempt retries are
  runner-owned; the engine only checks phase-boundary budgets and maps outcomes.
