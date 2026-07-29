# Workflows System — Specification (Agent Overflow)

> Status: **Settled, revision 2** — rev 1 planning completed 2026-07-11; rev 2
> (2026-07-24) re-centers the system on directly-started background runs and
> removes the queue. Implementation decisions + amendments live in
> `workflows-system-decisions.md` (rev-2 D-entries pending); the UI is
> specified in `workflows-system-ui/UI-SPEC.md` (rev-2 overlay rewrite
> pending). This is a WHAT spec (behavior / intent), not a HOW spec.
>
> Rev 2 supersedes rev 1's queue-centric model. Notable reversals, made
> deliberately: the work queue is **removed** (concurrency comes from
> resources, §6); the "join never git-merges" rule is **relaxed** to "no
> silent merge" (§9). Everything else in rev 1 that isn't explicitly
> rewritten here stands.
>
> Lineage: based on `orc`'s workflow/phase model where it fits; executes on
> Agent Overflow's existing thread/provider runtime.

## 1. Purpose

A system that drives **work items** — each a goal bound to a project — through
**defined multi-phase workflows**, autonomously to completion, pausing only at
explicit gates that need a human.

- **A run is a background job, started directly.** There is no queue and no
  drain cycle. Three entry points share one start path:
  - an **agent starts a run from a chat thread** (the §5 CLI) and keeps
    working; the run's resting states wake that thread (§5);
  - a **human starts a run from the overlay** (§10);
  - the **§11 scheduler** starts a run when a trigger fires.
- **Positioning: workflows are background jobs, not the center of the app.**
  Most work stays in normal threads. Workflows cover **large decomposable
  efforts** (parallel port/refactor campaigns, §3 fan-out + §3 call recursion),
  **scheduled/triggered automations**, and **unattended build-and-validate**.
  The surface (§10) is **deliberately secondary and quiet**: an overlay, one
  clear "needs you" signal, never a takeover.
- **Durability is structural.** Every run, phase attempt, child run, and
  fan-out unit is persisted as it happens; the app can die at any moment and
  the whole tree is still there, parked and resumable (§12).

## 2. Work item lifecycle

An item moves through a small set of states:

> **running → needs-human → (back to running) → done**, or **failed** / **cancelled**

- **running** — actively being driven through its workflow. An item is running
  from the moment it starts; a phase may be *waiting on a resource* (§6) while
  its item is running. **`running` is always exitable** — no wait is unbounded;
  §12 defines the watchdog, pause, cancel, and crash-recovery paths that get an
  item out of it.
- **needs-human** — paused for a person: a gate, a question, genuinely stuck, a
  human pause, or a §12 control tripped. Resumes to **running** once the human
  (or the origin agent, §5) responds.
- **done** — workflow completed to its terminal success.
- **failed** — stopped without success and not resolvable by a human handoff
  (vs needs-human, which is recoverable).
- **cancelled** — a **human chose to stop it** (the §12 kill button). Kept
  distinct from `failed` (work failed) so the two read differently on the
  surface (§10).

There is deliberately **no `queued` state**: a run starts running. Contention
is expressed as a *phase* waiting on resource capacity (§6), never as an item
waiting in a line.

**Every `failed` / `needs-human` / `cancelled` transition carries a typed
reason** (gate, question, stuck, stalled, paused, interrupted,
budget-exhausted, retries-exhausted, check-failed-genuine, agent-error,
wiring-error, disposition, setup-failed, unit-failed) — recorded in the run
record (§10) and shown on the run's row, so a stopped item is never a silent
dead end. `paused` (a human or graceful shutdown chose to stop it, §12) and
`interrupted` (the app died out from under it, §12) are distinct reasons with
the identical resume path, so the morning-after view tells you *why* it
stopped without changing *how* you continue it.

## 3. Phases and the variable system

A **phase** is a configurable unit of work — not merely "an agent." A phase
declares:

- a **driver**: **agent** (provider + model — Claude or Codex, per phase) or
  **tool** (a profile-bound deterministic command — build, test, lint, merge,
  provision). Deterministic work is a first-class phase, not "an agent
  babysitting a script." **Both drivers are in scope; neither is optional.**
- **tools / MCP** available to it (optional, per phase),
- a **prompt** assembled from a template with variables injected (agent
  driver), or a **command** resolved through the project profile (tool driver),
- an **access** declaration: `read-only` | `write` — enforced, not advisory
  (§9).

**How a tool phase produces its envelope.** The binding (`check:` or
`command:`) resolves through the live project profile to an **argv array** —
never a shell string — whose elements are interpolated from the variable
context exactly like an agent prompt. The process runs in the phase's
workspace with the profile's resolved secrets in its environment and
`AO_ENVELOPE` pointing at a file path it may write. Every tool phase emits
`passed` (boolean) and `exit-code` (number) whether or not it writes that
file; a command that needs to emit more writes a control envelope there and
the system overlays those two outputs onto it, since a process cannot know its
own exit status while it is still running. **A non-zero exit is `passed:
false`, not a phase failure** — the gate decides what a red check means (§4).
Only infrastructure failure (unresolvable binding, missing binary, a kill) is a
phase failure, and an unparseable written envelope parks directly: retrying a
deterministic command cannot make it valid.

**Variable system — the data backbone.** A workflow carries a variable context:

- the **workflow start** seeds it from the item's inputs (goal, ticket info,
  project config / tools, …);
- **each phase** reads variables into its prompt/command and **emits new
  variables**;
- **gates** read variables to decide.

Phases and gates are intertwined: **a phase's output is what drives its gate**,
and that output also becomes the variables the next phase consumes.

**Phase shape.** A phase honors one I/O contract no matter how it runs:

- **single** unit;
- **fan-out/fan-in** — N parallel units → a join that consolidates their
  outputs into the phase's envelope. Units are declared **statically** (a
  fixed list) or **dynamically**: `over:` names an array variable, `as:` binds
  the element, and one unit template stamps per element — the unit count is a
  runtime fact (e.g. a plan phase emits `sections`, the port phase fans out
  over them). A unit is bound to **exactly one** of three things — a `prompt:`
  (agent), a `command:` (tool), or a `call:` (another workflow, §3a) — and any
  other combination is a validation error. Each unit names its
  **own driver/provider/model/access** (mixed
  Claude/Codex fan-outs are a feature, not an accident) — and only the units
  and the join do: **a fan-out phase runs no work of its own, so declaring a
  driver, provider, model, prompt, command, or access on the phase is a
  validation error** naming the per-unit field instead. (A phase-level
  declaration that reached no unit would be an authoring trap: it reads like
  it governs them and it does not.) The phase still declares what belongs to
  the whole attempt — inputs, the outputs its join answers, the resources held
  once for the attempt, its watchdog, and the grants every unit's session is
  scoped by. Unit semantics:
  - units run in parallel, bounded by resources (§6);
  - **width is bounded absolutely by the project profile** (`max_fan_out_width`,
    §8 — default 32, no unlimited setting). This is *not* the §6 capacity
    bound, which throttles work that all still runs: a fan-out wider than the
    ceiling **refuses**, it never truncates. Silently dropping units would
    leave the join consolidating a set nobody chose. A static list over the
    ceiling is a **dry-run finding** (blocking, naming the phase, the width,
    the maximum, and the profile key); a dynamic `over:` width is only known at
    expansion, so the engine refuses it there — before any unit row,
    sub-worktree, or provider session exists — and parks
    `needs-human(wiring-error)` with the resolved width in the attempt's
    envelope. The ceiling is per project, never per workflow: a definition that
    could raise its own ceiling would not be one. It is read live at each
    expansion, like every other bound (D29);
  - each unit runs on its **own AO thread** (§7 — inspectable like any phase)
    and, when writing, in its **own sub-worktree** (§9);
  - **unit failure policy:** a unit that exhausts its §12 retries stops the
    launch of not-yet-started units; **in-flight units run to completion**
    (their work is durable — interrupting wastes it); then the phase parks
    `needs-human(unit-failed)` carrying the failed unit's thread id, narrative
    path, and partial envelope. Surfacing follows the run's thread binding
    (§5): a bound origin thread gets the wake with failure details; otherwise
    the overlay + OS notification. Recovery actions: retry the failed unit,
    drop it (join proceeds over survivors, recorded in the gate trace), or
    take over its thread — the one it is already running in (§7).
  - a **call-bound unit** runs a whole sub-workflow instead of a turn: it
    starts a child run (§3a) in the unit's own sub-worktree and rests until
    that run is terminal, and the child's declared `outputs:` become the
    unit's envelope. This is what expresses "each work item gets its own
    implement → review → fix loop on its own branch, then the join merges" —
    the unit is the isolation boundary and the child is the work. A call unit
    declares `call:`, `args:`, and optionally `max_depth:` and nothing else:
    provider/model/prompt, command, access, and outputs are all the child
    workflow's to declare. A **join may not be a call**: its envelope IS the
    phase's, and every phase-level continuation (an answer, a takeover
    finalize, a resume in place) is a continuation of the join's own session;
    fan out to a call unit instead.
  - the **join** is an ordinary unit (agent or tool) that runs after all units
    rest; its envelope is the phase's envelope. What a join *does* — synthesis
    or merge — is the author's choice (§9).
- **call** — the phase invokes another workflow (§3a). Its result envelope is
  the child run's declared `outputs:`.

("Long-running" is **not** a shape: a normal phase runs until it emits *done*,
however long that takes — even waiting on an external event is just a tool
phase that polls.)

### 3a. Call phases — composition and recursion

A **call phase** names a workflow `id` (statically — resolution follows §8
scoping; never a variable, so the dry-run can validate the whole call graph)
plus an argument map from the caller's variables to the child's declared
inputs. A **fan-out unit** may carry the same binding (§3); everything below is
true of both edges, and where they differ is stated per point.

- **The child is a real run**: its own item row, phase history, loop counters,
  budgets accounting, and run-record — linked to the parent and rendered as a
  tree in run detail (§10). The parent phase parks while the child runs; no
  process state is held (§12 crash recovery covers the whole tree).
- **Workspace flows down the call stack** (§9): the child executes in the
  caller's workspace context. Isolation is introduced only by fan-out, never
  by nesting — so a self-call loop iterates in one worktree instead of
  spraying them. For a **unit** call the caller's workspace context *is* that
  unit's sub-worktree, cut from the item's branch exactly as a writing agent
  unit's is and registered on the unit row before the child starts; the child
  provisions nothing of its own, and the join merges the unit's branch without
  caring whether a turn or a sub-workflow produced it.
- **Recursion is allowed and bounded.** A cyclic call graph (including
  self-call, and including a cycle closed by a fan-out unit's call edge)
  requires a declared **`max_depth`** on the cycle's call edge;
  exceeding it parks `needs-human(wiring-error)` with the call chain in the
  trace. The declared bound counts the edge — for a unit, the
  (workflow, phase, unit) triple — so sibling units of one attempt never spend
  each other's budget. Budgets (§12) are enforced against the **root item's**
  envelope across the entire tree, so a runaway recursion hits the
  token/USD/wall-clock ceiling even under a generous depth bound.
- **A child is resolved from disk per invocation.** The caller's own remaining
  phases keep the definition the run froze at start, but every call reads its
  target fresh (§8 scoping) — which is what makes a campaign's next wave pick
  up a prompt the human edited while the previous wave ran.
- **"Loop with an exit condition" is a call**: a workflow whose final gate
  either routes to a call of itself (next batch / next round) or to done. Each
  invocation is a fresh child run with fresh loop counters — per-iteration
  scoping falls out structurally instead of needing counter bookkeeping.
- Child runs never bind threads and never notify on their own (§5); they
  surface through the parent's tree.

**Phase output — control envelope + narrative file.**

- Every phase emits a **structured control envelope** (system-owned). Its schema
  is **generated, never hand-written**, and **flat**: system control fields at
  the top level — a **closed status set `done` | `question` | `stuck`** (§7) —
  with the phase's declared **output variables nested under `outputs`** (no
  collision with control fields by construction). Discriminated-union schema
  shapes are **impossible cross-provider** (D2a), so branch requirements
  ("question text required when status is question") are enforced by the
  engine **and stated verbatim in the system prompt suffix** — a rule the
  engine enforces but never states costs the phase its envelope retry (§12).
  The generated schema honors both providers' strict-mode rules
  (`internal/providerschema` is the single source of those rules; the mock
  provider enforces them so the harness cannot drift from reality).
- **Engine post-validation is the sole success authority.** The engine
  re-validates every envelope against the full schema regardless of what the
  provider claims (§10), and **envelope size caps are engine-enforced**
  ("write to a file, return a path") — run records stay lean.
- The **narrative** (what it did, reasoning, decisions) is written to a
  **file** in a system-dictated location; its path is **system-attached** to
  the run record, never an agent-filled envelope field. The system never
  parses it; humans and later phases read it on demand.

**Workflow-level `outputs:` — the run's deliverables.** A workflow may declare
named values and/or artifact files, sourced from phase outputs — distinct from
the narrative (a process log). Artifact files are **copied into an app-managed
per-run artifact store at the producing phase's completion**, so deliverables
survive worktree discard (§9). Run detail lists them (§10); agents fetch them
through the §5 CLI; a bound origin thread receives them in the wake message
(§5). A call phase's envelope carries the child's workflow outputs.

**Typed, validatable variables.** Variables are typed as **JSON Schema
fragments** (string/number/boolean/enum/array/object — no custom type system);
most required, some optional.
A **dry-run validates the whole workflow before it runs** — and not only
type-wiring. It statically checks: every required input has an upstream
producer and the types line up; **every gate outcome names an existing phase;
every phase is reachable from start; every loop-back declares its §4 bound;
every call edge resolves to a validatable workflow and every call cycle
declares `max_depth`; write-need propagates through the call graph (§9); every
referenced resource is bindable by ≥1 project profile; every variable
reference resolves; every `over:` reference names an array-typed variable;
every static fan-out list is inside the binding profile's `max_fan_out_width`
(§3, §8).**
Each failure **names the offending element** ("gate G routes to phase X, which
does not exist"), so a miswired workflow fails fast with a precise message
instead of mid-run. The dry-run also **reports** (informational, not a
failure) a static fan-out wider than a binding profile's provider capacity —
the run would throttle to capacity (§6), and you should learn that from
validation, not from watching units wait. A dynamic `over:` width is a runtime
fact; its throttling surfaces as waiting-on markers in run detail (§10).
The capacity report and the `max_fan_out_width` finding are different
statements about the same phase and both stand: inside the ceiling but over
capacity is pacing, over the ceiling is a refusal to run at all. The width
finding applies even when no profile resolved — a project without a
`profile.yaml` still gets the default ceiling at run start, so skipping it
would let a definition validate clean and then be refused at its first
expansion.

**Validation principle (anti-theater).** Deterministic project checks play two
roles: (a) the agent is **given them to validate its own changes as it
works**, before it declares a phase done; and (b) a **deterministic gate**
runs them as the hard backstop. The goal is *"give the agent what it needs to
prove its change is sound,"* **not** *"make the tests pass."* The system must
never reward gaming a gate over genuinely validating the change.

## 4. Gates and transitions

After a phase completes, a **gate** decides what happens next. A gate is a
**pure routing decision** — it reads variables (including the phase's output)
and picks an outcome; it does no work of its own. All *work* lives in phases.

**Shape: an ordered route list on the phase, first match wins.** There are no
free-floating gate nodes — a gate is its phase's exit routing. Predicates are a
**small structured set** (comparisons, membership, existence, and/or/not
combinators) evaluated in order with short-circuit; string-shape decisions
belong in a phase, not a predicate. **A runtime no-match** (list exhausted,
nothing matched) parks `needs-human(wiring-error)` with the evaluated
predicate trace persisted — never a crash, never a silent advance.

**Outcomes:** **advance** (to a named next phase) · **loop back** (carries
feedback; **every loop route declares a mandatory bound**) · **park**
(needs-human).

**Loop bounds are per fresh entry, not per item lifetime.** A loop edge's
counter counts consecutive traversals **since the loop's target phase was last
entered from outside the cycle**; entering the cycle fresh (a forward edge, a
call, a human re-run) resets it. Counters remain derived from the persisted
gate traces — nothing new is stored — and human-takeover attempts still don't
consume loop budget. (Rev 1 counted per item lifetime, which starves later
iterations of any retry budget inside outer loops; batch-scale iteration
belongs to call phases (§3a), whose child runs get fresh counters by
construction.) An exhausted bound falls through to the next route; exhausting
everything parks needs-human.

**What a gate reads to decide:**

- a **deterministic check's** result — a tool phase (tests, lint, build,
  merge): pass/fail + typed variables,
- an **ai judgment** — an agent phase that assesses readiness,
- a **human** decision — approve/reject (**a rejection carries an optional
  note** that rides as feedback into the loop-back), or an answer that injects
  variables and resumes the phase,
- **auto** — the prior phase simply succeeded.

These are not three evaluator subsystems: **what produced the variable is the
evaluator**; the gate mechanism is one.

Anything a gate seems to "do" — run a script, investigate a failure — is
actually a **phase the gate routes to**.
Example: on a failed check the gate routes to a **diagnosis phase** whose
structured report becomes a variable, feeding a smarter retry or a more useful
park — never a blind loop.

**Real failure vs flake/infra.** A red check is *not* automatically the
change's fault. The diagnosis phase classifies genuine failure vs.
environment/flake so the system never loops or parks on a broken stack.
(Hard-won from prior runs.)

**Execution-failure retry is a separate layer.** Loop-back above is for a
phase that *produced a result* the gate rejects — it carries validation
feedback into the retry. A phase that **failed to produce an envelope at all**
(a transient provider overload, network drop, or subprocess death) is
different: no feedback, just wait-and-retry. §12 handles it with a small
conservative backoff; do not conflate it with feedback-carrying loop-back.

**Structure.** Because gates route among phases, a **workflow is a graph of
phases connected by gated transitions**, not a straight line.

## 5. Phase capabilities, the `ao` CLI, and thread binding

**Structure is declared; agency is granted.** The phase graph is fixed and
statically checkable. What an agent can *do dynamically* inside a phase is the
set of **tools its phase is granted** — this is where side-effecting power
lives, deliberately kept out of the graph so the graph stays analyzable.

**First-party agency is a scoped CLI (`ao`), not an injected toolset — and not
MCP.** Phases and interactive chat threads talk to the system through one
short CLI; **nothing workflow-related sits in provider context until it is
used** (an MCP server's tool schemas are always-loaded context; a CLI is zero
until invoked, with depth discovered via `--help` — and it is provider-neutral
by construction). The **per-phase credentials injected at phase entry
authorize only the subcommands the workflow granted** that phase — "tools
granted to a phase" *is* that grant. From an interactive chat thread the same
CLI rides the provider's normal bash-approval UX with the user's full
authority.

**The CLI surface** (workflow-facing): `agent-overflow workflow validate | list | schema |
new` (authoring) · `run | status | result | list-runs` (execution) · `pause |
resume | cancel | rerun | retry-unit | retry-failed-units` (control). `run`
starts a run
immediately and returns the run id; it does not block.

**Thread binding and wake — one mechanism, three entry points.** Every root
run records an optional **bound thread**:

- **agent-started** (from a chat thread): bound to the origin thread
  automatically. When the run reaches a resting state — done, failed,
  needs-human — the app **wakes the thread** by injecting a compact result
  message (terminal state + typed reason + workflow outputs + narrative /
  artifact / failed-unit-thread references) through the **existing
  queued-user-message path**: delivered at the provider's next tool boundary
  mid-turn, immediately when idle — exactly like a user sending a message into
  a running thread. The agent resumes with the result in context, acts, or
  escalates to the human.
- **human-started / scheduled**: unbound until needed. Resting states surface
  via the overlay + OS notification (§10); **"open in thread"** creates and
  binds a thread seeded with the run's result (the intent-seed pattern), or
  **binds an existing open thread** chosen from a picker — after which resting
  states wake that thread like the agent case.
- **child runs** (§3a) never bind and never notify; they surface through the
  parent's run tree. A descendant that parks `needs-human` while the root is
  still waiting produces the **root's** wake and notification, composed to name
  the parked descendant (its run id, workflow, typed reason, and parked phase) —
  a subtree blocked on a question is never invisible just because the run
  holding it is not the one a human is watching.

If a bound thread has been deleted, the run falls back to the unbound surface
— a wake is never silently lost.

Grantable first-party capabilities (phase-side):

- **start a run** — decomposition: spinning off independent work that is *not*
  part of this run. The started run binds to the *starting phase's* thread
  only if the grant says so; default unbound, surfaced in the overlay.
- **schedule a job / automation** — hand work to the §11 scheduler;
- **report back** — the outbound twin of §11's poller: a terminal phase posts
  the item's result (status / comment) to the ticket or MR it came from, by
  shelling the user's authenticated, **profile-bound** command (`gh` / `glab`
  / `acli` — §8). Same delegate-to-host model — no stored creds — so a
  scheduler-driven loop *closes* (work gets done **and** the human watching
  the ticket learns);
- **update job notes** — a terminal phase may rewrite its job's continuity
  notes (§11); a no-op rewrite is a normal outcome;
- **introspection** — run status / outputs / records, for the agents that
  manage the system (a woken origin thread investigating a failure, an agent
  a human pointed at the workflow files).

**Deferred (post-v1): agent-invoked inter-agent discussions** — an agent
questioning another agent via AO's native discussions. Additive, not
structural; human-initiated discussion flows are unaffected.

**Side effects are idempotent across re-runs (surface-and-skip).** Loop-back
(§4), take-over discard+re-run (§7), and crash-recovery re-runs (§12) all
re-enter a phase, so a first-party side-effecting tool (start-run / schedule /
report-back) **records what it did per (item, phase)**; on a re-run the system
**shows the prior effect and defaults to skip** rather than silently re-firing
(no duplicate Jira ticket, no double-started run). It does *not* try to make
arbitrary MCP side effects idempotent — those it simply flags as "will replay"
to the human.

**Both.** Any **MCP server can be configured** (once set up) and granted to
phases, **plus** the curated first-party CLI above for interacting with the
system itself. MCP is a per-phase grant for *external* tools; the system's own
surface stays CLI.

## 6. Concurrency & resources

There is no queue. A run starts immediately; **contention lives at the phase
level, on resources**:

- A **phase declares the resources it needs**; a **resource has a capacity**
  (a mutex is capacity 1; "3 testcontainer slots" is capacity 3). A per-phase
  concurrency cap is just a resource named after that phase.
- **Every agent phase and fan-out unit implicitly acquires
  `provider:<name>`** (e.g. `provider:claude`, `provider:codex`) with a
  capacity set in the project profile (default small). This is the bounded-
  parallelism guarantee the queue used to provide: thirty started runs do not
  mean thirty concurrent CLI sessions — phases wait on provider capacity, and
  a waiting phase is visible as such in run detail (§10). Tool phases don't
  acquire provider capacity.
- A resource lock is **phase-scoped**: acquired on phase entry, released on
  exit — including on failure, park, or pause (never leaked). An item holds
  `live-stack` only during the phase that needs it, not its whole run.
- **A capacity is not a ceiling.** Capacity paces work that all still runs;
  the profile's `max_fan_out_width` (§3, §8) refuses a fan-out attempt outright
  before any unit starts. A fan-out inside the ceiling but over provider
  capacity simply throttles, which is what the dry-run's width *report*
  describes; a fan-out over the ceiling never runs, which is a *finding* and a
  runtime refusal.
- **Workflow declares needs; the project profile declares capacities.**
  `live-stack` capacity 1 is an environment fact for a project, not part of
  the workflow — a workflow stays portable across projects. **Capacities are
  read live at each acquisition** — editing `profile.yaml` (including the
  `provider:<name>` limits and `max_fan_out_width`) takes effect on the next
  phase/unit start or fan-out expansion, no
  restart, no re-run. **Capacities are per-project instances**: the same
  resource name in two projects never contends (an app-wide global resource
  is a possible later addition, not now).
- (Impl note) multi-resource acquisition uses a canonical order /
  all-or-nothing to avoid deadlock; freed capacity goes to the longest-waiting
  phase.
- **Lock release is the §12 teardown path.** Every way a phase stops (success,
  failure, park, pause, cancel, watchdog, crash-restart) runs the one teardown
  path, so a lock is never leaked. A phase *blocked on a resource* is
  therefore bounded by the holder's guaranteed release on any exit — it is
  **not** the "stalled" case the §12 watchdog targets (that one is an *active*
  turn that stopped emitting).

**Global pause — the kill switch, not a queue.** One engine-level toggle:
while paused, **no new phase or unit starts anywhere**; in-flight turns finish
and their items rest at the next phase boundary. Unpausing releases held
starts. This is the "stop the world safely" affordance (and the graceful-quit
path, §12) — it carries no ordering, no priority, no per-project variant.

## 7. The human surface — viewing, steering, and needs-human

**Every phase and fan-out unit runs as a normal AO thread** in its workspace
(worktree, sub-worktree, or the project root for derived-read-only runs, §9),
so one rule governs all human interaction: **a turn must be *yielded* before
you can inject into it.**

**Viewing is passive.** Open a phase's thread and watch it stream live. No
intervention — the phase runs its agentic turn (with the output schema
attached) and finishes normally.

**Pause is first-class.** Pausing a run interrupts its in-flight turn(s)
through the §12 teardown (partial envelope recorded, locks released) and parks
`needs-human(paused)`. **Resume** creates the next attempt **on the same
provider thread** with a continue message — the identical mechanics as
answering a question below, so the agent keeps its session context across the
pause. Pausing a run with active fan-out units interrupts every in-flight
unit; each unit resumes on its own thread.

**Sending a message takes over.** If the turn is **in-flight**, sending
**interrupts it first** (forces it to yield); your messages then run as
**free-form turns with no schema** — ordinary steering in that thread and
worktree. You finish the takeover by:

- **Complete** — the system runs one **finalize turn that re-attaches the
  output schema**; its constrained final message is the phase's envelope (§3)
  and the gate routes as if the phase had finished on its own. (You may
  instead fill the envelope's fields by hand — it is structured — with no
  model turn.)
- **Discard + re-run fresh** — if your poking left the worktree somewhere you
  don't want to ship, drop it and re-run the phase. The re-run can seed from
  the phase's **recorded input envelope** (§10) rather than only live state —
  so iterating on one phase of an N-phase graph doesn't require re-running the
  whole graph. This is also the path a human uses to resume a phase that §12
  parked after a crash or watchdog trip.

Interrupting leaves the worktree **mid-edit** (partial state) — fine, you took
over to fix things, which is why the natural finishes are Complete or discard
+ re-run, not "resume exactly where it was." The multi-turn session keeps the
agent's context across the interrupt. (Relies on AO's existing
stop/interrupt.)

**Discard with a loss preview.** Any resting run offers **discard**: before
anything is destroyed, the system shows exactly what would be lost — per
worktree in the run's tree (primary, sub-worktrees, child runs): branch name,
dirty file count, unmerged commit count. Accepting tears the tree down through
the §12 contract and removes run-created branches whose commits never landed.
The preview is the consent; nothing is cleaned silently.

### needs-human — when the phase yields on its own

A phase parks at **needs-human** because of a **gate**, a **question**, being
**genuinely stuck**, or a **failed unit** (§3). `question` and `stuck` are
**envelope statuses** (§3) — the turn ends cleanly with that shape; nothing is
suspended provider-side. It surfaces (overlay + notification, §10) with its
**blocking reason**, a short **digest**, and the **diff / checks / cost /
narrative** — never raw internals (below). If the run is thread-bound (§5),
the wake message carries the same digest to the origin agent, which may
resolve it through the CLI (answer, retry-unit, retry-failed-units, rerun) or
escalate to you.
Resolve by:

- **Gate decision** — approve / reject (optional note). A click; the gate
  routes. No thread interaction needed.
- **Answer a question** — the turn already ended with status `question`. Send
  the answer; it runs as the **next turn in the same session with the same
  schema attached** — straight to the envelope. No interrupt, no finalize
  turn, no provider-level suspension mechanic.
- **Take over** — drive it yourself, exactly as above: open the phase's
  thread from the run tree and send (steer free-form → Complete or discard +
  re-run). The send is what takes over; there is no take-over button (D32).
- **Unit recovery** — for `unit-failed` parks: retry the unit, retry every
  failed unit of the attempt at once, drop it (join proceeds over survivors),
  or open its thread. The whole-attempt repair (D33) is the usage-limit case:
  one cause fails most of a wide fan-out, the human waits the limit out (or
  switches account) and repairs all of it with one action — the same edge, the
  same reopened attempt, and the same admission through the project's
  semaphores as repairing each unit in turn, so a wide repair queues instead of
  bursting. Units under human steering are left alone.

| Phase turn state | Sending a message → |
|---|---|
| In-flight (running) | interrupt → yields → steer free-form → **Complete** (finalize turn re-adds schema) or discard + re-run |
| Parked on a `question` envelope | answer runs the next turn, same session, same schema → envelope |
| Parked `paused` / `interrupted` | resume runs the next turn, same session, continue message |

**One contract, one schema.** Whether a phase finishes on its own, is
completed via a finalize turn, or has its fields filled by hand, the result is
the *same* typed envelope constrained by the *same* output schema. The gate
never needs to know which produced it.

### Resolving at scale — the sweep

- **Needs-attention sweep.** Run detail (§10) steps parked runs one at a time
  (`j`/`k` next/prev), each leading with a short generated digest ("what
  happened / what it needs") and inline approve / answer / resume / rerun /
  retry-unit / retry-failed-units / discard. The morning-after throughput
  path.
- **Handing work to an agent is the wake, not a button (D32).** A run bound to
  a thread (§5) delivers its digest into that conversation on every resting
  transition, and the agent there resolves it through the CLI. The overlay's
  one-click "spawn me a triage thread" affordances — per-run hand-off and the
  per-project triage agent — were removed: the useful hand-off already happens
  through the binding the CLI made, and a button that opens a fresh
  conversation about a run was context transfer without a conversation to
  transfer it into. A better primitive may replace it later.
- **Taking a run over is a send.** Open the phase's thread from the run tree
  and type: the send interrupts the turn, detaches the attempt, and parks the
  run `needs-human(taken-over)`. No thread is created for it.

**No internals on human surfaces.** Variables, envelopes, JSON, and gate
traces never render in the human UI — they exist to make workflows manageable
*by agents* through the `agent-overflow` CLI. Human phase detail = narrative
digest, diff, checks, cost.

## 8. Intake — creating and starting runs

**An item, minimally:** a **project** + a **goal/prompt** + a **chosen
workflow** + a **base branch** + optional **seed variables**. The **project is
required** — this app controls many repos; it scopes the item for display and
is the repo its worktree is cut from.

**Producers of that same shape, all calling one start path:** a human in the
overlay (§10); **an agent from any chat thread** via `agent-overflow run start` (§5 —
the run binds to the thread and wakes it); a phase via the **start-run
capability** (§5 decomposition); the **§11 scheduler** on a trigger. Manual is
just the n=1 case; every producer emits the identical item.

**The `/workflow` composer command — context on demand, never standing.**
Nothing workflow-related sits in a chat thread's context by default. When a
thread is about to author or drive workflows, the user invokes `/workflow` in
the composer (or the overlay's "author in a thread" button opens a thread
pre-seeded the same way): it injects one compact block — the `ao` binary path,
this project's workflow directory and scope rules, the §5 command cheat-sheet,
and links to the project's active runs. Everything deeper is discovered via
`--help` and `agent-overflow workflow schema`. This replaces rev 1's chat-enqueue MCP
server and its proposal/confirm flow entirely: the agent starts runs directly
through the CLI under normal bash approval, and the human control point is the
overlay and the workflow's own gates — not a pre-start confirmation click.

**Step mode** is a per-item intake option ("pause at every gate"): every gate
behaves as a human gate — the trust-building mode for a new workflow. A
workflow may also declare it as its default.

**Workflow is chosen explicitly at start** — you name the workflow.
Attribute-based **routing** (type/labels → workflow) is a convenience that
rides in with the scheduler (a trigger must pick a workflow without a human);
the core stays explicit.

**Seeds** come from whatever the producer has: a ticket's fields (Jira /
GitHub import), what you typed, the conversation context the agent shaped
them from, the project profile.

### Workflow scope and the project profile

**Definitions live in app-managed directories — shared plus per-project —
never in the project's repo.** **Disk is the source of truth**: definitions
are never stored anywhere else (the only persisted copy is a run's frozen
snapshot, below). A workflow's identity is a **declared `id`** inside the
file, not its filename; **resolution is project → shared**, project winning on
`id` collision. Edit a file → the next start picks it up.

**Authoring is file work, started by the human.** Creating or editing a
workflow or profile means editing the definition files — `agent-overflow
workflow new` scaffolds one, `agent-overflow workflow validate` dry-runs it,
and an agent can be pointed at them from any thread the human opens. There is
no dedicated editor surface and **no button that spawns an authoring thread**:
D32 removed the studio spawner from the overlay, because a surface that starts
a conversation for you is not how this authoring actually gets done.

A **project profile** is an app-managed per-project **`profile.yaml`** (same
never-in-repo rule) holding the per-project **bindings**: the base branch, the
concrete **check commands** (the deterministic build/test/lint of §3), the
**tool-driver command bindings** (§3), the **resource capacities** (`live-stack`
= 1, the `provider:<name>` capacities — §6), the **maximum fan-out width**
(`max_fan_out_width` — the absolute ceiling on the units one fan-out phase
attempt may expand to, §3; default 32 when omitted, minimum 1, and deliberately
**no unlimited setting** — a project that wants 50 writes 50), the
**report-back / query-source
command bindings** (§5, §11), the **MCP / tool configs**, the **reliability
defaults** (the §12 inactivity timeout, envelope-retry count, optional
per-item budget), **secret references**, a **worktree setup** step (files
copied from the main workspace and commands run at worktree creation — `.env`,
dependency install; **setup failure parks `needs-human(setup-failed)`** before
any phase starts on a broken tree), and the **disposition policy** (`manual` |
`auto-pr` | `auto-merge`, default `manual` — §9).

**Secrets are references, never values.** Phases, scripts, and MCP servers
often need credentials (a private registry, an MCP API key, a paid API). The
profile stores a **reference** — `name → OS env var / keychain entry / file
path` — resolved into the phase environment / MCP config at phase entry and
**never persisted** into the run record, envelope, or narrative. Resolved
values are **masked before the agent-written narrative is saved** (the
narrative is untrusted output and would otherwise leak a key). Same
delegate-to-host philosophy as §11: the system holds no secret material, only
pointers.

**Workflow declares; project binds.** A workflow is the **portable graph** —
phases, gates, transitions, and the checks/resources/commands it references
*by name*. The **project profile supplies the concrete bindings** those names
resolve to, so the same workflow runs correctly across projects, each
parameterized by its own profile.

A workflow is therefore **either shared** (any project runs it, resolved
against that project's profile) **or project-scoped** (defined for and offered
to only one project, when it only makes sense there). An item picks a workflow
that is shared or belongs to its project. Generic workflows stay shared rather
than being copy-pasted per repo.

### Pin a run to the workflow it started on

At **run-start**, the engine **freezes the resolved workflow graph + project
bindings into the run record** (§10) and executes from that **snapshot**,
never live config. You *will* edit a workflow while runs are active (the
normal solo-dev loop); pinning means a running item can't hit a phase that
vanished, a gate whose target moved, or a renamed variable. Edits affect only
**later** starts. A call phase resolves and freezes the **child's** definition
at call time into the child's own record (so a mid-run edit affects the next
iteration's child, never the in-flight one — and "which definition did
iteration 3 use?" stays answerable).

### Starter workflows

Ship **2–3 example workflow files** plus a **`new workflow`** scaffold that
stamps a minimal valid graph. The examples encode the hard-won patterns — a
diagnosis phase, a bounded loop-back, a partitioned fan-out with a merge-join
and conflict-resolution loop (§9), a recursive batch driver (§3a) — so
authoring is "tweak an example," not "design from scratch." Content, not
engine.

## 9. Worktree & isolation

**Workspace need is derived, not declared — and propagates through calls.** If
no phase in the graph (including the graphs of every workflow reachable
through call edges) writes, the run gets **no worktree** — it executes
read-only against the project root. Report/research/triage workflows stop
paying the worktree tax; their deliverables go to the §3 artifact store, never
the repo.

**`access` is enforced, not advisory.** A phase's / unit's access declaration
maps to the provider session's runtime mode: `read-only` runs sandboxed
(no file writes, no destructive commands — the provider's restricted mode);
`write` runs with full access **in its own isolated workspace**. A read-only
phase on the project root physically cannot dirty it. (Rev 1 derived worktree
need from `access` but never enforced the mode at the session — closed in rev
2; unattended parallel writers make this non-negotiable.)

**Otherwise: one item = one primary worktree + branch**, cut from the project
profile's base branch at start (base overridable at intake). **By default it
persists through every terminal state** (done, failed, parked, cancelled) — it
is the artifact you act on: inspect it, **work in a thread in that worktree**
(steer a running/parked phase's own thread, §7; or **fork a fresh thread** to
continue a *done* item), or dispose of it. A per-workflow **cleanup policy**
(`auto` | `manual`, default `manual`) amends this: `auto` discards the
worktree at terminal state once §3 artifacts are captured — for writing
workflows **only after a successful disposition has landed**; an unlanded
branch is never silently discarded.

**Workspace flows down the call stack (§3a).** A call phase's child executes
in the caller's workspace context: the item's primary worktree for a serial
call, the unit's sub-worktree when called from inside a fan-out. A child never
provisions isolation of its own; only fan-out does. Consequences: a self-call
loop iterates in one worktree; a child's writes land on the caller's branch
with no merge step; the whole call tree shares one disposition (the root
item's).

**Writing fan-out units get isolated sub-worktrees**, each on a real branch
cut from the item's branch at unit start. Sub-worktrees and their branches are
**registered in the run record** and torn down through the §12 contract —
a crashed run can never strand them silently; the §7 discard preview lists
them. A **call-bound unit** (§3) gets exactly the same checkout, cut and
registered on the same unit row: what runs inside it is a child run rather
than a turn, which the join, the discard preview, and the crash sweep are all
indifferent to. Its try number names the branch, so a retried unit's next
child cuts fresh instead of inheriting what the failed try left behind.

**Two join patterns — the author picks by what the join does:**

- **Synthesis join** (explore-and-synthesize): N units attempt the *same*
  goal; the join **reads** their diffs and envelopes and produces a new
  artifact — a pick, a spec, a fresh implementation in the primary worktree.
  Unit branches are ephemeral inputs, disposed after.
- **Merge join** (partitioned work): N units each own a *disjoint* slice; the
  join is a **tool phase that git-merges unit branches into the item's
  branch**, emitting typed outputs (`clean`, `conflicts[]`, check results).
  Its gate routes conflicts to a **resolution agent phase** (write access,
  conflict state in its variables) and loops bounded (§4), then parks.
  Conflict handling is **agentic by design**: there is deliberately no
  declared path-ownership metadata, no per-file no-touch lists (enumerating
  what units must avoid is unmaintainable and rots into active harm). The
  merge is attempted deterministically, conflicts arrive typed, and an agent
  resolves them with the full conflict state — bounded, then human.

**The system never merges silently** (rev 1's "the join never git-merges"
relaxes to this): every merge is an explicit tool phase whose result is typed,
gated, and routed — attempted deterministically, resolved agentically,
bounded, then human. Disposition (below) stays the only place the *project's*
base branch is ever touched.

**Parallel-writer fan-out vs decomposition** — they look alike but differ in
*what survives*:

- **Decomposition (§5 start-run)** spins off **independent deliverables** —
  each its own item, own primary worktree/branch, surfaced and disposed
  separately by you.
- **Fan-out** spawns work **inside one item**; what survives is what the join
  synthesizes or merges into the item's branch. Units never surface as
  separate deliverables.

**Disposition is in-app: merge / PR / discard** on done items. **Nothing
auto-merges by default** — a project profile may opt into
**auto-merge-on-done** (§8; side projects; production stays manual), which
proceeds only on a clean merge from a green terminal state. **Any conflict or
dirty base refuses and parks `needs-human(disposition)`** — never forced,
never silent.

## 10. The surface — the workflows overlay and per-phase threads

**One button, one overlay — panes never unmount.** A workflows button in the
sidebar footer (bottom-left) carrying the single needs-attention badge opens
the **workflows overlay**: a full-surface layer rendered **over** the pane
host as a sibling — the pane tree stays mounted and untouched underneath, so
closing the overlay is instant and rebuilds nothing. (Explicitly not the
settings pattern, which swaps the surface out and forces pane remounts.)
There is no workflows pane, no per-project sidebar section, and no workflow
presence in normal thread lists — **phase, unit, studio, and triage threads
are excluded from thread lists**, reachable from the overlay (and openable as
normal thread panes beside it once summoned). Studio and triage-agent threads
are no longer created (D32); the exclusion keeps existing ones hidden.

**Overlay structure — project-grouped, drill-down:**

- **Home**: per project — its workflow definitions (read-only rows; authoring
  is file work, §8) and its runs: active first (live state, current phase,
  waiting-on markers), then resting (needs-attention leading), then recent
  history. Scheduled automations (§11) show trigger + next fire inline.
- **Run detail**: the run **tree** — phases in order; a fan-out phase expands
  to its units; a call phase expands to its child run (recursively). Leads
  with the digest, diff, checks, and cost; hosts the resolution actions
  (approve / reject / answer / pause / resume / rerun / retry-unit /
  retry-failed-units / take-over-unit / finish-takeover / discard-with-preview /
  disposition / bind-thread) plus the §7 needs-attention sweep. Every phase/unit attempt
  links its thread — completed, failed, and superseded attempts included.
  Every one of those opens a thread that already exists; **no action here
  creates one** (D32).
- Threads always break out of the overlay into normal panes; the overlay is
  for state and action, not for chatting.

**The run record is persisted.** The overlay is a read over a **persisted run
record** — ordered phases, each phase's envelope + gate decision + narrative
reference, timings, resource waits, unit and child-run linkage, the **frozen
workflow snapshot** the run executed under (§8), a **typed reason** on every
`failed` / `needs-human` / `cancelled` transition, and a per-phase
**intervention** field (take-over, complete-by-hand, discard+re-run, pause —
kind / when / note). It is also the **recovery journal** (§12).

### Notifications

- A **native OS notification** fires on **`needs-human`** and **`failed`**
  only (never on `done` / `running` — those are the passive badge), carrying
  project + goal + the typed blocking reason, and **deep-links to the parked
  run's detail** in the overlay. Thread-bound runs (§5) additionally wake
  their thread — the notification still fires; the badge is still
  authoritative.
- The notification is a **nudge, not the source of truth**: the durable
  needs-attention badge on the workflows button **re-surfaces on app open**,
  so a missed or cleared notification never loses work.

(Rev 1's drain-to-empty coalesced summary died with the queue.)

### Streaming and the structured envelope coexist (verified empirically)

A phase must both **stream** (so you can observe/steer it as a thread) and
**emit its typed control envelope** (§3). These coexist via an **output schema
constraining the turn's final message** — verified live and adversarially on
**both** providers (the D2a spike):

- **Claude:** the streaming session takes the schema; the full agentic event
  sequence streams; the payload arrives on the final result. But the mechanism
  is an *encouraged* synthetic tool call — **a turn can end cleanly with the
  payload simply absent** (the explicit retry-error subtype fires only under
  unusual pressure).
- **Codex:** the app-server takes a per-turn schema; decoding is
  logit-constrained but **validates structure only** — value constraints are
  silently ignored.

**So "valid-or-explicit-error" is false on both providers.** The engine
invariant (§3): **the only success signal is a present payload that passes the
engine's own full-schema validation.** Absent-or-invalid = envelope-production
failure → the §12 feedback-retry-then-park path. Never gate on provider error
flags or turn status alone.

The envelope remains the **constrained final message** of the phase's agentic
turn; intermediate work streams and stays observable; the **narrative** is
written to a file during the turn (§3). Wiring specifics (flags, params,
payload locations, per-turn re-send quirks) live in the decisions log (D2a);
the schema-generation rules both providers enforce live in
`internal/providerschema` (§3).

**A mocked suite cannot prove schema acceptance, so one gate is real.** Mock
providers accept any schema; the CLIs enforce strict mode and refuse the phase
before it starts — which is how five envelope-schema defects survived a fully
green harness. `make provider-smoke` (build-tagged out of the ordinary Go
suite, so `make verify` stays hermetic) drives one trivial single-phase
workflow through the **real** `claude` and `codex` binaries under default
binary resolution and asserts the three things only a real run can show:
the CLI accepted the generated envelope schema, the run reached `done` through
a real envelope carrying its declared output, and the writing phase ran in the
run's own worktree/branch (§9) with the project checkout untouched. It spends
real tokens, so it is manual: run it before a release and after upgrading
either provider CLI. A live rejection that `internal/providerschema` does not
already flag means the rule set is short one rule, and adding it there is what
makes the mock catch it from then on.

## 11. Scheduler / automations

An **automation = a trigger + an optional condition + an action**, and **the
action is always to start a run** (§8) through the same path every other
producer uses. The scheduler never runs a second executor.
(A "single-phase," "a script," "script → workflow," or "an agent that decides"
are all just *workflows*, §3 — so every action reduces to one item with a
chosen workflow + seed variables; an agent-decider is a workflow that starts
0..N runs via the start-run capability.)

**Overlap policy: skip-if-running.** If the previous run this automation
started is still active when the trigger fires, the fire is **skipped and
recorded** (visible on the automation's row). No queueing of fires, no
overlapping runs of one automation. (A workflow that genuinely wants overlap
is two automations.)

**Results reach a human through the §5 binding mechanism**: a scheduled run is
unbound; its resting states surface via overlay + notification, and "open in
thread" seeds a thread with the result for follow-up — the same affordance as
any human-started run. (A report automation's deliverable is its §3 artifacts;
the thread is how you discuss them.)

**Two trigger primitives:**

- **Cron** — time-based; any schedule.
- **Internal event** — the system's own lifecycle (item done / failed /
  needs-human, a phase completed, a gate outcome). A **closed, typed set** the
  system already emits (the §10 run-record stream); carries run context as
  variables. This is what chains automations (one item finishing triggers the
  next).

**Conditions** (run-if, reusing §4) gate whether a fired trigger actually
starts a run.

**Minimal in-overlay management + Run now.** The overlay (§10) shows an
automation's trigger, enable/disable, next fire, and skipped fires, plus a
**Run now** button. Anything richer (changing cron, seeds, conditions) is §8
file work over the automation config, not forms.

**Job continuity notes.** A scheduled/triggered job carries **per-job notes**
(a markdown blob) for cross-run continuity: visible and editable in the UI,
injected as a reserved seed variable, and optionally rewritten by a terminal
phase via the §5 **update-job-notes** capability (a no-op rewrite is a normal
outcome). Deliberately a notes file per job, **not a memory subsystem** (the
exclusion below stands).

### External sources — poll with authenticated CLIs, no webhooks

External triggers (MR comments, ticket transitions, pushes) are reached by
**polling through the user's already-authenticated CLIs** — **profile-bound
commands** (§8: `gh`, `glab`, `acli` for Jira), never a tool name hardcoded in
the engine — **not** by inbound webhooks. A poll is just a **cron automation
whose action workflow queries the source and starts** what's new: a phase
shells out to the CLI, computes "new since a cursor," and starts a child item
per hit. It is **not a third trigger primitive** — just cron + a
query-and-start phase using the curated **query-source** and **start-run**
tools (§5).

**The system manages no external credentials.** Auth is delegated to the
user's logged-in CLIs; the system holds no tokens and runs no inbound server.

**Required for correctness:** a **cursor/watermark** per polled source
(last-seen id / `updated >=` timestamp) + **dedup**, so one comment or
transition starts exactly one item.

**Trade-off (accepted):** polling reacts on the next tick (minute-scale
latency), not instantly — fine for coding automations. **Webhook ingress**
(instant push) is **deferred**, not deleted. Revisit only if a real
sub-minute-latency need appears.

## 12. Reliability & lifecycle controls

The controls that keep autonomous, unattended runs from silently wedging,
running away, or losing work. They are **one mechanism plus a few triggers**,
not a pile of features.

### The teardown contract (one path, many triggers)

There is **one teardown path**, and everything that stops a phase runs it:

> **stop the turn → release the phase's resource locks (§6) → write a partial
> envelope → route to a terminal / needs-human state with a typed reason.**

Triggers that invoke it: **crash-restart**, **pause**, **cancel**, **watchdog
trip**, **transient-retry exhaustion**, **budget exceeded**, **discard**.
Specifying it *once* is the point — separate release paths would each risk
leaking the `live-stack` mutex, which would deadlock every future item that
needs the stack. One path, tested once, makes every trigger correct for free.
Teardown is **tree-aware**: tearing down an item tears down its in-flight
fan-out units and recursively its child runs (§3a), so a stopped root can
never leave a grandchild running or a sub-worktree stranded.

### The triggers

- **Crash recovery (park, don't auto-resume).** A desktop app gets killed
  mid-run (sleep, quit, OS update, crash). On startup, any item in `running`
  whose current phase has **no terminal envelope** runs teardown and **parks
  `needs-human(interrupted)`** — across the whole run tree, children and units
  included, so the tree rests consistently. The system does **not**
  auto-re-run — an unattended double-execute is a worse failure than a paused
  item. Resume (§7) continues on the same provider thread, whose session file
  survived the crash. (The run record (§10) is the recovery journal; no
  checkpointing, no turn-replay.)

- **Pause (human or shutdown).** The §7 pause action, and the **graceful-quit
  path**: quitting the app pauses every active run (interrupt in-flight turns
  → teardown → park `needs-human(paused)`) before exit. Same resume as
  `interrupted`; the distinct reason tells you whether it stopped on purpose.

- **Inactivity watchdog.** §2 names "genuinely stuck," but a headless turn can
  hang silently. A phase whose **active turn emits no stream event for T** (a
  project-profile default, per-phase overridable) runs teardown and parks
  `needs-human(stalled)`. It is **not a wall-clock cap** — agent turns
  legitimately run minutes to hours; a duration cap is either uselessly loose
  or kills good work. It watches the stream the phase already emits; no
  heartbeat protocol. (A phase *waiting on a resource* is not "stalled" — that
  wait is bounded by the holder's guaranteed teardown release, §6.)

- **Cancel (the kill button).** A human stops a running item: kill the
  subprocess(es) → teardown → terminal **`cancelled`** (distinct from
  `failed`). A core desktop affordance for "this is running away" or "wrong
  goal." The worktree persists like any terminal state (§9); discard (§7) is
  the separate, previewed cleanup step.

- **Transient-execution retry.** A phase that **fails to produce an envelope**
  due to a **conservative allowlist** of transient errors (subprocess exit,
  known provider-overload responses, network errors) retries with **backoff,
  cap ~3**, then parks `needs-human(retries-exhausted)`. Anything **not** on
  the allowlist parks immediately — never retried. This is the no-feedback
  sibling of §4's feedback-carrying loop-back: a 529 carries no validation
  signal, it just waits and re-runs.
  **Distinct from this allowlist:** an envelope **absent or invalid after §3
  engine post-validation** gets **feedback-carrying retry turns** ("your
  envelope failed validation: <errors>") up to a **profile-set count (default
  1)**, then parks `needs-human(agent-error)` with the partial envelope — an
  agent-quality failure, not a transient; never backoff-retried. (The count is
  a profile knob because a fixed one-shot leaves no margin at high volume; the
  branch rules being stated in the prompt (§3) is what makes the default
  affordable.)

- **Per-item budget.** One **optional** ceiling per item — **tokens / $** if
  the provider reports spend, else **wall-clock** — checked at phase
  boundaries and enforced against the **root** item across its whole call
  tree (§3a). On exceed: teardown → `needs-human(budget-exhausted)` with
  spend-so-far. Headless full-access is exactly the config that runs away,
  and a solo dev pays per token. This is the *single* runaway ceiling; it
  subsumes a per-phase turn cap, so there is deliberately **not** a second
  per-phase knob.

### What this is NOT (kept out on purpose)

No Temporal-style deterministic replay or workflow patching (impossible for
non-deterministic agent turns — record the envelope, never replay the turn).
No heartbeat-RPC subsystem (a local watchdog over the existing stream is
enough). No dead-letter queue (the `failed` / parked states are it), no
backfill, no quiet-hours / escalation engine, no per-call approval prompts
(worktree isolation + enforced access modes + nothing-auto-merges is the
safety model; reserve approval for irreversible *external* effects like
`git push` via a tiny per-tool flag). The `running` state is simply **always
exitable**.

---

## Deliberately out of scope

- **Self-improving workflows / a memory subsystem.** Considered and
  **excluded** — not core to the product. AI-managed memory is historically
  junk (transient noise, hallucinated facts), and the durable knowledge that
  matters already lives in the repo (code, project profile, `CLAUDE.md`-style
  rules, ADRs) which phases read anyway. A "watcher" that proposes workflow
  tweaks is, if ever wanted, just a later §11 automation built on existing
  primitives — not a subsystem. Rationale and the research behind this call:
  `workflows-system-self-improvement-research.md`.
- **Agent-invoked inter-agent discussions** — post-v1 (§5). Human-initiated
  discussion flows are unaffected.
- **A work queue.** Removed in rev 2 (was rev 1's core). If a future need
  genuinely wants admission control beyond resource capacity (§6), it returns
  as a *caller* of the one start path — never as a second execution model.
- **Remote mutation of workflows** — remote browsers get **view-only**
  workflows, consistent with AO's existing remote posture. Remote
  gate-approval is a possible later relaxation.
- **Webhook ingress** — stays deferred (§11).
