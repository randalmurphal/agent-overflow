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
> — and **failed → running** on a rerun

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
  (vs needs-human, which is recoverable). Unlike `done` and `cancelled` it is
  **not terminal**: a **rerun** re-enters the failed phase and takes the item
  back to **running**. That is the only edge out of `failed` — a finished run is
  re-entered by starting a new one, and a cancelled run was stopped on purpose.
- **cancelled** — a **human chose to stop it** (the §12 kill button). Kept
  distinct from `failed` (work failed) so the two read differently on the
  surface (§10).

There is deliberately **no `queued` state**: a run starts running. Contention
is expressed as a *phase* waiting on resource capacity (§6), never as an item
waiting in a line.

**Every `failed` / `needs-human` / `cancelled` transition carries a typed
reason** (gate, question, stuck, stalled, paused, interrupted, checkpoint,
budget-exhausted, provider-retries-exhausted, loop-limit-exhausted,
check-failed-genuine, agent-error,
wiring-error, disposition, setup-failed, unit-failed) — recorded in the run
record (§10) and shown on the run's row, so a stopped item is never a silent
dead end. `paused` (a human or graceful shutdown chose to stop it, §12),
`interrupted` (the app died out from under it, §12), and `checkpoint` (it
reached the call boundary it was asked to stop at, §12) are distinct reasons
with the identical resume path, so the morning-after view tells you *why* it
stopped without changing *how* you continue it. `checkpoint` is the only one of
the three that is not a fault at all — the run did exactly what it was told.
`retries-exhausted` remains readable only for runs persisted before provider
retry exhaustion and workflow loop exhaustion received distinct reasons.

## 3. Phases and the variable system

A **phase** is a configurable unit of work — not merely "an agent." A phase
declares:

- a **driver**: **agent** (provider + model — Claude or Codex, per phase) or
  **tool** (a profile-bound deterministic command — build, test, lint, merge,
  provision). Deterministic work is a first-class phase, not "an agent
  babysitting a script." **Both drivers are in scope; neither is optional.**
- an optional **`effort:`** on an agent turn — the reasoning tier, from the
  closed none/minimal/low/medium/high/xhigh/max/ultra set. Unset means the
  model's catalog default. It is legal exactly where `provider:`/`model:` are
  (an agent phase, an agent fan-out unit, an agent join) and a validation error
  anywhere else. The *name* is validated statically; whether a **given model**
  advertises that tier is not — the model catalog is provider-owned and partly
  live — so an unsupported tier is coerced onto the model's own default when
  the turn's thread is created.
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
own exit status while it is still running. **The overlay applies to a written
`status: done` envelope only** — a `question` or `stuck` envelope carries no
outputs at all (§4's branch rules), so there is nothing to stamp them onto and
they pass through as written. **A non-zero exit is `passed:
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

**`history.<phase>` — the loop's memory (D51).** Ordinary references carry
only a phase's *latest completed* envelope, which makes every loop round
blind to the rounds before it — the live campaign's review↔fix loop
oscillated for 14 rounds because each round was memoryless. A phase (or its
units and join) may declare the reserved input `history.<phaseID>` (schema
exactly `{type: array}`, optional `window: N` — default 10, hard cap 50) and
receive that phase's prior attempts oldest-first, excluding the attempt now
running: completed attempts as `{attempt, status, outputs}`, non-completed
ones as honest stubs (`{attempt, status, envelopeStatus?, reason?,
question?}` — the one place a non-completed attempt is visible to prompts;
ordinary references keep their completed-only rule). The rendered series is
byte-budgeted with explicit whole-entry elision, never silent truncation.
It is a prompt surface only — gates and workflow outputs cannot reference
it. `history` is reserved as a phase id and workflow-input name.

**`call-depth` — the wave ordinal nobody computes (D65).** The reserved
read-only binding `call-depth` is the current run's depth in the call tree
(root = 0), bound by the engine from its own counter — the live campaign
threaded a wave-number seed through self-call args and incremented it with
model arithmetic, which desynced. Reserved at every declaration site
(workflow input, phase input, fan-out `as:`, and phase **id** — a phase named
`call-depth` would have its output object silently shadowed), and bound after
seeds so a seed carrying the name cannot override the engine's answer. It is
not the same fact as an authored wave number: a campaign restarted as a fresh
root has depth 0 while its wave numbering continues — both exist on purpose.

**Phase inputs inherit workflow input schemas (D68).** A phase (or unit,
join, call-arg) input bound to a declared workflow input by its bare name,
declaring no schema of its own, inherits the workflow input's schema whole at
parse time — frozen with the snapshot like everything else. An explicit
schema wins and is deliberately not compatibility-checked (narrowing is the
point). ~40% of the live campaign's YAML was re-typed schemas; the only new
refusal is none.

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
  **own driver/provider/model/effort/access** (mixed
  Claude/Codex fan-outs are a feature, not an accident) — and only the units
  and the join do: **a fan-out phase runs no work of its own, so declaring a
  driver, provider, model, effort, prompt, command, or access on the phase is a
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
    provider/model/effort/prompt, command, access, and outputs are all the
    child workflow's to declare. A **join may not be a call**: its envelope IS the
    phase's, and every phase-level continuation (an answer, a takeover
    finalize, a resume in place) is a continuation of the join's own session;
    fan out to a call unit instead.
  - the **join** is an ordinary unit (agent or tool) that runs after all units
    rest; its envelope is the phase's envelope. What a join *does* — synthesis
    or merge — is the author's choice (§9). A join that **fails is a unit
    failure** (D48): the attempt parks `needs-human(unit-failed)` with the
    join in the failed set, and the retry verbs re-run the join alone over
    the preserved unit results — the wave's finished work, including entire
    called child runs, is never the price of a failed merge. Dropping a join
    stays refused: it is what consolidates the units, so its absence cannot
    be accepted. A join may additionally declare **`accounts_for_units: true`
    (D64)**: its outputs must then include `merged` (unit-id array) and
    `blocked` (`{unit, reason}` array), and the engine refuses a `done`
    envelope whose merged ∪ blocked is not EXACTLY the unit set — a missing
    unit named, an unknown or duplicated one refused, a blank reason refused.
    The refusal is ordinary envelope-validation feedback (a retry that names
    the unaccounted units), never a park. This is the fix for the measured
    incident where a hand-written merge script's stop-at-first-conflict
    silently dropped an approved lane: the engine does not merge — the
    author's script still decides policy — but no lane can vanish from the
    accounting. The starter content carries a reference merge script that
    skips-and-continues on conflict and emits the contract.
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
  up a prompt the human edited while the previous wave ran. For a run parked
  mid-flight, the freeze has an explicit escape (D50): `run resume
  --refresh-def` / `run rerun --refresh-def` re-read the definition and its
  prompt files from disk at that fresh entry — the repair for "I edited the
  prompt of a parked phase". Refresh never happens implicitly, only ever at
  a fresh phase entry (a continuation keeps the definition its attempt was
  launched under), and it is refused when the entry phase is absent from the
  edited definition or the new workspace need is incompatible with what the
  run has.
- **Absence crosses the edge as absence.** Arguments evaluate where the
  resolved child is in scope: an arg whose reference does not resolve is
  **omitted** when the child input it seeds is declared `optional:` — the
  child sees the same absence a direct start without that seed would give it
  — and refuses (`wiring-error`, message naming `arg (ref)`) only when the
  input is required or undeclared. The refusal parks on a **persisted phase
  attempt row**, so it is diagnosable from run status (D45 — the first live
  campaign died at its own recursion point because an absent optional input
  was treated as a wiring failure, with no attempt row to say so).
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
  ("write to a file, return a path") — run records stay lean. Post-validation
  owes literal presence to **`status` alone**: an absent `outputs` /
  `question` / `reason` / `narrative` reads exactly as the null a
  schema-bound provider would have sent, so a hand-written tool envelope
  carries no null boilerplate — while a `done` envelope whose phase declares
  outputs is still refused per missing declared output, by name (D44). The
  generated schema is untouched: provider strict mode still requires every
  key, nullable.
- The **narrative** (what it did, reasoning, decisions) is written to a
  **file** in a system-dictated location; its path is **system-attached** to
  the run record, never an agent-filled envelope field. The system never
  parses it; humans and later phases read it on demand. **Who writes the file
  depends on the driver and the access**: a writing agent element writes it
  itself, a tool element gets its masked output tail written there, and a
  `read-only` agent element — which cannot write files at all (§9) — puts its
  account in the envelope's optional **`narrative` control field**, which the
  system lifts into the file. That field is the fifth control name alongside
  `status` / `outputs` / `question` / `reason`; it is **legal on every status**
  (a done, a question, and a stuck element all did work worth an account) and is
  **stripped before the envelope reaches the engine**, so no gate, join result,
  or persisted envelope ever carries prose. A separate narrative *message* is
  not an option: Codex applies a turn's `outputSchema` to every assistant
  message in the turn, so a schema-constrained element cannot emit prose at all.
  An element that supplies neither file nor field falls back to the session's
  final assistant text, marked as recovered (D39). A phase that produced nothing
  at any tier leaves no file, and every surface that points at one checks it
  exists first.

**Workflow-level `outputs:` — the run's deliverables.** A workflow may declare
named values and/or artifact files, sourced from phase outputs — distinct from
the narrative (a process log). An output may be **`optional: true`** (D67):
absent at completion, it is omitted from the result envelope exactly as an
absent optional call arg crosses a call edge (D45), never a failure — the
repair for the incident where a campaign died at the moment it completed,
because a declared output's producer never runs on the completion path. A
REQUIRED output whose producer is not on every path to `done` is a dry-run
finding (`workflow.output-unreachable`, naming the output, the phase, and one
witness path); a producer on every done path stays exactly as strict as
before. Artifact files are **copied into an app-managed
per-run artifact store at the producing phase's completion**, so deliverables
survive worktree discard (§9). Run detail lists them (§10); agents fetch them
through the §5 CLI; a bound origin thread receives them in the wake message
(§5). A call phase's envelope carries the child's workflow outputs.

**Campaign memory — the run tree shares one log (D57).** Every root run owns
an app-managed memory directory (`<configDir>/workflow-memory/<root-run-id>/`,
created lazily on first note) whose `notes.ndjson` is the append-only record
of what the tree learned: environment quirks, porting patterns, failure
modes, hand-offs to the next wave. A note is typed from a **closed kind set
`pattern | warning | learning | handoff`** — a bad kind is refused exactly
like a bad envelope status — with text and cited files bounded, and
**provenance (run / phase / attempt / unit / wave) stamped by the system,
structurally impossible to author**. Two write channels mirror the narrative
precedent: a write-capable element (or a human, with `--run`) uses
`agent-overflow memory add`; a read-only element — which cannot reach the CLI
at all — puts entries in the envelope's optional **`memory` control field**,
stripped at the same seam as `narrative` and written with system-stamped
provenance, so no gate or persisted envelope ever sees it. Every element's
prompt carries the memory **path** (readable in every access mode) and a
**bounded digest**: handoffs first, then everything else, newest-first,
grouped by kind, entries falling off whole under the budget, the header
stating `N of M notes` and naming the full log. Promotion is automatic —
recency and the budget are the curation; a **curator phase** that distills
raw notes is starter content, never an engine feature. Memory outlives the
run (a done campaign's memory is its record), survives discard, and is
deleted with the project's workflow records. Reading another tree's memory
is deliberately not a capability — a campaign's lessons are not project run
state.

**The goal chain — every element knows what it serves (D63).** A workflow may
declare **`non_goals:`** (bounded list, frozen with the snapshot) — the
author's "do not drift here" list, def-owned where goals are run-owned. Every
agent element's prompt (units and the join included — the join decides what
ships) opens with a bounded **goal-chain block**: the goals from the
campaign's root down to this run, root first, middle elided past six links
with the elision stating its size, consecutive identical goals collapsed to
one link (a call copies the caller's goal verbatim, and forty waves of one
sentence is noise, not context) — followed by this run's workflow's
non-goals, and the root workflow's when they differ. Every value is
untrusted-quoted and labelled as recorded data, not instruction. A bare run
with no goal and no ancestry gets no block: zero noise for the simple case.
The acceptance-criteria ledger is deliberately CONTENT, not engine: criteria
seed the root as a typed array input, each wave's plan emits a `coverage`
output (`{id, state: uncovered|covered|satisfied|regressed, evidence,
lane}`) forwarded through the self-call args exactly like the wave number —
dry-run-checkable today, demonstrated in the campaign starter.

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

**A loop bound is a literal or a reference.** `max: 2` is static; `max:
inputs.fix-budget` resolves against the run's variables at evaluation time, so
a campaign can seed retry budgets per run (`--seed fix-budget=4`) without
editing the definition. The resolved value must be an integer ≥ 1 — anything
else is a `wiring-error` park, never a silent default — and the gate trace
records the resolved bound, so a run's history says what budget it actually
ran under.

**Loop bounds are per fresh entry, not per item lifetime.** A loop edge's
counter counts consecutive traversals **since the loop's target phase was last
entered from outside the cycle**; entering the cycle fresh (a forward edge, a
call, a human re-run) resets it. Counters remain derived from the persisted
gate traces — nothing new is stored — and human-takeover attempts still don't
consume loop budget. (Rev 1 counted per item lifetime, which starves later
iterations of any retry budget inside outer loops; batch-scale iteration
belongs to call phases (§3a), whose child runs get fresh counters by
construction.) An exhausted bound falls through to the next route; exhausting
everything parks `needs-human(loop-limit-exhausted)`. A bare resume re-enters
the parked phase without refilling the cycle; `run resume --phase <earlier-id>`
enters the loop target from outside and restores its bound.

**Fall-through is the "on exhausted" hook — author it, don't wish for it.**
Because an exhausted loop route continues down the route list, the routes
*after* a loop are its exhaustion policy: they are reachable exactly when the
loop no longer fires (its predicate stopped matching, or its budget is spent).
A gate that parks on loop-limit-exhausted is a gate whose author declared no
cheaper exit. The pattern for severity-aware exhaustion — have the deciding
phase emit a routable output (e.g. `worst-severity`), then:

```yaml
gate:
  routes:
    - when: {gt: {ref: review.findings-count, value: 0}}
      loop: fix
      max: 2
    - when: {eq: {ref: review.worst-severity, value: minor}}
      to: confirm-minor        # reached only once the loop is spent
    - park: review-unresolved  # majors with no budget left → a human
```

**`park:` is undecidable on purpose; `human:` is the decidable form.** Both
rest under `needs-human(gate)`, but a `human:` route declares an approve
target and a reject loop — resolving it (D41) *completes* the parked attempt,
so its outputs stay in the variable context — while a `park:` route declares
no continuation at all: there is nothing an approve could select, `run
resolve` refuses it by construction, and the repair is `run resume` (a fresh
attempt of the phase) once the cause is addressed. Only *completed* attempts
feed later phases' variables, so resuming with `--phase` past a parked
attempt runs the target without the parked phase's outputs. If the final stop
of a route list should be decidable — "a person confirms, or sends it back" —
author it as a `human:` route, not a `park:`.

**`notify:` is the wake without the park (D54).** Any route that leaves the
run running — forward, loop-back — may carry `notify: true`: the gate routes
exactly as it would have, and the run's bound thread additionally receives a
**progress wake** (run, phase, route taken, bounded outputs digest). It is
the authorable form of "tell me at wave boundaries, don't stop" — a campaign
gate reads `blocking → human:`, everything else `→ next, notify: true`, and
the supervisor hears every wave without any run ever parking for it. On a
`human:` or `park:` route it is a static finding (the park already wakes; a
decoration there promises a second wake for one event); on a terminal route
it is a non-blocking report and fires nothing (the resting wake already goes
out). The gate trace records that it fired; delivery is best-effort and can
never park, fail, or delay the run. Called runs' notifies compose as the
**root's** progress wake naming the descendant; an unbound root has no
progress surface and the decoration is inert.

**A loop route owns its round's temperature and its question (D60/D61).** Two
knobs, legal on `loop:` routes only (and only where the target runs one
session of its own — refused statically against tool, call, and fan-out
targets):

- **`session: continue | fresh`** — `fresh` (the default, unchanged) re-enters
  cold; `continue` runs the new logical round **on the target phase's most
  recent provider thread**. The new round still receives its complete resolved
  phase prompt (including a route `prompt:` override), because the earlier
  thread contains a different round's task. Only recovery of the same parked
  round uses the short continuation message described in D70. Anti-anchoring
  stays the default on purpose: review edges re-enter cold, and starters set
  `continue` on the fix edge, where losing "what I just tried" is the
  measured ping-pong cost. If the prior thread is gone (crash, deletion),
  the round runs cold with a recorded note. If the cursor exists but provider
  preflight rejects it, the unsent warm attempt is superseded and the same new
  round is reconstructed cold, preserving its complete prompt, route override,
  guidance, and degradation note — a degraded reuse is never a park.
  Which mode actually ran is visible in provenance: two
  attempts sharing a thread id *is* the record, rendered as
  `session=continued` on `run status`.
- **`prompt: <file.md>`** — the re-entered attempt renders this file instead
  of the phase's own prompt. "Later rounds ask a narrower question" becomes
  authorable: a convergence loop's round-3+ edge carries a "only blocking or
  material findings extend this loop" prompt. Resolved, template-checked
  against the target's inputs, and frozen with the snapshot exactly like
  phase prompts; route-scoped, never sticky — a later loop without `prompt:`
  gets the phase's own prompt back. System suffixes (envelope contract,
  memory digest) still apply; the override replaces the authored body only.

**A spent reject budget refuses the reject; it never converts the park.** A
`human:` route's reject loop carries a bound like any loop; once it is spent
the gate still declares an approve, so a further reject is **refused** with
the live options named — approve, `run resume --phase <target>` (entering the
loop's target from outside is a fresh entry that refills its bound), or
cancel — and the run stays parked exactly as it was (D47; converting the park
to `retries-exhausted`, as rev 1 did, destroyed the still-declared approve).
A human-gate park likewise refuses a bare resume and a resume at the parked
phase — the decision belongs to `run resolve` — while naming a *different*
phase is allowed: that is the human abandoning the gate to redo earlier work.

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
new` (authoring) · `run | status | inspect | narrative | result | list-runs |
watch` (execution — `watch` blocks SERVER-side and prints each transition as
it happens, `--tree` widening to called runs; it exists because the
alternative was 712 hand-rolled polls and one monitor that died silently, so
its exit codes distinguish "the run rested" from "my timeout expired" from "I
was cut off", D58) · `pause |
resume | cancel | rerun | retry-unit | retry-failed-units | soft-stop |
amend | guide` (control — `amend --seed k=v` is `--refresh-def`'s counterpart
for inputs, D59: durable on a resting run, validated per key by the intake
validator, its output stating when the run reads the change; `guide` is §7's
pending-guidance slot, D62) ·
`memory add | list` (the campaign memory log, §3/D57 — writing memory is part
of doing the work, so a phase's scoped token admits it without a declared
grant; authority is row-level, its own tree only). `run`
starts a run
immediately and returns the run id; it does not block. **The read surface is a
CLI obligation (D52):** everything an operator needs to decide a park —
worktree and branch, seeds, children, per-attempt provenance and envelope
output digests (`run inspect`, with `--phase` drill-down), the engine's park
cause (D53, bounded on attempt lines, whole under `--phase`), and the
narrative prose (`run narrative`) — is one verb away, because the live
campaign's supervisor otherwise answered those questions with raw SQL against
the app's own database and hand-built file paths.

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
  holding it is not the one a human is watching. The wake carries what acting on
  it needs: the **call chain** from the root down to the parked run (elided in
  the middle past six runs, with the elision stating its own size), the parked
  run's **own** failed units labelled as such, and a closing that names which
  run to act on **and the literal command that acts on it** (D38). A campaign's
  sixth wave is a run the reader has never seen, so the message has to be enough
  to issue `agent-overflow run retry-failed-units <child-run-id>` without a
  second command to work out the tree (D36a).

If a bound thread has been deleted, the run falls back to the unbound surface
— a wake is never silently lost.

**A wake says something new or it does not go out (D55).** Wakes are
deduplicated by **content signature** — run, resting state, typed reason,
phase and attempt, question text, engine cause, and the same again for a
parked descendant — never by a time window. A wake matching the last one
delivered, with nothing having happened on the run since, is suppressed with
a durable log line; any field differing delivers. The signature of the last
delivered wake persists on the run row (v52) so a restart's crash-rebuild
re-park does not re-send the message a supervisor already read, and it clears
whenever any member of the run tree returns to `running` — which is what
every resolve, answer, resume, retry, and rerun does, so acting on a run
re-arms its wake. One delivery seam enforces this; no composer bypasses it.
Progress wakes (D54) coalesce under the same rule by their own signature,
which includes the attempt — a decorated loop-back reports every wave, not
just the first. When in doubt (a signature read fails), the wake delivers:
a duplicate is noise, a silent drop is an outage.

**The wake body is sized to act on, not to drill into (D56).** Beyond the
digest, references, and engine cause, a wake carries the run's worktree path
and branch (a descendant's only when they differ from the root's), and a
needs-human gate park carries the parked attempt's **outputs digest** —
bounded by the same helper `run inspect` uses, never a second bounding — with
the overflow line naming the exact `run inspect` drill-down command. The
question bound is sized for real gate questions (2000 runes; 800 truncated
live-campaign questions mid-sentence). The read verbs (D52) stay the
drill-down; the wake stays compact.

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
  a human pointed at the workflow files);
- **resolve** — decide the parks a run this phase started rests on:
  approve/reject a human gate, answer a question. Separate from start-run on
  purpose: starting and stopping work is routine, while answering a decision
  the workflow author routed to a human is authority an author hands out
  deliberately. Interactive sessions (a human-driven thread, where every
  invocation passes the provider's own approval UX) hold it implicitly.

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
- **A fan-out unit may declare `resources:` of its own**, acquired for that
  unit's lifetime through the same project semaphores — the case is a
  command unit inside a wave that needs environment capacity (e.g. one
  `container-slot` per gate-check). Phase-declared resources stay
  phase-scoped (taken once by the attempt, not once per unit); unit-declared
  ones are per unit, on top of the agent unit's implicit provider slot.
  Call units declare none — the child workflow's phases declare what they
  need.
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
pause. If that provider context has disappeared, the unsent continuation is
superseded and the same parked round is reconstructed on a new thread with its
complete original prompt, delivered guidance, route override, amended inputs,
and an explicit context-loss note. Pausing a run with active fan-out units interrupts every in-flight
unit; each unit resumes on its own thread.

**Stopping after this wave is the deferred half of pause (§12, D36).** Where
pause stops the run *now*, `Stop after this wave` asks the tree to stop at its
next call boundary: nothing in flight is interrupted, the wave that is running
finishes, and the run parks `needs-human(checkpoint)` instead of invoking the
next one. It is the affordance for a long campaign a human wants to end
cleanly rather than cut short, and **Resume** takes the call it skipped. The
request is one piece of state — arming and withdrawing both go through one
verb — and the boundary that fires consumes it, so the resume does not stop
again.

**Guidance steers without parking (D62).** `run guide <run-id> "<text>"`
appends to a run's **pending-guidance slot** — the mirror of `notify:` (that
is run→thread; this is thread→run). Entries wait for the run's next **fresh
phase entry**, render there as a labelled, bounded, untrusted-quoted block,
clear on delivery (the attempt row persists first, so a crash in the gap
**redelivers rather than loses** — a lost instruction is the worse failure),
and leave a feedback note on the attempt that ran with them. Delivery is
only ever at that boundary: continuation resumes leave the slot pending
(and say so — `guidance-pending=N` rides the run's status line), phases that
render no prompt are skipped with entries retained, and **nothing is ever
injected into an in-flight turn** — the mid-turn half of correction remains
explicitly deferred. A called run may be guided directly (its row, its
prompts — same ownership truth as `run amend`). Entries are system-stamped
with author and time; a phase's guidance names the run that wrote it.

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
the §12 contract — settling **parked** tree members too, now that cancel
reaches them (D46; only a `disposition` park is skipped, since its running
disposition settles it) — and removes run-created branches whose commits never
landed. The preview is the consent; nothing is cleaned silently.

### needs-human — when the phase yields on its own

A phase parks at **needs-human** because of a **gate**, a **question**, being
**genuinely stuck**, or a **failed unit** (§3). `question` and `stuck` are
**envelope statuses** (§3) — the turn ends cleanly with that shape; nothing is
suspended provider-side. It surfaces (overlay + notification, §10) with its
**blocking reason**, a short **digest**, and the **diff / checks / cost /
narrative** — never raw internals (below). A park the **engine** decided —
a worktree that would not cut, a wiring error, a budget breach — additionally
carries the engine's own diagnosis on the parked attempt row (D53): the wake
renders it as one bounded line, run detail and `run status` / `run inspect`
show it, and it is never written into the envelope, which stays the agent's
artifact. If the run is thread-bound (§5),
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
- **Unit recovery** — for `unit-failed` parks: retry the unit (the join
  included, D48), retry every failed unit of the attempt at once, drop it
  (join proceeds over survivors; never the join itself), or open its thread.
  One rule governs the resume verbs here (D48): **`run resume` continues and
  preserves** — on a `unit-failed` park it reopens the failed units and the
  join over the attempt's finished work, exactly as the retry verbs do —
  while **`run resume --phase <id>` (which may name the parked phase) is the
  one explicit "start over"**: a fresh attempt, the wave re-expanded, called
  child runs respawned. Discarding finished work always requires saying so.
  The whole-attempt repair (D33) is the usage-limit case:
  one cause fails most of a wide fan-out, the human waits the limit out (or
  switches account) and repairs all of it with one action — the same edge, the
  same reopened attempt, and the same admission through the project's
  semaphores as repairing each unit in turn, so a wide repair queues instead of
  bursting. Units under human steering are left alone.

| Phase turn state | Sending a message → |
|---|---|
| In-flight (running) | interrupt → yields → steer free-form → **Complete** (finalize turn re-adds schema) or discard + re-run |
| Parked on a `question` envelope | answer runs the next turn, same session, same schema → envelope |
| Parked `paused` / `interrupted` / `provider-retries-exhausted` | resume continues the parked round: short message on the same provider context; full reconstructed prompt on a new thread when that context is unavailable (D70) |

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
the **reason→verb repair map** (D38; since D41 that map includes settling a
gate or question via `run resolve` / `run answer`, under the `resolve` grant
in a phase session), and links to the project's active runs. Everything deeper is
discovered via `--help` and `agent-overflow workflow schema`. This replaces rev 1's chat-enqueue MCP
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
per-item budget), **secret references**, and the **disposition policy**
(`manual` | `auto-pr` | `auto-merge`, default `manual` — §9).

The **worktree setup** step — files copied from the main workspace and commands
run at worktree creation (`.env`, dependency install) — is NOT part of the
profile. It is per-project **app settings** (Settings → Projects), persisted on
the project row, so chat-thread worktrees run the same recipe workflow
worktrees do. Each command runs in the new worktree with **`AO_PROJECT_ROOT`**
and **`AO_WORKTREE_PATH`** exported as absolute paths, so a recipe can link back
to the main checkout instead of only snapshotting it, and **setup failure parks
`needs-human(setup-failed)`** before any phase starts on a broken tree. See
`internal/worktreesetup/AGENTS.md`.

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

**`access` is enforced, not advisory — for agent work.** A phase's / unit's
access declaration maps to the provider session's runtime mode: `read-only` runs
sandboxed (no file writes, no destructive commands — the provider's restricted
mode); `write` runs with full access **in its own isolated workspace**. A
read-only agent phase on the project root physically cannot dirty it. (Rev 1
derived worktree need from `access` but never enforced the mode at the session —
closed in rev 2; unattended parallel writers make this non-negotiable.)

**The tool driver is the carve-out, and it is deliberate.** A `driver: tool`
phase or command unit has no provider session to configure a runtime mode on: it
is a raw process, started in the phase's workspace with the project profile's
resolved secrets and no sandbox of any kind. Its `access` declares what the
system *provisions* for it — a sub-worktree for a writing unit — never what it is
prevented from doing. What bounds it is the project profile: the argv comes from
a profile binding the user wrote, never from the model, so a tool phase can do
exactly what its owner already put in `checks:` / `commands:`. A read-only
declaration on a command that writes is therefore a mis-declaration, not a
refusal.

One consequence worth stating: `access: read-only` on an AGENT element also means
that element cannot write its own **narrative** file. It is asked for the
narrative in its envelope's `narrative` control field instead (§3), and the
system writes the file from it; an element that supplies neither falls back to
D39 recovery.

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
indifferent to. The branch name carries the whole provisioning coordinate —
owning item, phase, phase attempt, unit, try — so a retried unit's next child
cuts fresh instead of inheriting what the failed try left behind, and two
fan-outs that share one item branch (successive waves of a self-call campaign,
a re-expanded phase) can never derive each other's names.

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

When the trigger is the **engine's own decision** (setup failure, wiring
error, budget breach — not an agent outcome or a human verb), teardown also
persists the engine's diagnosis as the attempt's **park cause** (D53): a
separate column from the envelope, created together with the attempt row if
the park fired before one existed, bounded, and surfaced through §7's wake
and §5's read verbs. The same events land in the **engine log**
(`logs/engine-YYYY-MM-DD.ndjson`, D53a) — always on, because a run parks
once.

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

- **Soft stop ("stop after this wave").** A standing request set on a **root**
  run and honoured by its whole tree: at its next **call boundary** — the moment
  before a `shape: call` phase would invoke the next run — the run parks
  `needs-human(checkpoint)` instead of calling. Nothing in flight is
  interrupted and nothing is spent; the wave that was running finished. Resume
  takes the call it skipped, so a campaign continues exactly where it stopped.
  The firing boundary **consumes** the request, so the resume does not stop
  again. It is refused on a called run (a tree is stopped as a tree, like
  pause) and on a run that is not running (there is no next boundary to reach);
  withdrawal is legal in every state. A workflow with no call edge accepts the
  request and simply never fires it. Deliberately NOT checked at a fan-out
  unit's call edge: a unit call is work *inside* a wave, and stopping there
  would strand the siblings its join is waiting for. (D36)

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
  `failed`). A **parked** item is cancellable too (D46): `needs-human →
  cancelled` is a legal edge, and since a parked run holds no processes and
  no resources the cancel is pure bookkeeping — the parked attempt row stays
  untouched as the record of why a human was asked, resting call children
  settle with the tree, and a parent whose called child was cancelled
  observes it through the normal child-settlement path. A core desktop
  affordance for "this is running away" or "wrong goal." The worktree
  persists like any terminal state (§9); discard (§7) is the separate,
  previewed cleanup step.

- **Transient-execution retry.** A phase that **fails to produce an envelope**
  due to a **conservative allowlist** of transient errors (subprocess exit,
  known provider-overload responses, network errors) retries with **backoff,
  cap ~3** — each retry re-sending into the SAME live session — then parks
  `needs-human(provider-retries-exhausted)`. Anything **not** on the allowlist parks
  immediately (`agent-error`) — never retried. This is the no-feedback
  sibling of §4's feedback-carrying loop-back: a 529 carries no validation
  signal, it just waits and re-runs.
  **The park is continuable (D70).** A bare `run resume` continues the parked
  attempt on its own provider session with a continue message — the same
  contract as `paused`/`interrupted`, because an API-error death is the same
  shape: the turn stopped through no fault of the work with the session file
  intact, and the transient layer was already continuing that session between
  retries. A turn that ran for an hour before the provider fell over costs a
  resume, not a re-run. The dead-session fallback reconstructs that same round
  in a new thread with its full original prompt and a context-loss note;
  and `--phase <id>` as the explicit start-over apply as for every continuable
  reason; `--refresh-def` is refused on the bare resume there like any
  continuable park, since the attempt being continued rendered the frozen
  definition.
  **A usage-limit refusal skips the ladder and schedules its own return
  (D71).** When the typed refusal is a provider quota error AND the session
  reported the limit windows, retrying in seconds against a limit that resets
  in hours is waste: the run parks `provider-retries-exhausted` immediately, the park
  cause states the reset moment, and a persisted `auto_resume_at` fires a
  bare resume (the same continuation) at that moment plus jitter — surviving
  app restarts via a boot sweep. Any manual action on the park disarms it.
  `run resume --at <time>` arms the same mechanism by hand on any continuable
  park. A refusal missing either half (no typed enum, or windows the session
  never reported) takes the ordinary ladder unchanged.
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
  **The spend is truthful across providers, and one number serves every
  surface (D69).** Rows that report tokens but no dollars (every Codex turn)
  are priced at read time through the app's one rate table, so a USD ceiling
  is enforced against wire cost *plus* estimates rather than silently
  ignoring ~70% of real spend; the spend says how much of itself is
  estimated and how many rows even that could not price. A token ceiling
  ignores both caveats — a token count is exact — while a USD ceiling the
  tree has not provably crossed refuses to be judged on missing rows (they
  can only move the total up, and an unevaluable ceiling must not read as
  headroom); a breach already proven by the priced lower bound parks the
  run for its budget, the truthful reason. Display and enforcement resolve
  through the SAME call: `run status`/`run inspect` render
  `budget=<spent>/<ceiling> (<n>%)` with the estimated flag and unpriced
  count, the budget-exhausted wake carries the composed number, and the
  reserved read-only **`budget` prompt binding** ({kind, ceiling, spent,
  remaining, estimated} in the ceiling's own units, unbound when no ceiling
  exists) lets an element say what it is nearly out of instead of
  discovering the ceiling by being parked at it — prompt surface only,
  refused in predicates, never writable.

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
