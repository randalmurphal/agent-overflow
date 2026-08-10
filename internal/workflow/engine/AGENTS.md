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
  survived to be torn down. It is also where the ATTEMPT-scoped state dies: the
  fan, the delivered guidance, and both loop-route knobs are cleared there, so a
  per-round value cannot reach a round that did not earn it no matter which exit
  path the attempt took.
- There is no queued state. `StartItem` admits an item straight to `running`
  and enters its first phase; back-pressure is a *phase* waiting on resource
  capacity, never an item waiting in line. `waiting` is one FIFO list of held
  starts — phase attempts and fan-out units alike — so freed capacity goes to
  the longest-waiting piece of work regardless of which kind it is. A held start
  carries the variable context its attempt row was PERSISTED with and renders
  exactly that on release: the wait is unbounded (an unpause an hour later, a
  wide wave's capacity), and a context rebuilt at release handed the model a
  `budget` block the attempt's own `input_envelope` contradicted.
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
- A unit's *own* `resources:` are the other half, and they are per running unit.
  `unitResources` hands admission everything a working unit declared — agent and
  tool driver alike, with provider capacity appended only for an agent unit
  because only it runs a turn — so a gate-check command unit inside a review
  fan-out claims its `container-slot` through the same all-or-nothing
  acquisition, the same live profile, and the same FIFO a phase does. A call
  unit still acquires nothing; one that *declares* resources is refused rather
  than ignored, because validation rejects that declaration statically and a
  frozen definition carrying it cannot produce runnable work.
- Runner start failures are mapped to typed park reasons by sentinel, never by
  string matching: `ErrSetupFailed` → `setup-failed` (workspace provisioning,
  setup hooks, secret resolution, a process that would not start),
  `ErrWiringFailed` → `wiring-error` (the frozen definition and the live project
  profile cannot produce runnable work — an unbound check/command, a failed
  argv interpolation), everything else → `agent-error`. These are the same
  reasons the engine parks its own equivalents under — an unroutable gate or a
  phase missing from the snapshot is `wiring-error` — so a run's park reason
  does not depend on which side of the `Runner` boundary noticed. A failed
  resource acquisition is classified by the same sentinel through
  `acquisitionParkReason`: a live profile that cannot be read or a resource the
  project never sized is `setup-failed`, while a frozen definition that cannot
  describe an acquisition at all — an agent element with no provider, a call
  unit claiming capacity for work it does not run — carries `ErrWiringFailed`
  and parks `wiring-error`. A new runner failure mode picks one of these
  sentinels or adds one here; it does not fall through to `agent-error` by
  accident.
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
- **Resume continues and preserves; `--phase` starts over.** One rule across
  every surface. `ContinuableReason` is the whole of it: a bare `Resume`
  (`targetPhase == ""`) on `paused`, `interrupted`, `checkpoint`, `unit-failed`,
  or `retries-exhausted` routes into `resumeItem` and continues the parked
  attempt, and every other park re-enters the phase with a fresh attempt. The
  membership is one list (`continuableReasons`, derived from `resumableReasons`)
  that both predicates and the refusal naming them read, so a new member cannot
  join the rule and miss the message. The dispatch lives
  in `resume` itself rather than in the app, so no entry point can reach the
  fresh path for a park whose finished units — whole called runs among them — it
  would silently redo; `enterPhaseFresh` is the other half, and `resumeItem`
  calls it directly for the two gaps where there is nothing to continue.
  Naming a phase is ALWAYS the fresh entry, including when it names the parked
  phase itself: that is how a human deliberately asks for re-expansion. Whether
  it also refills a loop budget is the target's position, not the verb: only
  entering the loop's target from OUTSIDE the cycle refills (`freshLoopEntry`
  answers false when the previous row's phase equals the phase being entered),
  so `--phase <the parked phase>` re-expands without giving any bound back,
  and naming an earlier phase is the refill. The human-gate
  guard is checked before the dispatch and is unaffected — a `gate` park is not
  continuable, so bare resume still reaches its refusal.
- **A run runs the definition it froze, and a fresh entry may re-read it
  (`refresh.go`).** The freeze is the default: the snapshot inlines every prompt
  file, and the designed channel for an edit is the call edge, which resolves
  its target fresh per invocation — so a campaign picks an edit up at its next
  wave. `Resume`'s and `RerunFailed`'s `refreshDefinition` is the channel a run
  parked for OPERATOR REPAIR otherwise has none of: it re-resolves through
  `e.definitions.Resolve`, re-freezes (`freezeSnapshot`, the same write the
  setup-failed entry uses), and takes the verb from there, so every later
  attempt and a crash rebuild render the edited definition too. It is offered
  at a FRESH PHASE ENTRY only — a bare resume of a `ContinuableReason` park
  continues an attempt whose units, and the runs those units called, were
  launched under the frozen definition, and is refused with `--phase <id>`
  named as the deliberate discard. `resolveDefinition` decides every other
  refusal before the first write, so a refused refresh leaves the run record
  untouched: a definition that will not resolve or has no phases, one that no
  longer declares the phase being entered, and one whose `WorkspaceNeed` the run
  cannot satisfy (`checkRefreshedWorkspace`). That last one is asymmetric
  because provisioning is: a ROOT run under `project-root-read-only` has written
  nothing and provisions lazily, so it may ADOPT a newly-writing definition and
  cut the worktree at this entry exactly as its first phase would have, while a
  run that already provisioned one may not give it back (its work lives there,
  and the remaining phases would run in the project root) and a CALLED run may
  not acquire one (§9: it executes in the workspace its root froze). The run
  start is not re-stamped — a wall-clock budget is measured against it — and the
  refresh needs no column of its own: the re-frozen snapshot IS the durable
  evidence, with the engine log and the next attempt's feedback note
  (`definitionRefreshNote`) saying it happened.
- **A resting run's SEEDS may be amended, and the amendment says when it is
  read (`amend.go`).** Seeds are not part of the snapshot: `variableContext`
  rebuilds them from the run ROW at every phase entry, so the column is live
  data a run re-reads rather than a definition it froze — which is the whole
  mechanism `AmendSeeds` rests on, and why its boundary rule reads differently
  from `--refresh-def`'s. Every refusal is decided before the write, so a
  rejected amendment leaves the record byte-identical: a `running` run is
  refused (an attempt reads its seeds when it starts, and a write under one
  would leave a single attempt rendering two sets of inputs), a terminal run is
  refused (nothing is left to read them), a `disposition` park is refused to the
  verbs that settle it, and a key the frozen workflow does not declare is
  refused naming the ones it does. Values are validated by `def.ValidateInput`
  — the per-key half of the validator a start seed passes through, applied per
  NAME rather than over the whole object so an amendment is never refused
  because of an unrelated pre-existing gap in a manually started run's seeds.
  What an amendment can never do is reach work that already happened: a
  completed phase keeps its outputs, a fan-out attempt keeps the variables its
  units were expanded with (`restoreFanOut` reads them back from the persisted
  input envelope), and a called run keeps the seeds its caller's arguments
  evaluated to. `SeedEffect` states which of those the parked run is in —
  `fresh-phase-entry` when a bare resume repairs the parked attempt IN PLACE (a
  fan-out or a call phase), `next-attempt` otherwise — so the operator is told
  when the value will be read instead of inferring it from the verb they resume
  with. Like the refresh, it needs no column of its own: the seeds column IS
  the durable evidence, with `LogEventSeedAmend` saying it happened. A CALLED
  run is amendable for exactly the reason its seeds are its own row; naming the
  caller is the app's to add, not this package's.
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
  itself instead of as an `agent-error` from a failed runner start. A
  `unit-failed` park joins them through the same `resumeFanOutAttempt`: the
  units blocking it are reopened, a failed join is continued on its thread, and
  everything that finished keeps its result. What it does NOT join is the
  cascade into a parked DESCENDANT — `resumeCallPhase` and
  `resumeUnitCallChildren` stay on `ResumableReason`, because a child resting on
  a failed unit needs a human's judgment about that unit, not a silent retry
  ridden in on its parent's resume.
- **`retries-exhausted` joins them, and it is the one member whose turn DIED.**
  The three above stopped a run — a human's pause, a process death, a requested
  checkpoint — and `unit-failed` rests on work that finished. This one rests on
  an attempt the runner gave up on: a provider API failure that outlasted the
  transient-retry layer's backoffs (`app_workflow_observe.go` →
  `OutcomeTransientExhausted`). The provider tolerance that makes continuing
  safe is already proven on both sides — the transient layer re-sends into that
  SAME live session between backoffs (`app_workflow_reliability.go` `timerFired`
  → `sendIfActive`), which is the live-session case, and a `paused` or
  `interrupted` park continues a session whose process died, which is the
  killed-process case. What a fresh entry bought was nothing: it discarded the
  in-context state of a turn that may have run for many minutes. The same
  `ThreadExists` fallback covers a session that is genuinely gone.
  - **The reason is shared, and the continuation does not repair the other
    half.** A spent loop or reject bound parks under it too
    (`def.DecisionRetriesExhausted` → `finishDecision`), and no continuation
    refills a bound. Neither did the fresh entry a bare resume used to take:
    re-entering the phase that parked is not an entry from OUTSIDE its cycle, so
    `freshLoopEntry` never refilled it either — both shapes re-park, and the
    continuation is the cheaper of the two (a fan-out continues its join instead
    of re-expanding the wave; a call phase re-links its child instead of
    starting a second one). Only `--phase <id>` naming an EARLIER phase — the
    loop's target, entered from outside the cycle — gives the bound back, which
    is why every surface names it beside the continuation and why `resumeNote`
    is worded for both causes rather than naming one.
  - **A non-transient execution failure is NOT here.** It parks `agent-error`
    (`phaseFailureReason`, `fsm.go`) — spec §12's "not on the allowlist parks
    immediately" — and that reason is a shared bucket: envelope-validation
    exhaustion, a turn that completed with an error, a cancelled call child, and
    every runner start failure that matches no sentinel land in it. Continuing
    it wholesale would continue four unrelated things, so it stays fresh.
- **Cancel reaches a parked run, under any park reason.** The scheduler evicts
  an item the moment it leaves `running`, so `cancel` dispatches on residency:
  a live run goes through `teardown`, and a parked one through `cancelParked`
  — `loadParked`, the tree's live descendants down through the same
  `cancelCallChildren`, then the `needs-human → cancelled` FSM edge. That edge
  exists for exactly this: a run resting at a gate a human decides never to
  approve would otherwise be immortal short of resuming it into work nobody
  wants. A parked run holds no runner, no resources, and no held start, so the
  transition IS its teardown, and the attempt row it parked on is left exactly
  as it is — that row is the only account of why a human was ever asked, and
  cancel does not rewrite history it did not make. The transition is an
  ordinary one, so a cancelled descendant settles the call edge waiting on it
  through `settleCallChild` like any other terminal child (a call phase parks
  `agent-error`, a call unit fails). A `disposition` park is refused: it is a
  DONE run waiting on a merge/PR/discard, and the disposition verbs settle it.
- A human gate refuses `Resume` **in place** — that decision belongs to
  `ResolveHumanGate` — but accepts one aimed at a DIFFERENT phase. Naming
  another phase is not a way to take the gate's decision; it is the human
  abandoning the gate to redo earlier work, and it is the escape from a gate
  whose reject budget is spent, because it enters that phase from outside every
  cycle through it and so refills their bounds.
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
  source (`gate.loop-ancestor`). The bound itself may be seeded (`max:` naming a
  variable): `def` resolves it at evaluation and the persisted decision carries
  the resolved integer, so a derived count and a recorded budget are always
  comparing the same numbers. `resolveHumanGate` resolves a `reject.max` the
  same way against the attempt's variables — a bound that will not resolve
  fails the human's action rather than reading as an exhausted budget.
- **A phase's variable context is built FOR one attempt, and that attempt is
  passed, never inferred.** `attemptRef` (`context.go`) is the current attempt
  and its envelope. It cannot be read off the runtimeItem because the two
  disagree exactly where it matters: at a fresh phase entry `item.attempt`
  still holds the attempt the run is LEAVING, and the attempt being entered has
  no row yet — so inferring it would exclude a real prior attempt of that phase
  from its own history binding, which is the round a re-entered phase most
  needs to see. The zero value means "no current attempt" and matches no row.
- **The reserved reads are bound from the run ROW, after the seeds.**
  `def.CallDepthVariable` carries `store.WorkItem.CallDepth` — 0 for a directly
  started run, one more per call edge below it — so a recursive campaign renders
  the wave ordinal the engine knows rather than one a model incremented through
  its own `args:` and let drift. Fan-out units and the join inherit the
  attempt's context, so a lane reads the same number its phase does. Binding
  after the seeds is what makes the value unfalsifiable: `def` refuses the name
  at every declaration site, but a seeds column can still carry it (a caller's
  arguments are evaluated, not validated against this list), and the engine's
  answer has to win.
- **`history.<phase>` is composed here and bounded here** (`history.go`). Only
  the phase being run has its declarations read: the binding costs one decode
  per prior attempt, and materializing every phase's history on every gate
  evaluation would be paid by the runs that reference none of it. It is bound
  LAST, for the reason `units` is — a seed of the same name cannot replace the
  history a phase declared. Fan-out units and the join inherit the attempt's
  context, so a unit prompt reads the same binding its phase declared.
  - An entry is `{attempt, status}` — the persisted row's status, from the
    `work_item_phases` enum — plus, for a `completed` attempt, its envelope's
    `outputs`. A non-completed one (parked, failed, cancelled, superseded)
    carries `envelopeStatus`, `reason`, and `question` from the envelope it
    rested with, and **never `outputs`**: nothing ratified any. Non-completed
    attempts appearing at all is a deliberate carve-out from "only completed
    attempts feed variables", scoped to this binding — a round that parked with
    a question is part of why a loop is on its fourth lap, and a series that
    skipped it would read as a shorter, cleaner history than the one that
    happened. The ordinary `<phase>.<output>` reference is untouched.
  - The window trims from the OLD end. `def.MaxHistoryBytes` then bounds the
    whole series: entries are measured newest-first and, once the budget is
    reached, every older one becomes a skeleton carrying an `elided` sentence
    instead of its content — the attempt still appears, so a shortened window
    can never be read as a phase that ran fewer times. The newest prior attempt
    is always carried whole, because one envelope can fill the budget by itself
    and eliding the round the next attempt is reacting to would be worse than
    carrying no history at all.
  - An envelope is decoded only where its content is carried, and one that will
    not decode is then an ERROR (the run parks `wiring-error`) rather than a
    dropped entry — the same rule `addOutputs` applies, and the column is
    CHECK-constrained JSON this engine wrote.
  - The series lands in the attempt's persisted `input_envelope` like every
    other variable, so a phase's rows grow with its lap count. That is what the
    byte budget bounds, and it is the right record: the input envelope is the
    account of what the attempt actually ran with.
- **`budget` is composed here too** (`bindBudget` / `renderBudgetBinding` in
  `context.go`), from the same `ResolveBudget` the check enforces with, at
  attempt start. An element can then say what it is nearly out of instead of
  discovering the ceiling by being parked at it. The entry is
  `{kind, ceiling, spent, remaining, estimated}` rendered in the ceiling's own
  units — int64 tokens, float64 dollars, `time.Duration` strings for a wall
  clock — because a model reasoning about "25" must not have to guess whether
  that is dollars or tokens. `estimated` is the one caveat that changes what the
  number means and is always false for the two exact kinds.
  - **A run with no ceiling leaves the name UNBOUND**, which is why `def`
    declares it `optional:` — `Interpolate` then renders `(not provided)` per
    D44 rather than a fabricated zero ceiling that would read as "you have
    nothing left". Most runs are that run.
  - **A ledger that will not answer leaves it unbound too, and `bindBudget`
    returns nothing.** The binding is one optional field of a variable context,
    and that context is built at GATE EVALUATION as well as at phase entry —
    where there is no attempt left to park. Failing it there marked a phase that
    had already COMPLETED as parked, threw its gate advance away, and left no
    verb that could repair the run. The read failure is stated through the
    `LogSink` (`LogEventBudgetUnread`) instead. Enforcement is the loud half and
    stays loud: `checkBudget` / `enforceBudget` still refuse to START a phase
    under a ceiling whose spend cannot be read.
  - It is prompt-surface only, exactly as `history.<phase>` is: `def` refuses it
    in a gate predicate naming the alternative (have the phase decide and
    declare that as an output), and it is not writable. Routing on spend is
    arithmetic in a predicate, which the anti-change list refuses.
- **An over-budget reject is refused, never converted.** A human gate declares
  two verbs; parking the run `retries-exhausted` because the reject route's
  bound is spent would silently take the approve away as well — a third outcome
  the gate never declared. `resolveHumanGate` therefore fails the human's
  action and leaves the run resting `needs-human(gate)` with its trace and
  attempt untouched, the same shape a step gate's refused reject takes, and the
  message names every option that is still live: approve, `run resume --phase
  <id>` (a fresh entry into the loop's target refills its bound), or cancel.
  All three actually work, which is why the resume guard admits a targeted
  resume and why cancel reaches a parked run at all.
- Done-item disposition parking is an item lifecycle action in
  `item_actions.go` and stays serialized through the command loop.
- Per-item budgets are checked before every phase attempt. Item overrides win
  over live profile defaults; token/USD spend comes through `SpendSource`, and
  wall clock uses the engine clock against the persisted item start time.
  `ResolveBudget` (`budget.go`) is the whole answer — which ceiling is in force,
  the tree's spend against it, and whether it is crossed — and `checkBudget` is
  it with only the last field read. Everything that DISPLAYS a budget (the run's
  `budget=` line on `agent-overflow run status` / `run inspect`, the reserved
  `budget` prompt binding) resolves through the same call, so the number an
  operator reads and the number that parks the run cannot differ. A run under no
  ceiling never reaches the ledger at all: `ResolveBudget` answers nil before
  `SpendSource` is asked.
- **`Spend` says how much of itself it is sure of.** `Estimated` marks a total
  partly composed from the app's rate table because a provider reported tokens
  and no cost (every Codex row), and `Unpriced` counts the rows even that could
  not price. What the two MEAN is the ceiling's to decide, and the kinds decide
  differently: a token ceiling ignores both — a token count is exact whatever
  the rate table knows — while a USD ceiling the tree has NOT obviously crossed
  cannot be judged at all, because the missing rows can only move the total
  upward and a ceiling nobody can evaluate must not read as headroom. A breach
  already proven by the priced lower bound needs no such caveat: the run parks
  for its budget, which is the truthful reason. The refusal used to live in the
  spend source and so failed a TOKEN budget too, parking runs `setup-failed` on
  the first turn of a model the rate table had not learned.
- **`BudgetView.Unjudged` is a field, not an error, and `checkBudget` is the one
  place it becomes one.** Enforcement must refuse an unjudgeable ceiling; a READ
  must still answer. Returning an error from `ResolveBudget` took `run status`
  and the `{{budget}}` binding away from the operator over exactly the fact they
  needed to see — and would have parked a phase entry (`bindBudget`) for a run
  comfortably inside its ceiling. The read surfaces render the priced lower
  bound, flag it, and name the unpriced count.
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
- **Every ENGINE-diagnosed park persists its cause.** `teardownRequest.cause`
  is the error the engine parked on, and `teardown` writes it to the attempt's
  `park_cause` column (`store`, v51) — bounded at `MaxParkCauseBytes` with the
  truncation stated. It is deliberately NOT the `output_envelope`: the envelope
  is the AGENT's artifact, and a phase that parked because a worktree would not
  cut ran no turn, so engine prose written there is read as a model's terminal
  outcome by the history binding, the crash rebuild, and the wake alike. The
  cause is set at every site where a Go error is the reason — setup failures,
  wiring errors, failed acquisitions, budget breaches, unknown outcomes,
  unroutable gates, snapshot decode failures, refused calls and refused
  expansions, the soft-stop boundary. It is deliberately absent where the
  reason IS the account: an agent's own resting envelope (`question`, `stuck`,
  `stalled`, an execution failure, a gate decision with its trace), and the
  human-driven or self-describing parks (`taken-over`, `paused`,
  `interrupted`, `cancelled`). `ReopenWorkItemPhase` clears it, so a repaired
  attempt does not carry a park that is being undone.
- **A park rests on an attempt row whenever a phase is known.** `teardown`
  completes the current attempt when there is one; when there is not — every
  pre-create failure in `enterPhase`, and the budget check that runs before
  it — `parkOnNewAttempt` creates one carrying the cause. This is why
  `beginRun` names `workflow.Phases[0].ID` BEFORE it freezes the snapshot: a
  run that could not start its first phase parks on that phase rather than on
  nothing. It costs one thing, stated where it happens: the loop-budget
  derivation now sees a parked attempt where it previously saw the advance, so
  a resume through it under-refills rather than refills — the direction that
  derivation is already required to err in. The one remaining rowless park is
  a run whose workflow never resolved at all: it has no phase to rest on, its
  cause reaches the caller and the engine log, and zero attempt rows is then
  the honest record of a run that never entered a phase.
- **A `notify:` gate announces once, and the run has already moved on.**
  `noteGateNotify` (`fsm.go`) emits `workflow:gate-notify` (`NotifyEvent`)
  when a decision the run CONTINUES through carries `def.RouteDecision.Notify`
  — advance and loop, the only kinds `decisionForRoute` sets it for. Three
  things are deliberate. It runs AFTER the teardown that persisted the attempt
  and its gate trace, so the app resolving the message reads a record that
  already says what the gate decided, and a failed teardown announces nothing.
  It carries the phase and attempt the gate CONSUMED, captured before
  `finishDecision` reassigns them, because that is the attempt whose outputs
  the message is about. And it is fire-and-forget across the ordinary `Emit`
  seam with no result: a progress wake can never park, fail, or delay the run
  it reports on. Step mode is the one runtime case where a decorated advance
  announces nothing — the run parked at that gate instead of continuing, and
  the park is its own surface.
- **A loop route may make its re-entry warm, and it degrades rather than parks
  (`loop_route.go`).** `session: continue` sets `item.priorThreadID` from the
  NEWEST attempt of the loop TARGET that holds a thread which still exists — the
  same field an `Answer`, a resume-in-place, and a takeover finalize set, so
  there is one same-session mechanism and a warm loop round is the same thing to
  the runner as every other continuation. It cannot fail: every reason a session
  is unavailable (a deleted thread, a phase that never ran, a store read that
  would not answer) runs the round COLD with the fact stated in the attempt's
  feedback and in `LogEventLoopSession`, because `applyLoopRoute` runs after the
  teardown that persisted the deciding attempt and has nowhere to put an error
  but a park — which would turn an unavailable optimisation into an outage. A
  target that starts no session of its own — a `shape: call` phase — is the same
  degradation decided one step earlier: the arming is refused before the field is
  set, because an id a call phase's entry cannot consume is an id the phase after
  it would. `enterPhase` clears it there regardless, so the field cannot outlive
  an entry that has no use for it however it came to be set. The
  mode needs no column: the two attempt rows share a thread id, which is what
  `agent-overflow run status` renders as `session=continued`.
- **A loop route may also override the prompt of the round it creates.** What is
  persisted is the route COORDINATE (`PhaseInput.PromptRoute`), never the body:
  the snapshot already holds every route's inlined prompt, so storing the text
  again would double a run's largest record. `promptBody` is the single point
  where the substitution happens — on the `RunRequest.Phase` the runner is
  handed — so no consumer can render a phase's body while an override was in
  force. It belongs to exactly one entry — the entry into that route's own loop
  TARGET — and `consumePromptRoute` is where that is decided: a fresh entry
  consumes the arming only when the armed route loops to the phase being entered,
  so a coordinate that outlived its entry is INERT rather than a narrower question
  asked of a phase nobody asked it of. Checking the target rather than trusting
  the field is what makes the override phase-scoped by construction, and it is
  what a park landing between the deciding gate and the target's entry needs:
  every recovery path re-arms from the persisted DECISION (`recoverDecision`,
  `recoverPersistedHumanDecision`, step mode's resolve), so `loadParked` restores
  only the route the parked ATTEMPT ran with (`item.promptRoute`, which a
  continuation re-renders) and never the arming itself. A coordinate that
  no longer resolves after a `--refresh-def` falls back to the phase's body
  rather than failing the attempt.
- **Both knobs are read off the DECISION, not off the route alone.**
  `decisionForRoute` sets `Session` only for a loop, and `applyLoopRoute`
  additionally refuses a route whose `Loop` is empty — which is what a human
  gate's reject carries, since `resolveHumanGate` synthesizes a loop decision
  whose index points at the `human:` route it came from. Neither knob is
  authorable there, and reading one off that route would apply a declaration
  validation refuses.
- **A run may be steered without being parked, at a phase-entry boundary and
  nowhere else (`guidance.go`).** `Guide` appends one entry to the run's
  pending-guidance slot (`store` v53); the next FRESH phase entry of that run
  delivers every pending entry into the attempt it creates — on
  `RunRequest.Guidance`, in the persisted `PhaseInput`, and as one feedback note
  saying where the block came from — and clears the slot once a provider session
  that renders them has actually started. It is the thread→run
  direction of `notify:`, and it exists because steering a free-running campaign
  otherwise cost a pause, an edit, and a resume.
  - **There is no mid-turn injection, by design.** Correcting a turn already in
    flight is an explicitly deferred non-feature (root `AGENTS.md`), it maps to
    no provider wire event on either CLI, and a prompt arriving halfway through
    a turn would reach the model as a second instruction with no contract saying
    which one wins. A run that is mid-phase keeps the guidance pending.
  - **A continuation is not a boundary.** `phaseEntry` is a parameter rather
    than something inferred, because nothing on the item distinguishes the two —
    a warm loop round and an `Answer` continuation both carry a prior thread id.
    An answered question, a takeover finalize, and a bare resume of a
    continuable park all continue a round the operator was already steering, and
    a block arriving mid-round would be a second instruction to a turn that has
    already read the first.
  - **A phase that renders no prompt is not a boundary either**
    (`deliversGuidance`): a `driver: tool` phase, a `shape: call` phase, and a
    fan-out whose every element is a command have no turn to read the block, so
    clearing the slot there would be the silent loss the ordering rule exists to
    prevent. The entries wait for a phase somebody can read them in.
  - **The two writes are deliberately NOT atomic, and the order is the whole
    contract.** The attempt row carrying the entries is persisted first, and the
    slot is cleared not when that row lands but when a provider session that
    RENDERS the block has started (`ackGuidance`, off the runner's own start
    result). Everything between those two points is a window in which the entries
    are still pending, so every way an attempt can end without a turn — a pause
    taking its held start down, a failed acquisition parking it, a crash — is a
    REDELIVERY rather than a loss. The reverse order, a single transaction, and a
    clear at the row write all convert that window into a lost instruction: a row
    would exist, the slot would be empty, and nothing would ever render what the
    operator wrote. Between telling a run something twice and never telling it at
    all, twice is the answer — the entry carries the time it was left, so a
    second delivery reads as what it is.
  - **The clear removes the delivered entries, never the column.** The slot is
    live during that window: an operator who guided the run again inside it left
    an entry no attempt has read, and emptying the column wholesale would drop
    exactly the instruction this ordering exists to protect. Removal is by value
    and therefore idempotent — a wave's second agent unit finds nothing left to
    remove — and a failed clear leaves the entries pending (a redelivery, the safe
    direction) with the fact emitted and logged rather than swallowed.
  - **The author is engine-stamped and the slot is bounded.** `GuidanceDraft.By`
    comes from the app's authenticated caller, never from the text, because "a
    human said this" is the one claim in a quoted block worth forging. A
    terminal run and a `disposition` park are refused (no phase entry is left),
    a `running` run is the target the verb exists for, and an entry past
    `MaxGuidanceEntries` is refused rather than rotating one out: a run that has
    not reached a boundary cannot have read what is already waiting, so a ninth
    entry would bury the eight it joins.
- **Run-lifecycle logging goes through an injected sink, not `log.Printf`**
  (`log.go`). `Config.Log` is a `LogSink` taking a typed `LogEvent` (park, cancel, resume,
  definition-refresh, rebuild, capacity); the app wires it to the NDJSON
  `engine-YYYY-MM-DD.ndjson` stream in `internal/logging`, and a nil sink falls
  back to the standard logger so a bare engine still says what it did. The
  engine does not import the app's logging package: the sink is the seam, and
  `LogEvent` is what this package is willing to say about itself. The log is
  the diagnostic trail AROUND the durable record, never a substitute for it —
  anything a human must act on belongs on the run or the attempt. What it says
  is what HAPPENED, not what was asked for: a resume is recorded by the BRANCH
  that takes it (`noteResume`), never from the request. `ContinuableReason` only
  routes a bare resume into the continuation path, and that path still re-enters
  fresh wherever there is nothing to continue — a `driver: tool` phase that held
  no session, a thread deleted under an agent phase, a call whose child was never
  created, a run that froze no definition — so a note derived from the verb
  called every one of those a continuation. Each branch emits exactly once and
  before doing its work, which keeps "a resume that fails partway still says what
  it was doing" true per branch; only a resume that fails while DECIDING logs
  nothing, and it changed nothing either. `ResumeItem` and the cascades into
  parked descendants record themselves the same way for free.

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

## Call phases

- **Two call edges share one implementation, in two halves.** `planCall` is
  everything that decides whether the call can be made — the ancestry walk, both
  depth bounds, the live `ResolveCall`, and the argument evaluation — and
  `invokeCall` is everything that makes it: the workspace decision and the
  linkage that makes the child recoverable. Both a `shape: call` phase
  (`startCall`) and a call-bound fan-out unit (`startUnitCall`) go through both.
  The split is what keeps "a call that cannot be made is not a unit failure"
  true: planning writes nothing, so the unit edge plans **before** its row moves
  to `running` and a refusal leaves a `pending` row rather than a failed one.
  `callEdge` is what tells the two edges apart:
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
- **An argument the caller cannot resolve is judged by the CHILD's declared
  inputs, not by the caller.** `resolveCallArgs` (`call_args.go`) reports what
  did not resolve and `requireResolvedArgs` decides: an argument seeding an
  input the child declares `optional:` is OMITTED, so the child sees an absent
  optional input — the same run a direct start without that seed would have
  produced. One
  seeding a required input, or naming no child input at all, is a
  `wiring-error` park naming the argument and its reference. That ordering is
  why the child is resolved before the arguments are evaluated: optionality is
  a fact about the target, so it cannot be known before the target is. A
  campaign forwarding its own optional seed to its next wave dies at neither
  end of that.
- The parent attempt row is persisted (with its evaluated args) **before** the
  child exists — an argument that did not resolve is simply absent from the
  record, since the refusal is the call edge's to raise once it has resolved
  the child, and by then this row exists to carry it. That order is what makes
  a crash recoverable: the only gap it
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
- **A declared output the completing child did not produce is judged by its own
  declaration, not by the fact of its absence** (`childOutputEnvelope`). A
  REQUIRED one still fails — the caller's gate routes on these names — but an
  `optional:` one is OMITTED from the synthesized envelope rather than nulled or
  fatal, which is the same shape an absent optional call argument takes (D45),
  so the parent's optional input sees the "not supplied" a direct start would
  have produced. Before it, a campaign whose planner could exit directly
  declared a handoff sourced from a phase that route never entered, and the
  whole tree parked `wiring-error` at the exact transition that meant it had
  succeeded (incident D-C1). The dry-run now reports that shape statically too
  (`workflow.output-unreachable`, `internal/workflow/def`); the runtime rule is
  what covers a snapshot frozen before the finding existed.
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
  attempt's `park_cause`.
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
  level down: `startUnitCall` plans the edge against `unitVars` (the attempt's
  variables plus the `as:` binding), then persists the unit `running` and
  invokes it. The unit then rests holding no runner, no resources, and no
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
  onto the attempt's `park_cause` — nothing runnable was ever produced, so there is
  no unit outcome to record. The first two are decided by `planCall` before the
  unit row moves, which is what leaves that row `pending` rather than failed.
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
- Waking a bound thread is app wiring too. The engine's contribution is two
  events and nothing else: the `workflow:item-state` transition, and
  `workflow:gate-notify` for a `notify:` route the run continued through.
  `app_workflow_notifications.go` decides from those who is told and how (a
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
