# internal/workflow/engine/

Persisted workflow item/phase FSM coordination, direct run start, project-local
resource semaphores, and startup recovery.

## Invariants

- The command-loop goroutine is the sole owner of mutable scheduler state and
  every FSM transition. Runner callbacks enqueue commands; they never mutate
  state directly.
- `teardown` is the only path that releases resource holders. Normal phase
  exit, parks, failures, cancellation, and crash sweep all use it. It is also
  the only caller of `Runner.Stop` (takeover uses `StopForTakeover`), so an
  agent session or a tool phase's process tree dies in the same place its
  resources are released — including the `runnerStarting` window, where an
  attempt that has not reported yet is still stopped by key. Teardown is
  tree-aware: `teardownUnit` is the per-unit half of the same contract (the
  only place a unit's capacity is released and the only caller of
  `Runner.Stop` for a unit key), an attempt's in-flight units come down before
  the phase releases anything of its own, and `sweepPersistedUnits` then fails
  any row still claiming `running` — the case where no in-memory state
  survived to be torn down.
- There is no queued state. `StartItem` admits an item straight to `running`
  and enters its first phase; back-pressure is a *phase* waiting on resource
  capacity, never an item waiting in line. `waiting` is one FIFO list of held
  starts — phase attempts and fan-out units alike — so freed capacity goes to
  the longest-waiting piece of work regardless of which kind it is.
- Resource capacity comes from the live project profile at acquisition time.
  Acquisitions are sorted and all-or-nothing; names never contend across
  projects.
- Every agent-driver phase implicitly acquires `provider:<provider>` on top of
  its declared resources — the bound on concurrent CLI sessions. Capacity comes
  from the project profile like any other resource and falls back to
  `DefaultProviderCapacity` when undeclared; `provider:` is a reserved name
  prefix in `internal/workflow/profile` validation. A tool-driver phase never
  takes provider capacity, and a frozen agent phase with no provider is a
  wiring error rather than an unbounded start.
- A fan-out phase runs no turn of its own, so it takes no provider slot: its
  units and its join each acquire `provider:<their own provider>`. If the phase
  also held one it would compete with the very units it waits for, and at
  capacity 1 it would deadlock outright. The phase's *declared* resources stay
  phase-scoped — acquired once at entry and held for the whole attempt — so a
  `live-stack` mutex is taken once by the attempt, not once per unit.
- Runner start failures are mapped to typed park reasons by sentinel, never by
  string matching: `ErrSetupFailed` → `setup-failed` (workspace provisioning,
  setup hooks, secret resolution, a process that would not start),
  `ErrWiringFailed` → `wiring-error` (the frozen definition and the live project
  profile cannot produce runnable work — an unbound check/command, a failed
  argv interpolation), everything else → `agent-error`. These are the same
  reasons the engine parks its own equivalents under — an unroutable gate or a
  phase missing from the snapshot is `wiring-error`, a failed resource
  acquisition is `setup-failed` — so a run's park reason does not depend on
  which side of the `Runner` boundary noticed. A new runner failure mode picks
  one of these sentinels or adds one here; it does not fall through to
  `agent-error` by accident.
- Global pause is one engine-level flag (`Pause`, persisted by the app in
  settings and restored through `Config.Paused`). While paused no phase starts
  anywhere; in-flight turns run to completion and their items rest at the next
  phase boundary, still `running` with a held phase start — this is not
  `needs-human`. Unpause replays the held starts through the one
  `startWaiting` release path.
- SQLite is the recovery journal. Startup iterates projects, rebuilds running
  and parked items, and parks interrupted running attempts rather than
  re-running them. Parked and terminal items are evicted from memory; resume
  loads a parked item from SQLite on demand.
- `Answer` is valid only for `needs-human(question)`. It persists a new phase
  attempt whose feedback carries the answer and sets `RunRequest.PriorThreadID`
  from the parked attempt so the runner continues the same provider session.
- `TakeOver` parks a live or parked attempt as `needs-human(taken-over)` through
  teardown, releasing resources and runner timers without touching its
  worktree/provider history. `CompleteTakeover` creates one finalize attempt on
  that same thread; validation exhaustion re-parks as `taken-over`.
- `RerunFailed` is the only `failed → running` edge. It re-stamps the run start
  before the transition and carries the previous attempt's failure feedback into
  the new one; the attempt begins immediately, subject to resources and pause.
- Loop bounds are per fresh entry, not per item lifetime. `loopCounts` derives
  each edge's spend from the persisted attempt rows alone — a loop edge counts
  its traversals since its target phase was last entered from *outside* the
  cycle. Only a non-loop entry into that target refills the budget (a forward
  advance, the run's first attempt, a rerun after `failed`, a human `Resume`
  aimed at another phase); a loop traversal never resets anything, not even a
  sibling edge sharing its target — if it did, two edges aimed at one phase
  would clear each other every lap and iterate forever under their bounds.
  Continuations of the same attempt (an `Answer`, a resume in place, a takeover
  finalize) are not entries and refill nothing, and taken-over attempts are
  skipped outright. Unclassifiable history is treated as a continuation, so the
  derivation can only under-refill, never unbind. The walk assumes attempt rows
  arrive in insertion order, which is why every path that re-enters a
  non-resident item seeds the engine clock from its persisted timestamps first.
  This holds only because `def` rejects cycles closed by forward routes
  (`gate.unbounded-cycle`) and requires a loop target to strictly dominate its
  source (`gate.loop-ancestor`).
- Done-item disposition parking is an item lifecycle action in
  `item_actions.go` and stays serialized through the command loop.
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
- A phase's `access` declaration is enforced at the provider session, not only
  used to derive workspace need. `def.Phase.EffectiveAccess()` is the single
  predicate behind both: `write` → `provider.RuntimeFullAccess` (full access
  inside the run's isolated workspace), `read-only` or unset →
  `provider.RuntimeReadOnly` (mutations denied outright, never prompted). Unset
  means read-only on both axes, so an unannotated phase can only ever be
  under-privileged. The mapping is stamped onto the phase thread row at
  creation (`createWorkflowThread`), which makes it survive restarts, resumes,
  and `Answer` continuations for free — `SessionOptions` are re-derived from
  the row every time. A writing phase in the graph means every phase shares the
  worktree, so workspace is NOT a proxy for access. Tool-driver phases hold no
  provider session; for them `access` affects workspace derivation only, and
  nothing pretends otherwise. An agent phase pinned to a provider that cannot
  apply a runtime mode (`provider.Capabilities.EnforcesRuntimeMode` false) is
  refused with `ErrWiringFailed` rather than started with an inert declaration.

## Fan-out attempts

- Expansion happens at phase entry, from the frozen phase plus the live
  variable context: a static `fan_out:` list expands to itself, and a dynamic
  `over:`/`as:`/`unit:` phase stamps one unit per array element. Width is a
  runtime fact — zero units is legal and runs the join immediately, and an
  `over:` variable that is missing or not an array at runtime is a
  `wiring-error` park, not a crash. Every unit row is persisted `pending`
  before any unit starts.
- Whether an attempt may still launch is derived from its unit statuses
  (`fanOutRun.blocked`), never latched into a flag, so an attempt rebuilt from
  its rows blocks for exactly the reason the live one did. A unit resting
  `failed` stops further launches and parks the run `unit-failed` once the
  in-flight units finish; a unit resting `taken-over` does the same under
  `taken-over`, which outranks a failure. In-flight units are never interrupted
  by a sibling's failure — their work is durable.
- The join is the attempt's final unit. It runs when every unit rests `done` or
  `dropped`, receives their persisted results (id, index, status, outputs,
  branch, worktree, thread) under the reserved `units` variable, and its
  envelope *is* the phase's envelope — so a join failure is an ordinary phase
  failure (`agent-error`), not the unit-failure policy.
- `RetryUnit` / `DropUnit` / `TakeOverUnit` repair an attempt rather than
  replacing it: the phase attempt row is reopened (`ReopenWorkItemPhase`) so
  finished units keep their results, and the run only returns to `running` when
  no unit is left blocking it. A unit that cannot *start* is not a unit failure
  — it parks the attempt under the same sentinel-mapped reason a single-shape
  phase would, because nothing runnable was ever produced.

## Boundaries

- Provider and app/channel implementations live behind `Runner` and `Emitter`.
- Workflow resolution and project-profile loading live behind their narrow
  sources. The frozen `Snapshot` pins definitions, never profile capacities.
- No timers, watchdogs, retry backoff, worktree setup, or transport/app wiring
  belongs in this package. Reliability timers and sub-attempt retries are
  runner-owned; the engine only checks phase-boundary budgets and maps outcomes.
- Persisting the pause flag and emitting it to the frontend is app wiring. The
  engine owns the live flag and the `workflow:engine-state` payload shape.
