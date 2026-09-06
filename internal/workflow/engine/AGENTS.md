# internal/workflow/engine/

Persisted workflow item/phase FSM, direct run start, project-local resource semaphores, and
startup recovery.

Rules only. Rationale, rejected alternatives, and incidents live in
`docs/specs/workflows-system.md` (section numbers) and
`docs/specs/workflows-system-decisions.md` (D-numbers). Cite, do not restate.

## Ownership

- The command-loop goroutine owns all mutable state and every FSM transition. Runner callbacks
  enqueue commands and never mutate state.
- `teardown` is the only path that releases resource holders and the only caller of
  `Runner.Stop` (takeover uses `StopForTakeover`); `teardownUnit` is its per-unit half.
  Attempt-scoped state dies there: the fan, delivered guidance, and both loop-route knobs.
- There is no queued state: `StartItem` admits straight to `running`, and `waiting` is one FIFO
  of held starts, phase attempts and fan-out units alike. A held start renders the variable
  context its attempt row was persisted with. Do not rebuild it at release: the wait is
  unbounded and a rebuilt context contradicts the attempt's own `input_envelope`.
- SQLite is the recovery journal. Startup rebuilds running and parked items, parks interrupted
  running attempts rather than re-running them, and `sweepPersistedUnits` fails any unit row
  still claiming `running`.
- A run runs the definition it froze, and the designed channel for an edit is the call edge.
  `refreshDefinition` (`refresh.go`) repairs a parked run at a fresh phase entry and
  `AmendSeeds` (`amend.go`) edits a resting run's seeds; both refuse before the first write and
  neither reaches work that already happened.

## Never change chat to protect a workflow

A workflow fix must not change normal thread/chat behavior. Workflow sends use the standard
send path. Workflow-side protection is the bounded start plus honest failure reporting, never a
workflow-special branch in shared code.

## Resources

- Capacity comes from the live project profile at acquisition time. Acquisitions are sorted and
  all-or-nothing, and names never contend across projects. A profile source answering nil with
  no error is refused as broken, not read as unbounded. Every agent-driver phase implicitly
  acquires `provider:<provider>` on top of its declared resources, defaulting to
  `DefaultProviderCapacity`. A tool phase takes none, and a frozen agent phase with no provider
  is `ErrWiringFailed`.
- A fan-out phase takes no provider slot, because its units and join each take their own and
  the phase would deadlock against them at capacity 1. Its declared resources stay
  phase-scoped, acquired once per attempt. A unit's own `resources:` are per running unit, with
  provider capacity appended only for an agent unit. A call unit acquires nothing and one
  declaring resources is refused.

## Park reasons

- Runner start failures map by sentinel, never string matching: `ErrSetupFailed` to
  `setup-failed`, `ErrWiringFailed` to `wiring-error`, everything else to `agent-error`. A new
  runner failure mode picks a sentinel or adds one, and `OutcomeSetupFailure` is the twin for
  an asynchronously reported start failure. `agent-error` is a shared bucket no repair verb
  reaches. Park there only when nothing narrower fits.
- Every engine-diagnosed park writes its cause to the attempt's `park_cause` column (store
  v51), bounded at `MaxParkCauseBytes` (8 KiB). Never write it to `output_envelope`, which is
  the agent's artifact. It is absent where the reason is the account: an agent's resting
  envelope, and `taken-over` / `paused` / `interrupted` / `cancelled`.

## Resume

- **Resume continues and preserves; `--phase` starts over.** `ContinuableReason` is the whole
  rule: a bare `Resume` on `paused`, `interrupted`, `checkpoint`, `unit-failed`,
  `provider-retries-exhausted`, `provider-usage-limited`, or legacy `retries-exhausted`
  continues the parked attempt, and every other park re-enters the phase fresh, the parked
  phase included. Membership is one list (`continuableReasons`) read by both the predicates and
  the refusals that name the set, and the dispatch lives in `resume`, never in the app.
- **Pause then resume continues in place.** It never restarts the phase and never re-sends the
  original prompt: `runner.BuildContinuationPrompt` sends the delta.
- A parked attempt whose provider context is gone reconstructs the same round on a new thread
  with full persisted prompt context and feedback saying so. `ThreadExists` is a cheap target
  check only; the runner proves live or cold provider context before sending and reports
  unavailable context back rather than parking `agent-error`. A resume does not cascade into a
  parked descendant on a continuable reason: the call-child paths stay on the narrower
  `ResumableReason`.
- `provider-usage-limited` is the typed usage refusal (D75). Nothing here consults the recorded
  provider-account scope for admission, auto-resumes it, or interrupts running siblings; that
  scope is wake correlation only.
- A human gate refuses `Resume` in place but accepts one aimed at a different phase, the escape
  from a gate whose reject budget is spent. An over-budget reject is refused, never converted
  to a park, which would silently take the approve away too.
- Loop bounds are per fresh entry, derived from persisted attempt rows alone, and only entering
  a loop target from outside the cycle refills one. `loop-limit-exhausted` is therefore not
  continuable: only `--phase <id>` at an earlier phase refills the bound. Continuations are not
  entries and unclassifiable history counts as a continuation, so the derivation can only
  under-refill. The walk assumes insertion order, so every path re-entering a non-resident item
  seeds the engine clock from its persisted timestamps first.

## Pause, soft stop, cancel

- `PauseItem` is a root action over a whole tree and joins the teardown contract: it refuses a
  called run, walks the subtree deepest-first, and parks each live member
  `needs-human(paused)`. It differs from cancel in exactly one respect,
  `teardownRequest.retainCallChildren`. A persisted-`running` member the scheduler does not
  hold is an error, not a skip. Global pause starts no phase anywhere. In-flight turns finish
  and rest at the next boundary, still `running` with a held start, not `needs-human`. A soft
  stop is checked at call boundaries and nowhere else (D36). It is deliberately not checked in
  `startUnitCall`, which would strand a join's siblings. Cancel reaches a parked run under any
  park reason, taking live descendants down first and leaving the attempt row as it is. A
  `disposition` park is refused; those verbs settle it.

## Variable context

- `attemptRef` (`context.go`) is passed, never inferred. At a fresh entry `item.attempt` still
  holds the attempt being left, so inferring would drop a real prior attempt from its own
  history binding. Reserved reads bind from the run row after the seeds, so a seeds column
  cannot falsify them.
- **`history.<phase>` is composed and bounded here** (`history.go`); `def` owns the declaration
  rules and constants. A `completed` entry carries its envelope `outputs`; a non-completed one
  carries `envelopeStatus` / `reason` / `question` and never `outputs`. The window trims from
  the old end, then `def.MaxHistoryBytes` turns older entries into skeletons carrying an
  `elided` sentence, so a shortened window can never read as a phase that ran fewer times. An
  envelope that will not decode is a `wiring-error`, never a dropped entry.
- **`budget` is composed here too** (`bindBudget`) from the same `ResolveBudget` the check
  enforces with. A run with no ceiling, or a ledger that will not answer, leaves the name
  unbound (D44) and logs, because this context is also built at gate evaluation where there is
  no attempt left to park.

## Budgets

- Checked before every phase attempt, enforced per tree against the root (§12). Item overrides
  win over live profile defaults, and a child carries no budget of its own. The item parked is
  the one whose check tripped, not the root.
- `ResolveBudget` (`budget.go`) is the whole answer and takes a `BudgetSubject` rather than a
  `store.WorkItem`, so a narrower projection resolves the same ceiling. Every display surface
  resolves through it, so the number an operator reads and the number that parks the run cannot
  differ. A token ceiling ignores `Spend.Estimated` and `Spend.Unpriced`. A USD ceiling not
  already crossed by the priced lower bound cannot be judged, and `BudgetView.Unjudged` carries
  that as a field that only `checkBudget` turns into an error.

## Access

`def.Phase.EffectiveAccess()` is the single predicate behind enforcement and workspace
derivation: `write` to `provider.RuntimeFullAccess`, `read-only` or unset to
`provider.RuntimeReadOnly` (D22), so an unannotated phase can only be under-privileged. It is
stamped onto the phase thread row at creation, so it survives restarts and resumes. A writing
phase anywhere in the graph means every phase shares the worktree, so workspace is not a proxy
for access.

## Guidance and feedback: ordering, not atomicity

`Guide` (`guidance.go`, store v53) steers a run at a phase-entry boundary and nowhere else.
Feedback (`feedback.go`, store v64) is the same rule one level down, owed by the attempt rather
than the run. One contract covers both.

- **The two writes are deliberately not atomic, and the order is the contract.** The attempt
  row carrying the block is persisted first; the slot is cleared and the feedback stamped only
  once a prompt that renders the block has been dispatched to a live session. Everything
  between is a window where an attempt ending with no turn is a redelivery rather than a loss.
  Duplicate over loss, always.
- **The trigger is the send door, not the start result.** `AckFeedbackRendered` is the engine's
  one public ack, because a successful `Runner.Start` proves a session exists, not that a
  prompt reached it. The command loop settles only when the key names the live phase attempt,
  so a late, stale, or duplicate ack is a no-op.
- **What renders a block is `deliversGuidance`.** A `driver: tool` phase, a `shape: call`
  phase, and a fan-out whose every element is a command render no prompt, so they are not
  boundaries and the entries wait. A continuation is not a boundary either, which is why
  `phaseEntry` is a parameter rather than inferred. There is no mid-turn injection, by design.
- **A slot that will not decode parks once and heals** (`healGuidanceSlot`, the one heal every
  read goes through): raw bytes to the engine log, column emptied, delivery parks
  `wiring-error` so the bare resume that follows runs the phase. `Guide` heals and accepts,
  reporting the discard on `GuidanceState.Quarantined`.
- **Feedback redelivery collects every prior attempt of that phase still owing**, kept accurate
  by the marking rather than arithmetic. Only the note travels, bounded by
  `MaxRedeliveredFeedbackBytes` (32 KiB). **A repair in place is not an entry.** `RetryUnit`, a
  bare resume, and a join continuation relaunch elements without `enterPhase`, so `loadParked`
  restores the debt from the record.

## Events and logging

- **`workflow:phase-state` has one construction path.** `emitPhaseState` guarantees
  `PhaseEvent.OccurredAt` and `TestPhaseStateEventsHaveOneConstructionPath` fails on a second
  emitter. An emit beside a store write passes the time that write persisted, because
  `timestamp()` is monotonic and a defaulted stamp would put the event and the row in conflict.
  **Run-lifecycle logging goes through an injected sink** (`Config.Log`), never `log.Printf`.
  It is the trail around the durable record, never a substitute: anything a human must act on
  belongs on the run or the attempt.

## Loop routes and gate notify

- **A `notify:` gate announces once, and the run has already moved on.** `workflow:gate-notify`
  fires after the teardown that persisted the attempt, fire-and-forget, so a progress wake can
  never park, fail, or delay the run it reports on.
- **A loop route may make its re-entry warm, and it degrades rather than parks**
  (`loop_route.go`). A warm loop sends its full resolved prompt because it is a new logical
  round. Every reason a session is unavailable runs the round cold, stated in the attempt's
  feedback and the log, because `applyLoopRoute` runs after the deciding teardown and has
  nowhere to put an error but a park. **A loop route may also override the prompt of the round
  it creates.** What is persisted is the route coordinate, never the body, and it is consumed
  only when the armed route loops to the phase being entered, so a coordinate that outlived its
  entry is inert.

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
  unbounded. `parkFanOutSetup` writes the cause onto the attempt's
  `park_cause`, since no unit ran to author an envelope and the resolved width
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
  by a sibling's failure — their work is durable. `advanceFanOut` closes the one
  state that is neither: an attempt resting with its join already launched and
  not `done` has nothing left that could ever run, so it parks `unit-failed`
  rather than sitting `running` forever. Every legitimate repair is past it —
  reopening the join clears `joinStarted`, and a join that finished tears the
  attempt down instead of advancing it.
- The join is the attempt's final unit. It runs when every unit rests `done` or
  `dropped`, receives their persisted results (id, index, status, outputs,
  branch, worktree, thread) under the reserved `units` variable, and its
  envelope *is* the phase's envelope — so its outcome is the phase's outcome,
  gate and all. **A join that FAILS is a unit of the attempt failing**
  (`phaseFailureReason`): it parks `unit-failed`, not `agent-error`, because the
  wave behind it is finished work — for a campaign, whole child runs — and
  `agent-error` is a reason no repair verb reaches, which left re-entering the
  phase (and re-running every one of those children) as the only move a human
  had. `fanOutRun.blocked` still reads the WORK units only: a failed join blocks
  no launch, and counting it there would make its own repair refuse itself,
  since `continueFanOutJoin` asks that question before it reopens the join.
  Those results are what
  the STORE knows; the per-unit git state a merge join actually decides on
  (`commitsAhead`, `dirty`) is added by the app runner on the way in
  (`internal/workflowhost/units.go` `enrichJoinUnits`), because this package is git-free
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
  double-bump. Reopening the JOIN also clears `joinStarted` there — the flag
  means "the join of this attempt has been launched", and leaving it set would
  leave an attempt with a pending join nothing ever starts.
- `RetryUnit` / `RetryFailedUnits` / `DropUnit` / `TakeOverUnit` repair an
  attempt rather than replacing it: the phase attempt row is reopened (`ReopenWorkItemPhase`) so
  finished units keep their results, and the run only returns to `running` when
  no unit is left blocking it. A unit that cannot *start* is not a unit failure
  — it parks the attempt under the same sentinel-mapped reason a single-shape
  phase would, because nothing runnable was ever produced.
- **The join is a retry target and never a drop target.** `repairable` is the
  status rule every repair shares, and it says nothing about kind: retrying a
  failed join re-runs it over the results the wave already produced, which is
  the same continuation `Answer` takes. `droppable` adds the one refusal —
  accepting the join's absence would leave nothing to consolidate the units and
  no envelope for the phase, so there would be no attempt left to resume. The
  reported failed set follows the same rule: `workflowFailedUnits`
  (`app_workflow_unit_actions.go`) lists the CURRENT attempt's failed units,
  join included, so `run status`, the wake's references, and the verbs that
  repair them cannot name different sets.
- `RetryFailedUnits` is `RetryUnit` over every unit resting `failed`, as ONE
  command (D33). It exists because the failure it repairs usually has one cause
  hitting many units at once — a provider usage limit stopping most of a wide
  fan-out — and it is one command rather than N submitted retries because the
  loop serializes commands but not the gaps between them: a half-repaired
  attempt must not be observable by a concurrent drop or single retry. It
  collects the failed set — from `fan.all()`, join last and included, because a
  failed join is often the only failed unit there is — before its first write,
  so "nothing was failed" is a
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

## Call phases and call units

- **Two call edges, one implementation, two halves.** `planCall` decides whether the call can
  be made (ancestry walk, both depth bounds, live `ResolveCall`, argument evaluation);
  `invokeCall` makes it. Planning writes nothing, so a refusal leaves a `pending` row and a
  call that cannot be made is not a unit failure. `callEdge` is how `max_depth` is counted, so
  sibling units never spend each other's budget.
- A call phase runs no work of its own: it resolves its static target at call time (fresh
  `ResolveCall` per invocation, §8 scoping), evaluates `args:` against the caller's context,
  and starts the linked child, then rests holding no runner, resources, or provider capacity.
  **An unresolved argument is judged by the child's declared inputs**, which is why the child
  is resolved first: one seeding an `optional:` input is omitted (D45), one seeding a required
  input or naming no child input is a `wiring-error` park. The parent attempt row is persisted
  with its evaluated args before the child exists, so the only gap a crash can open is an
  attempt with no child, which `recoverCall` re-invokes in place without a new attempt row.
- The call phase's envelope is synthesized: a `done` child yields its declared outputs, a
  `failed` child fails the parent under `child-failed`, a `cancelled` child parks the parent
  `agent-error`, and a child parked `needs-human` is not terminal. A declared output the child
  did not produce is required-fails, `optional:`-omitted (D45).
- Child-to-parent notification never runs inside the child's own transition; it appends to the
  loop's `deferred` queue, so the parent's completion is a serialized step.
- Depth is bounded twice: the author's `max_depth` on the edge, and the engine's absolute
  `MaxCallDepth` (256), which exists because children resolve live. Workspace flows down and is
  never provisioned by a child, so the tree has exactly one (§9).
- Teardown is tree-aware (`call_cancel.go`): leaving a call phase for any reason brings the
  live descendant subtree down deepest-first. `cancelCallChildren` reads a fan-out's children
  from the store, not from `item.fan`, because the crash-rebuild path has no in-memory unit
  state and that is where a stranded grandchild survives.
- A call unit additionally: rests `running` with no runner flags (`resting()` counts that as
  resting for a call unit alone, the discriminator every recovery path keys on); gets no
  stamped workspace, so the child runs in the unit's own sub-worktree (§9); maps `needs-human`
  to still waiting and `cancelled` to unit `failed`; and refuses `TakeOverUnit`.

## Boundaries

- Provider and app/channel implementations live behind `Runner` and `Emitter`.
- Workflow resolution and project-profile loading live behind their narrow
  sources. The frozen `Snapshot` pins definitions, never profile capacities.
- No timers, watchdogs, retry backoff, worktree setup, or transport/app wiring
  belongs in this package. Reliability timers and sub-attempt retries are
  runner-owned; the engine only checks phase-boundary budgets and maps outcomes.
  - **One carve-out, and it is a reply deadline rather than a policy timer**:
    `runnerStartReplyBudget` (`reply_budget.go`) bounds how long an API CALLER
    already inside `request` waits for the runner starts its own command
    produced. It schedules nothing, retries nothing, and cannot change a run's
    state — expiry answers the caller SUCCESS and leaves the start running — so
    it is the wait's bound, not the work's. A timer that would decide anything
    about the run (a start deadline, a backoff, a scheduled resume) still lives
    app-side; `app_workflow_start_watchdog.go` is the start's own bound.
- Persisting the pause flag and emitting it to the frontend is app wiring. The
  engine owns the live flag and the `workflow:engine-state` payload shape.
- Waking a bound thread is app wiring too. The engine's contribution is two
  events and nothing else: the `workflow:item-state` transition, and
  `workflow:gate-notify` for a `notify:` route the run continued through.
  `internal/workflowapp/events.go` decides from those who is told and how (a
  wake into the run's bound thread, an OS notification when it needs a human, a
  descendant's park or progress announced at its root), and whether the message
  is one the thread has already been told. Discard — worktree removal and branch deletion — is entirely
  app-side; the engine only cancels the tree members the app hands it, which
  since `cancelParked` means the parked ones too (`workflowDiscardStops`): a
  run left needing a human after its checkout was removed and its branch
  deleted has no remaining move that can succeed.
- Automations are app-fed and engine-unaware. `internal/workflow/scheduler`
  never imports this package: its internal-event triggers are driven by the
  app forwarding `workflow:item-state` transitions from the same listener that
  wakes bound threads, and a fired automation enters through `StartItem` like
  any other start. The engine holds no timer, no trigger, and no automation
  identity — a run started by one is just a run whose `source` is `automation`.

## Host maintenance admission

`Config.BeginWork` is an optional host admission hook. Both creation of a new
run and every transition back to `running` acquire it before the durable state
write. The hook may wait for host maintenance but must not call this actor.
Existing running rows cover the spaces between phases; no second running-work
counter or user-pause mutation is needed. A new path that writes running
ownership must retain this boundary.
