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
- Resource capacity comes from the live project profile at acquisition time
  (`liveProfile`, the one read both acquisition and fan-out expansion go
  through). Acquisitions are sorted and all-or-nothing; names never contend
  across projects. A profile source that answers nil without an error is a
  broken source, not an unbounded project, and is refused as such.
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
- **Pause is a root action over a whole tree, and it joins the teardown
  contract.** `PauseItem` refuses a called run ("pause the run that called
  it"), walks the subtree deepest-first, and takes each live member down
  through `teardown` — interrupt the in-flight turn(s), release locks, write
  the partial envelope, park `needs-human(paused)`. It differs from cancel in
  exactly one respect: `teardownRequest.retainCallChildren` keeps the call
  children the phase is waiting on, because pause does not abandon the
  attempt. Members already parked for another reason keep that reason; a pause
  never rewrites why a run needs a human. A persisted-`running` member the
  scheduler does not hold is an error, not a skip — it would be a run nothing
  could ever stop. `PauseAllActive` is the same path over every active root
  and is what graceful quit calls before provider sessions die.
- **A soft stop is checked at call boundaries and nowhere else (D36).**
  `SetSoftStop` arms a standing request on the tree's ROOT (it refuses a called
  run, and refuses arming a run that is not `running`); `startCall` reads it
  before it resolves a target or writes a child row, and `parkSoftStop` clears
  it and parks that run `needs-human(checkpoint)`. The clear happens BEFORE the
  teardown: a cleared flag with a failed park is a loud error, while a set flag
  with a successful park would re-park the tree on every resume. It is
  deliberately NOT checked in `startUnitCall` — a unit call is work inside a
  wave, and stopping there strands the siblings its join waits for. The request
  is written only from the command loop, because the boundary's read and its
  clear live there too; `treeRoot` always re-reads the root row for the same
  reason.
- **`paused`, `interrupted`, and `checkpoint` resume identically.** All three
  stopped a run before the phase it was in produced a result, so `ResumeItem`
  continues on the session that parked rather than re-entering the phase: a
  single-shape attempt through `continueParkedAttempt`, a fan-out through the
  same repair `RetryUnit` applies (every unit resting `failed` reopened at
  once) and then either relaunch or `continueFanOutJoin`, a call phase by
  reopening the attempt and recursing into the child. Descendants parked for
  another reason stay parked and the root returns to waiting on them. A
  `checkpoint` park has no session to continue — its call phase never started
  one — so its resume is the call edge the stop skipped. The reasons differ
  for the human reading the run list, not for the recovery. A parked attempt
  whose thread no longer exists falls back to a fresh attempt whose feedback
  says so — the `ThreadExists` probe exists so a deleted session parks as
  itself instead of as an `agent-error` from a failed runner start.
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
- **`expandFanOut` is where the project's fan-out ceiling is enforced** (D29),
  between `def.ExpandUnits` and `CreateWorkItemUnits`, so a refusal leaves no
  unit row, no sub-worktree, and no provider session. It **refuses, never
  truncates**: an expansion wider than `def.EffectiveMaxFanOutWidth` of the
  *live* profile parks `needs-human(wiring-error)` — the same reason the
  unusable `over:` above takes, because a width the project forbids is again
  the frozen definition and the live context failing to produce runnable work.
  It applies to static lists too, not just dynamic ones: a frozen snapshot is
  decoded and never re-validated, so a run predating the rule or a project that
  lowered its ceiling mid-flight reaches here with no dry-run finding behind
  it. A profile that cannot be read parks `setup-failed` rather than expanding
  unbounded. `parkFanOutSetup` writes the cause into the attempt's envelope
  (`parkCauseEnvelope`), since no unit ran to author one and the resolved width
  is stated nowhere else. The same live-profile read also backs
  `noteFanOutCapacity`, which logs (never emits) when a wave is wider than the
  provider capacity its units will contend on — inside the ceiling but over
  capacity is pacing, not a refusal, and a wave of eight against a bound of two
  is otherwise indistinguishable from a slow provider. `restoreFanOut`
  deliberately does **not** re-check:
  lowering a ceiling must not make an attempt whose rows already exist
  unrecoverable.
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
  failure (`agent-error`), not the unit-failure policy. Those results are what
  the STORE knows; the per-unit git state a merge join actually decides on
  (`commitsAhead`, `dirty`) is added by the app runner on the way in
  (`app_workflow_units.go` `enrichJoinUnits`), because this package is git-free
  by boundary. `def` declares the whole shape, enriched fields included, so the
  prompt validator and the runtime context cannot disagree about what
  `{{units}}` renders.
- **Reopening a unit goes through `reopenUnit`.** Bumping the try number,
  attaching the feedback, clearing the envelope, and persisting all of it via
  `store.RetryWorkItemUnit` is one helper shared by `RetryUnit`,
  `RetryFailedUnits`, the fan-out resume, and the join continuation. The try
  number and the note are PERSISTED, not only held in memory, so an evicted and
  restored attempt comes back on the try it is actually on with the note that
  told it what to do differently. The engine computes the next number once and
  the later `StartWorkItemUnit` writes the same value, so there is no
  double-bump.
- `RetryUnit` / `RetryFailedUnits` / `DropUnit` / `TakeOverUnit` repair an
  attempt rather than replacing it: the phase attempt row is reopened (`ReopenWorkItemPhase`) so
  finished units keep their results, and the run only returns to `running` when
  no unit is left blocking it. A unit that cannot *start* is not a unit failure
  — it parks the attempt under the same sentinel-mapped reason a single-shape
  phase would, because nothing runnable was ever produced.
- `RetryFailedUnits` is `RetryUnit` over every unit resting `failed`, as ONE
  command (D33). It exists because the failure it repairs usually has one cause
  hitting many units at once — a provider usage limit stopping most of a wide
  fan-out — and it is one command rather than N submitted retries because the
  loop serializes commands but not the gaps between them: a half-repaired
  attempt must not be observable by a concurrent drop or single retry. It
  collects the failed set before its first write, so "nothing was failed" is a
  refusal that changed nothing; it leaves `taken-over` units to the human and
  the attempt parked on them; and it resumes through `resumeRepairedFanOut`
  like the single retry, so the repaired units are admitted one at a time
  through `acquireUnitResources` and queue in the shared FIFO rather than
  bursting past the provider bound.
- A phase-level continuation of a fan-out attempt is a continuation of its
  **join**, because the join's envelope is the phase's: `Answer` and
  `CompleteTakeover` route through `continueFanOutJoin`, which re-runs only the
  join (a fresh try on the thread the attempt parked on, carrying the answer as
  feedback) instead of re-entering the phase and re-expanding every unit. The
  work units keep the results the join exists to consolidate. It is refused
  when the join never ran ("repair its units instead") or while any unit is
  still blocking, because there would be nothing coherent to consolidate.
  `item.priorThreadID` / `takeoverFinalize` are consumed by the join alone;
  a work unit never inherits them, so a continuation cannot leak into the next
  phase's first attempt. The app runner is what makes this resolvable at all —
  it stamps the join's thread onto the phase attempt row (`AttachWorkItemPhaseRun`)
  the moment the join's thread exists.

## Call phases

- **Two call edges share one implementation.** `invokeCall` is everything up to
  the workspace decision — the ancestry walk, both depth bounds, the live
  `ResolveCall`, and the linkage that makes the child recoverable — and both a
  `shape: call` phase (`startCall`) and a call-bound fan-out unit
  (`startUnitCall`) go through it. `callEdge` is what tells them apart:
  `{phase}` for one, `{phase, unit}` for the other, which is also how a
  declared `max_depth` is counted (per (workflow, phase, unit), so sibling
  units never spend each other's budget) and how a child's `SourceRef` reads
  (`item/phase[unit]/attempt`). See "Call units" below for what only the unit
  edge does.
- A `shape: call` phase runs no work of its own: it resolves its static target
  **at call time** (a fresh `DefinitionSource.ResolveCall` per invocation, §8
  scoping), evaluates its `args:` against the caller's variable context, and
  starts the child as an ordinary run linked by `parent_item_id` /
  `parent_phase_id` / `parent_attempt`. The caller then rests — still `running`,
  holding no runner, no resources, and no provider capacity — until the child
  reaches a terminal state.
- The parent attempt row is persisted (with its evaluated args) **before** the
  child exists. That order is what makes a crash recoverable: the only gap it
  can open is an attempt with no child, which `recoverCall` re-invokes in place
  from the persisted args — no new attempt row, so phase history and every loop
  count derived from it stay honest. The reverse order could leave a child run
  no attempt claims. On rebuild, an alive child leaves the parent resting and a
  terminal child settles it exactly as a live completion would, so rebuild order
  does not matter.
- The call phase's envelope is synthesized, never authored: a `done` child
  becomes `{status: done, outputs: <the child workflow's declared outputs>}` and
  the parent's gate routes on those names. A `failed` child fails the parent
  phase under `child-failed` (a `RerunFailed` then makes a *fresh* call, never
  resumes the old child); a `cancelled` child parks the parent `agent-error`,
  because cancelling the parent too is the human's call. A child that parks
  `needs-human` is not terminal — the parent keeps waiting, and the child's
  eventual completion resumes it.
- Child→parent notification never runs inside the child's own transition. The
  terminal transition appends to the loop's `deferred` queue, which is drained
  after the current command settles, so the parent's phase completion is an
  ordinary serialized step rather than a re-entrant teardown.
- Depth is bounded twice: the author's `max_depth` on the edge (counted as the
  number of times *this* (workflow, phase) edge already appears in the run's
  ancestry) and the engine's absolute `MaxCallDepth`. The absolute one exists
  because children resolve live — an edit landing after the parent's dry-run can
  introduce a cycle validation never saw. Exceeding either parks the run that
  tried to recurse as `wiring-error`, carrying the rendered call chain in the
  attempt's envelope.
- Workspace flows down and is never provisioned by a child (§9): the child row
  is stamped with the caller's worktree/branch/base branch at creation, so a
  self-calling workflow iterates in one worktree. The workspace columns belong to
  the runner, so `startCall` re-reads the caller's row before stamping — the
  engine's in-memory item still carries what they were at start. Stamping can
  legitimately copy nothing (a read-only tree, or a call that is the root's first
  phase, which runs before any worktree exists); the runner then resolves the
  *root's* workspace through parent linkage and stamps the answer back, so the
  tree still has exactly one. The root's provisioning uses the propagated need
  (`def.PropagatedWorkspaceNeed`), frozen into the snapshot and carried on
  `RunRequest.WorkspaceNeed` — a read-only caller of a writing workflow still
  gets a worktree.
- Budgets are per tree, enforced against the root (§12): `enforceBudget` resolves
  the tree's root through parent linkage (cached on the runtime item), reads the
  root's budget envelope and persisted start time, and prices the whole tree
  through `SpendSource.TreeSpend`. A child carries no budget of its own —
  `startNewItem` refuses one — and the item parked is the one whose check
  tripped, not the root.
- Teardown is tree-aware (`call_cancel.go`): leaving a call phase for any reason
  (cancel, rerun after failure, takeover, crash park) brings the live descendant
  subtree down first, deepest child before its parent, through the same teardown
  contract. A descendant the scheduler evicted (parked) is transitioned in place
  — it holds no resources and no runner, so the transition *is* its teardown.

## Call units

- A **call-bound fan-out unit** (`unit_calls.go`) is the phase-scoped call one
  level down: `startUnitCall` persists the unit `running`, evaluates its args
  against `unitVars` (the attempt's variables plus the `as:` binding), and
  invokes the edge. The unit then rests holding no runner, no resources, and no
  provider capacity — `unitResources` returns nil for it, and `resting()`
  counts a `running` status with no runner flags as resting, which is a
  legitimate state **for a call unit and only for a call unit**. That is what
  makes it the safe discriminator every recovery path keys on.
- **The child gets no stamped workspace.** `callInvocation.inheritWorkspace` is
  false here alone: isolation is introduced by fan-out (§9), so the child runs
  in the *unit's* sub-worktree, which the app resolves through the child's
  `parent_unit_id` linkage and cuts with the same `provisionUnitWorktree` a
  writing agent unit goes through. Stamping the caller's worktree would put
  every unit's child back in the one checkout the fan-out exists to keep them
  out of.
- **Outcome mapping** (`settleUnitCallChild`): `done` → the unit `done` with
  the child workflow's declared outputs as its envelope; `failed` **or**
  `cancelled` → the unit `failed` with a note naming the run (there is no
  cancelled unit status, and to a fan-out both mean "this unit produced no
  result"); `needs-human` → not terminal, the unit keeps waiting. A `done`
  child whose declared outputs cannot be read fails *that unit*, not the
  attempt — the siblings' work is durable. From there the ordinary unit-failure
  policy applies. A completion from a child the unit is no longer waiting on (a
  retry replaced it) is stale and ignored, decided by comparing against the
  newest child of that unit key rather than by any in-memory flag.
- **A call that cannot be made is not a unit failure.** Unresolvable args, a
  depth refusal, or a failed persist park the whole attempt under the
  phase-level reason a single-shape phase would take, with the cause written
  into the attempt's envelope — nothing runnable was ever produced, so there is
  no unit outcome to record.
- **Recovery keeps the children.** `recoverFanOutCalls` is the rebuild's
  adoption path: it fails the attempt's runner-backed rows (that process is
  gone), restores the fan, and — if any call unit is still resting — acquires
  the phase's resources and re-links each one. A unit with no child re-invokes
  in place, which is safe because the unit row is written before the child
  exists; the reverse order would leave a child no unit row claims. If the
  resources are held by work this rebuild already adopted, it declines and the
  run takes the ordinary interrupted park, bringing its children down with it.
  `resumeUnitCallChildren` is the same re-link for a resume, and pause reaches
  it through `retainCallChildren`, which keeps a running call unit's row and
  its child intact.
- **`cancelCallChildren` (`call_cancel.go`) reads a fan-out's children from the store**, not from
  `item.fan`. The crash-rebuild path tears an attempt down with no in-memory
  unit state at all, and that is exactly the case where a stranded grandchild
  would otherwise survive the restart.
- **`TakeOverUnit` refuses a call unit.** There is no session to steer; the
  action belongs on the child run.

## Boundaries

- Provider and app/channel implementations live behind `Runner` and `Emitter`.
- Workflow resolution and project-profile loading live behind their narrow
  sources. The frozen `Snapshot` pins definitions, never profile capacities.
- No timers, watchdogs, retry backoff, worktree setup, or transport/app wiring
  belongs in this package. Reliability timers and sub-attempt retries are
  runner-owned; the engine only checks phase-boundary budgets and maps outcomes.
- Persisting the pause flag and emitting it to the frontend is app wiring. The
  engine owns the live flag and the `workflow:engine-state` payload shape.
- Waking a bound thread is app wiring too. The engine's contribution is the
  `workflow:item-state` transition; `app_workflow_notifications.go` decides
  from that one event who is told and how (a wake into the run's bound thread,
  an OS notification when it needs a human, a descendant's park announced at
  its root). Discard — worktree removal and branch deletion — is entirely
  app-side; the engine only cancels what is still in flight when asked.
- Automations are app-fed and engine-unaware. `internal/workflow/scheduler`
  never imports this package: its internal-event triggers are driven by the
  app forwarding `workflow:item-state` transitions from the same listener that
  wakes bound threads, and a fired automation enters through `StartItem` like
  any other start. The engine holds no timer, no trigger, and no automation
  identity — a run started by one is just a run whose `source` is `automation`.
