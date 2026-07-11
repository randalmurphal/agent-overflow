# Workflows System — Specification (Agent Overflow)

> Status: **Settled** — planning complete 2026-07-11; implementation decisions +
> amendments live in `workflows-system-decisions.md`; the UI is specified in
> `workflows-system-ui/UI-SPEC.md`.
> This is a WHAT spec (behavior / intent), not a HOW spec. Implementation
> notes appear only where they constrain the what; mechanics (wiring, schema
> DDL, token scopes) stay in the decisions log.
>
> Lineage: based on `orc`'s workflow/phase model where it fits; executes on
> Agent Overflow's existing thread/provider runtime.

## 1. Purpose

A system that drives a user-curated, prioritized **queue** of work items, each
through a **defined multi-phase workflow**, autonomously to completion —
pausing only at explicit gates that need a human.

- The **queue is the heart**: the user fills it, prioritizes it, and it drains.
  An individual item is just a queue of n=1; there is no separate one-off
  execution path.
- **Positioning: workflows are background jobs, not the center of the app.**
  Most work stays in normal threads (human discusses, agent works /
  orchestrates). Workflows cover **scheduled/triggered automations and custom
  tasks** — research runs, backlog triage, report generation, unattended
  build-and-validate. The surface (§10) is **deliberately secondary and
  quiet**: sized as a jobs system, one clear "needs you" signal, never a
  takeover.

## 2. Work item lifecycle

An item moves through a small set of states:

> **queued → running → needs-human → (back to running) → done**, or **failed** / **cancelled**

- **queued** — accepted into the queue, not yet started.
- **running** — actively being driven through its workflow. **`running` is always
  exitable** — no wait (a resource, an agent turn, an external poll) is unbounded; §12
  defines the watchdog, cancel, and crash-recovery paths that get an item out of it.
- **needs-human** — paused for a person: a gate, a question, genuinely stuck, or a §12
  control tripped. Resumes to **running** once the human responds.
- **done** — workflow completed to its terminal success.
- **failed** — stopped without success and not resolvable by a human handoff (vs
  needs-human, which is recoverable).
- **cancelled** — a **human chose to stop it** (the §12 kill button). Kept distinct from
  `failed` (work failed) so the two read differently on the surface (§10).

**Every `failed` / `needs-human` / `cancelled` transition carries a typed reason** (gate,
question, stuck, stalled, budget-exhausted, retries-exhausted, check-failed-genuine,
agent-error, wiring-error, disposition, setup-failed, interrupted) — recorded in the run
record (§10) and shown on the run's row, so a stopped item is never a silent dead end.

## 3. Phases and the variable system

A **phase** is a configurable unit of work — not merely "an agent." A phase
declares:

- **provider + model** (Claude or Codex, a specific model),
- **tools / MCP** available to it (optional, per phase),
- a **prompt** assembled from a template with variables injected,
- optionally a non-agent **script** step.

(orc is the proof-of-concept floor; any available functionality is allowed.)

**Variable system — the data backbone.** A workflow carries a variable context:

- the **workflow start** seeds it from the item's inputs (goal, ticket info,
  project config / tools, …);
- **each phase** reads variables into its prompt/script and **emits new variables**;
- **gates** read variables to decide.

Phases and gates are intertwined: **a phase's output is what drives its gate**, and
that output also becomes the variables the next phase consumes.

**Phase kind + shape.** A phase honors one I/O contract no matter how it runs.

- *Driver:* **agent-driven** or **tool/script-driven** — deterministic work (build,
  test, lint, provision) needs no model and is a first-class phase, not "an agent
  babysitting a script."
- *Shape:* one of —
  - **single** unit;
  - **fan-out/fan-in** — N parallel units → a join that consolidates their outputs
    into the phase's envelope (e.g. multi-lens review: N reviewers → one fixer);
  - **sub-workflow** — the phase runs a nested workflow; its result becomes the
    phase's envelope. Nesting is allowed but bounded. **Deferred post-v1**: the
    concept stands and nothing in v1 forecloses it, but v1 ships single and
    fan-out/fan-in only.

  ("Long-running" is **not** a shape: a normal phase runs until it emits *done*,
  however long that takes — even waiting on an external event is just a tool phase
  that polls.)

**Sub-workflows are inline composition only** (post-v1, above) — a reusable nested
workflow that runs as part of the phase (same work-item; worktree per §9 — its own
sub-worktree when it is a parallel writer), may run in parallel with other units in a
fan-out, and flows its result up when complete. **Decomposition is not a phase
shape**: spawning independent work is an agent *action* via a granted capability (§5),
not workflow structure.

**Phase output — control envelope + narrative file.**

- Every phase emits a **structured control envelope** (system-owned). Its schema is
  **generated, never hand-written**, and **flat**: system control fields at the top
  level — a **closed status set `done` | `question` | `stuck`** (§7) — with the
  phase's declared **output variables nested under `outputs`** (no collision with
  control fields by construction). Discriminated-union schema shapes are
  **impossible cross-provider** (D2a), so branch requirements ("question text
  required when status is question") are enforced by the engine, not the schema.
  This is the typed channel — it becomes downstream variables and gate inputs.
- **Engine post-validation is the sole success authority.** The engine re-validates
  every envelope against the full schema regardless of what the provider claims
  (§10), and **envelope size caps are engine-enforced** ("write to a file, return a
  path") — run records stay lean.
- The **narrative** (what it did, reasoning, decisions) is written to a **file** in a
  system-dictated location; its path is **system-attached** to the run record, never
  an agent-filled envelope field. The system never parses it; humans and later
  phases read it on demand.

**Workflow-level `outputs:` — the run's deliverables.** A workflow may declare named
values and/or artifact files, sourced from phase outputs — distinct from the narrative
(a process log). Artifact files are **copied into an app-managed per-run artifact
store at the producing phase's completion**, so deliverables survive worktree discard
(§9). Run detail lists them (§10); agents fetch them through the §5 CLI, and
enqueue-with-post-back delivers them to the requesting thread (§5).

**Typed, validatable variables.** Variables are typed as **JSON Schema fragments**
(string/number/boolean/enum/array/object — no custom type system); most required, some
optional.
A **dry-run validates the whole workflow before it runs** — and not only type-wiring. It
statically checks: every required input has an upstream producer and the types line up;
**every gate outcome names an existing phase; every phase is reachable from start; every
loop-back declares its §4 bound; every referenced resource is bindable by ≥1 project
profile; every variable reference resolves.** Each failure **names the offending element**
("gate G routes to phase X, which does not exist"), so a miswired workflow fails fast with
a precise message instead of mid-run.

**Validation principle (anti-theater).** Deterministic project checks play two roles:
(a) the agent is **given them to validate its own changes as it works**, before it
declares a phase done; and (b) a **deterministic gate** runs them as the hard
backstop. The goal is *"give the agent what it needs to prove its change is sound,"*
**not** *"make the tests pass."* The system must never reward gaming a gate over
genuinely validating the change.

## 4. Gates and transitions

After a phase completes, a **gate** decides what happens next. A gate is a **pure
routing decision** — it reads variables (including the phase's output) and picks an
outcome; it does no work of its own. All *work* lives in phases.

**Shape: an ordered route list on the phase, first match wins.** There are no
free-floating gate nodes — a gate is its phase's exit routing. Predicates are a
**small structured set** (comparisons, membership, existence, and/or/not combinators)
evaluated in order with short-circuit; string-shape decisions belong in a phase, not
a predicate. **A runtime no-match** (list exhausted, nothing matched) parks
`needs-human(wiring-error)` with the evaluated predicate trace persisted — never a
crash, never a silent advance.

**Outcomes:** **advance** (to a named next phase) · **loop back** (carries feedback;
**every loop route declares a mandatory bound**, counted per gate-edge and persisted
in the run record — an exhausted bound falls through to the next route, and
exhausting everything parks needs-human) · **park** (needs-human).

**What a gate reads to decide:**

- a **deterministic check's** result — a tool/script phase (tests, lint, build):
  pass/fail + variables,
- an **ai judgment** — an agent phase that assesses readiness,
- a **human** decision — approve/reject (**a rejection carries an optional note**
  that rides as feedback into the loop-back), or an answer that injects variables
  and resumes the phase,
- **auto** — the prior phase simply succeeded.

Anything a gate seems to "do" — run a script, investigate a failure — is actually a
**phase the gate routes to**.
Example: on a failed check the gate routes to a **diagnosis phase** whose structured
report becomes a variable, feeding a smarter retry or a more useful park — never a
blind loop.

**Real failure vs flake/infra.** A red check is *not* automatically the change's
fault. The diagnosis phase classifies genuine failure vs. environment/flake so the
system never loops or parks on a broken stack. (Hard-won from prior runs.)

**Execution-failure retry is a separate layer.** Loop-back above is for a phase that
*produced a result* the gate rejects — it carries validation feedback into the retry. A
phase that **failed to produce an envelope at all** (a transient provider overload, network
drop, or subprocess death) is different: no feedback, just wait-and-retry. §12 handles it
with a small conservative backoff; do not conflate it with feedback-carrying loop-back.

**Structure.** Because gates route among phases, a **workflow is a graph of phases
connected by gated transitions**, not a straight line.

## 5. Phase capabilities (tools)

**Structure is declared; agency is granted.** The phase graph is fixed and statically
checkable. What an agent can *do dynamically* inside a phase is the set of **tools its
phase is granted** — this is where side-effecting power lives, deliberately kept out
of the graph so the graph stays analyzable.

**First-party agency is a scoped CLI (`ao`), not an injected toolset.** Phases talk
to the system through a short CLI; the **per-phase credentials injected at phase
entry authorize only the subcommands the workflow granted** that phase — "tools
granted to a phase" *is* that grant. Agents background commands, poll, and read
files natively, so a CLI beats blocking in-context tool calls; from an interactive
chat the same CLI rides the provider's normal bash-approval UX. Mechanics (token
scopes, subcommand surface, transport) live in the decisions log (D15).

Grantable first-party capabilities include:

- **enqueue a work-item** — decomposition / spinning off independent work that is
  *not* part of this run (file a follow-up; break a large goal into separate items).
  Carries an optional **post-back-to-origin-thread** flag: when the run terminates,
  the requesting chat thread receives the outputs (summary + refs) — "agent asks
  for a report" is enqueue(post-back) → keep chatting → result arrives in-thread;
- **schedule a job / automation** — hand work to the §11 scheduler;
- **report back** — the outbound twin of §11's poller: a terminal phase posts the item's
  result (status / comment) to the ticket or MR it came from, by shelling the user's
  authenticated, **profile-bound** command (`gh` / `glab` / `acli` — §8). Same
  delegate-to-host model — no stored creds — so a scheduler-driven loop *closes* (work
  gets done **and** the human watching the ticket learns);
- **update job notes** — a terminal phase may rewrite its job's continuity notes
  (§11); a no-op rewrite is a normal outcome;
- **introspection** — run status / outputs / records, for the agents that manage
  the system (the §7 triage agent, the §8 studio).

**Deferred (post-v1): agent-invoked inter-agent discussions** — an agent questioning
another agent via AO's native discussions (a reviewer asks the worker "why this
decision?"). Additive, not structural; human-initiated discussion flows are unaffected.

**Side effects are idempotent across re-runs (surface-and-skip).** Loop-back (§4), take-over
discard+re-run (§7), and crash-recovery re-runs (§12) all re-enter a phase, so a first-party
side-effecting tool (enqueue / schedule / report-back) **records what it did per (item,
phase)**; on a re-run the system **shows the prior effect and defaults to skip** rather than
silently re-firing (no duplicate Jira ticket, no double-enqueue). It does *not* try to make
arbitrary MCP side effects idempotent — those it simply flags as "will replay" to the human.

**Both.** Any **MCP server can be configured** (once set up) and granted to phases,
**plus** the curated first-party CLI above for interacting with the system itself.

## 6. Running the queue — concurrency & resources

- The user **fills** the queue; **manual priority is drag reordering** of queued items.
- **The queue is a toggle, not a session: active / paused.** While **active**, an
  enqueued item starts as soon as a concurrency slot frees (drain by priority);
  **pausing** stops new starts — running items finish. There is no first-class
  "run session" entity; the run *record* stays per-item. **"Process N, then stop"**
  survives as an optional bound.
- A **coalesced summary** fires on drain-to-empty and on pause (§10).
- Items that hit **needs-human park aside**; the rest keep draining.

**Concurrency & resources:**

- The queue has a **global concurrency cap** (how many items run at once) — a user knob.
- A **phase declares the resources it needs**; a **resource has a capacity** (a mutex
  is capacity 1; "3 testcontainer slots" is capacity 3). A per-phase concurrency cap
  is just a resource named after that phase.
- A resource lock is **phase-scoped**: acquired on phase entry, released on exit —
  including on failure or park (never leaked). An item holds `live-stack` only during
  the phase that needs it, not its whole run.
- **Workflow declares needs; the project profile declares capacities.** `live-stack`
  capacity 1 is an environment fact for a project, not part of the workflow — a
  workflow stays portable across projects. **Capacities are per-project instances**:
  the same resource name in two projects never contends (an app-wide global resource
  is a possible later addition, not v1).
- (Impl note) multi-resource acquisition uses a canonical order / all-or-nothing to
  avoid deadlock; priority decides who gets a freed slot.
- A phase **blocked waiting on a resource holds its global concurrency slot** (no
  yield-and-reacquire). The slot an item holds is stable end-to-end; an idle slot
  while waiting is accepted in exchange for simple, predictable scheduling. The queue
  is rarely saturated (often N=1), so utilization isn't worth the complexity.
- **Lock release is the §12 teardown path.** Every way a phase stops (success, failure,
  park, cancel, watchdog, crash-restart) runs the one teardown path, so a lock is never
  leaked. A phase *blocked on a resource* is therefore bounded by the holder's guaranteed
  release on any exit — it is **not** the "stalled" case the §12 watchdog targets (that one
  is an *active* turn that stopped emitting).

## 7. The human surface — viewing, steering, and needs-human

**Every phase runs as a normal AO thread** in the item's workspace (worktree, or the
project root for derived-read-only runs, §9), so one rule governs all
human interaction: **a turn must be *yielded* before you can inject into it.**

**Viewing is passive.** Open a phase's thread and watch it stream live. No intervention —
the phase runs its agentic turn (with the output schema attached) and finishes normally.

**Sending a message takes over.** If the turn is **in-flight**, sending **interrupts it
first** (forces it to yield); your messages then run as **free-form turns with no schema**
— ordinary steering in that thread and worktree. You finish the takeover by:

- **Complete** — the system runs one **finalize turn that re-attaches the output schema**;
  its constrained final message is the phase's envelope (§3) and the gate routes as if the
  phase had finished on its own. (You may instead fill the envelope's fields by hand — it
  is structured — with no model turn.)
- **Discard + re-run fresh** — if your poking left the worktree somewhere you don't want to
  ship, drop it and re-run the phase. The re-run can seed from the phase's **recorded input
  envelope** (§10) rather than only live state — so iterating on one phase of an N-phase
  graph doesn't require re-running the whole graph. This is also the path a human uses to
  resume a phase that §12 parked after a crash or watchdog trip.

Interrupting leaves the worktree **mid-edit** (partial state) — fine, you took over to fix
things, which is why the natural finishes are Complete or discard + re-run, not "resume
exactly where it was." The multi-turn session keeps the agent's context across the
interrupt. (Relies on AO's existing stop/interrupt.)

### needs-human — when the phase yields on its own

A phase parks at **needs-human** because of a **gate**, a **question**, or being
**genuinely stuck**. `question` and `stuck` are **envelope statuses** (§3) — the turn
ends cleanly with that shape; nothing is suspended provider-side. It surfaces
(sidebar + workflows pane, §10) with its **blocking reason**, a short **digest**, and
the **diff / checks / cost / narrative** — never raw internals (below). Resolve by:

- **Gate decision** — approve / reject (optional note). A click; the gate routes. No thread
  interaction needed.
- **Answer a question** — the turn already ended with status `question`. Send the
  answer; it runs as the **next turn in the same session with the same schema
  attached** — straight to the envelope. No interrupt, no finalize turn, no
  provider-level suspension mechanic.
- **Take over** — drive it yourself, exactly as above (steer free-form → Complete or
  discard + re-run).

| Phase turn state | Sending a message → |
|---|---|
| In-flight (running) | interrupt → yields → steer free-form → **Complete** (finalize turn re-adds schema) or discard + re-run |
| Parked on a `question` envelope | answer runs the next turn, same session, same schema → envelope |

**One contract, one schema.** Whether a phase finishes on its own, is completed via a
finalize turn, or has its fields filled by hand, the result is the *same* typed envelope
constrained by the *same* output schema. The gate never needs to know which produced it.

### Resolving at scale — sweep, hand-off, and the triage agent

- **Needs-attention sweep.** Run detail (§10) steps parked runs one at a time
  (`j`/`k` next/prev), each leading with a short generated digest ("what happened /
  what it needs") and inline approve / answer / take over / re-enqueue / discard.
  The morning-after throughput path.
- **Hand off to an agent.** Every failed/parked run offers a one-click **triage
  thread** — pre-seeded with the run record, envelopes, diff, and typed reason, in
  the item's worktree — for when transferring context beats fixing it yourself.
- **Triage agent.** A conversational agent over the same data ("figure out what
  needs my attention and set up the conversations for it"): read-only introspection
  across items / reasons / records, spawns seeded triage threads (plus an optional
  framing note distilled from the conversation), and takes actions (enqueue, gate
  approve/reject) through AO's normal interactive approval UX. The sweep is the
  zero-agent fast path; the triage agent is the conversational path over the same
  data. The §10 drain summary can deep-link into it.

**No internals on human surfaces.** Variables, envelopes, JSON, and gate traces never
render in the human UI — they exist to make workflows manageable *by agents* (the §8
studio, the triage agent). Human phase detail = narrative digest, diff, checks, cost.

## 8. Intake — creating items and choosing a workflow

**An item, minimally:** a **project** + a **goal/prompt** + a **chosen workflow** + a
**base branch** + optional **seed variables**. The **project is required** — this app
controls many repos; it scopes the item for display and is the repo its worktree is cut
from.

**Producers of that same shape:** by hand in the AO UI; **from any interactive chat
thread** — the same enqueue capability phases get ("queue a fix for this"), shaping
the item from conversation context with a user confirm; an agent via the **enqueue
capability** (§5 decomposition); the **§11 scheduler** on a trigger. Manual is just
the n=1 case; every producer emits the identical item.

**Step mode** is a per-item intake option ("pause at every gate"): every gate behaves
as a human gate — the trust-building mode for a new workflow. A workflow may also
declare it as its default.

**Workflow is chosen explicitly at enqueue** — you curate the queue, so you name the
workflow. Attribute-based **routing** (type/labels → workflow) is a convenience that
rides in with the scheduler (a trigger must pick a workflow without a human); the core
stays explicit.

**Seeds** come from whatever the producer has: a ticket's fields (Jira / GitHub
import), what you typed, the project profile.

### Workflow scope and the project profile

**Definitions live in app-managed directories — shared plus per-project — never in
the project's repo.** **Disk is the source of truth**: definitions are never stored
anywhere else (the only persisted copy is a run's frozen snapshot, below). A
workflow's identity is a **declared `id`** inside the file, not its filename;
**resolution is project → shared**, project winning on `id` collision. Edit a file →
the next enqueue picks it up.

**Authoring is agent-first — the workflow studio.** Creating or editing a workflow or
profile is a **studio thread**: an agent granted the definition schema, the dry-run
validator, and write access to the workflow dirs + profiles — no dedicated editor
surface. Entry points: new workflow, edit existing, and "open in studio" from any run
(pre-loaded with its record + frozen snapshot). Studio threads never appear in normal
thread lists (§10).

A **project profile** is an app-managed per-project **`profile.yaml`** (same
never-in-repo rule) holding the per-project **bindings**: the base branch, the
concrete **check commands** (the deterministic build/test/lint of §3), the
**resource capacities** (`live-stack` = 1, of §6), the **report-back / query-source
command bindings** (§5, §11), the **MCP / tool configs**, the **reliability
defaults** (the §12 inactivity timeout and optional per-item budget), **secret
references**, a **worktree setup** step (files copied from the main workspace and
commands run at worktree creation — `.env`, dependency install; **setup failure
parks `needs-human(setup-failed)`** before any phase starts on a broken tree), and the **disposition
policy** (`manual` | `auto-pr` | `auto-merge`, default `manual` — §9).

**Secrets are references, never values.** Phases, scripts, and MCP servers often need
credentials (a private registry, an MCP API key, a paid API). The profile stores a
**reference** — `name → OS env var / keychain entry / file path` — resolved into the phase
environment / MCP config at phase entry and **never persisted** into the run record,
envelope, or narrative. Resolved values are **masked before the agent-written narrative is
saved** (the narrative is untrusted output and would otherwise leak a key). Same
delegate-to-host philosophy as §11: the system holds no secret material, only pointers.

**Workflow declares; project binds.** A workflow is the **portable graph** — phases,
gates, transitions, and the checks/resources it references *by name*. The **project
profile supplies the concrete bindings** those names resolve to, so the same workflow
runs correctly across projects, each parameterized by its own profile. (This is §6's
needs-vs-capacity split generalized to all project-specific facts.)

A workflow is therefore **either shared** (any project runs it, resolved against that
project's profile) **or project-scoped** (defined for and offered to only one project,
when it only makes sense there). An item picks a workflow that is shared or belongs to
its project. Generic workflows stay shared rather than being copy-pasted per repo.

### Pin a run to the workflow it started on

At **run-start**, the engine **freezes the resolved workflow graph + project bindings into
the run record** (§10) and executes from that **snapshot**, never live config. You *will*
edit a workflow while items are draining (the normal solo-dev loop); pinning means a running
item can't hit a phase that vanished, a gate whose target moved, or a renamed variable.
Edits affect only **later** items. (Snapshot-at-start — not version-negotiation or
patching; it also answers "which definition did yesterday's run use?")

### Starter workflows

Ship **2–3 example workflow files** (`build-and-validate`, `multi-lens-review`,
`poll-jira-and-start`) plus a **`new workflow`** scaffold that stamps a minimal valid graph.
For a solo dev the blank graph is the onboarding wall; the examples encode the hard-won
patterns (a diagnosis phase, a bounded loop-back, fan-out review → join) so authoring is
"tweak an example," not "design from scratch." Content, not engine.

## 9. Worktree & isolation

**Workspace need is derived, not declared.** If no phase in the graph writes, the run
gets **no worktree** — it executes read-only against the project root.
Report/research/triage workflows stop paying the worktree tax; their deliverables go
to the §3 artifact store, never the repo.

**Otherwise: one item = one primary worktree + branch**, cut from the project
profile's base branch at start (base overridable at intake). **By default it persists
through every terminal state** (done, failed, parked, cancelled) — it is the artifact
you act on: inspect it, **work in a thread in that worktree** (steer a running/parked
phase's own thread, §7; or **fork a fresh thread** to continue a *done* item), or
dispose of it. A per-workflow **cleanup policy** (`auto` | `manual`, default
`manual`) amends this: `auto` discards the worktree at terminal state once §3
artifacts are captured — for writing workflows **only after a successful disposition
has landed**; an unlanded branch is never silently discarded.

**Disposition is in-app: merge / PR / discard** on done items. **Nothing auto-merges
by default** — a project profile may opt into **auto-merge-on-done** (§8; side
projects; production stays manual), which proceeds only on a clean merge from a green
terminal state. **Any conflict or dirty base refuses and parks
`needs-human(disposition)`** with a pre-seeded triage thread — never forced, never
silent.

**Writes to the primary worktree are serial** — only one unit writes it at a time. The
common fan-out is **read/analysis** units (N reviewers, N lenses) whose outputs a join
consolidates while a single unit does the writing.

**Parallel writers — isolated sub-worktrees.** A fan-out MAY spawn **multiple writing
agents, each in its own isolated sub-worktree** (detached HEAD / throwaway branch off the
item's branch) — the **explore-and-synthesize** pattern: "implement this N ways," then a
join reads every attempt and figures out the good pieces to write the real spec /
implementation. The attempts diverge freely without colliding.

**The join always consolidates by reading + synthesis, never by git-merge.** A join is an
agent/tool that **reads the N divergent results** (their diffs and output envelopes) and
**produces a new artifact** — a spec, a pick of the best, or a fresh real implementation
in the item's primary worktree. The system **never auto-merges divergent agent edits**;
the only git merge that ever happens is the **disposition** of the item's primary branch
(manual by default, above). Sub-worktrees are **ephemeral inputs to the join**, disposed
after.

**Parallel-writer fan-out vs decomposition** — they look alike but differ in *what
survives*:

- **Decomposition (§5 tool)** spins off **independent deliverables** — each its own item,
  own primary worktree/branch, surfaced and disposed separately by you.
- **Parallel-writer fan-out (a §3 shape)** spawns **throwaway explorations** inside one
  item; their only output is what the join synthesizes. They never surface as separate
  deliverables.

## 10. The surface — sidebar, workflows pane, and per-phase threads

**Sidebar: a per-project workflows section + a global footer badge.** Each project
gets a **separate sidebar section** for its workflows — never mixed with normal
threads; a **global badge in the sidebar footer** carries the single needs-attention
count and opens the pane. **Phase, studio, and triage threads are excluded from
normal thread lists** — their own mode, reachable only from the workflows surfaces.

**One workflows pane with a stacked drill-down** — a normal multi-pane citizen, not
a surface takeover: **overview** (workflow list with aggregate state — running /
queued / scheduled / needs-attention) › **workflow detail** (live runs, history,
next scheduled run, job notes) › **run detail** (phases, digest, diff / checks /
cost). Only threads break out — a phase/triage/studio thread always opens as a
normal thread pane beside it. **Run detail leads with the digest, diff, checks, and
cost, and hosts the resolution actions** (approve / reject / answer / hand-off /
disposition) plus the §7 needs-attention sweep. The earlier **kanban board and
inspection modal are superseded** by this pane. Full surface behavior:
`workflows-system-ui/UI-SPEC.md`.

**Every phase is a normal AO thread.** Open a phase's thread from run detail to
**observe, steer, or just watch**; steering follows the turn-state interaction model
in §7 (sending a message interrupts an in-flight turn). **Historical phase threads
stay openable** — run detail exposes the thread of *every* phase attempt (completed,
failed, superseded retries), not just the running one. Continuing a *done* item forks
a fresh thread in the same worktree (§9).

**The run record is persisted.** The pane and its history are reads over a
**persisted run record** — ordered phases, each phase's envelope + gate decision + narrative
reference, timings, resource waits, the **frozen workflow snapshot** the run executed under
(§8), a **typed reason** on every `failed` / `needs-human` / `cancelled` transition, and a
per-phase **intervention** field (the messy §7 paths — take-over, complete-by-hand,
discard+re-run — kind / when / note). It is also the **recovery journal** (§12): an item in
`running` whose current phase has no terminal envelope is what crash recovery looks for on
restart.

### Notifications — the park-aside model needs a nudge

Park-aside (§6/§7) assumes the human comes back, but on a desktop app the user isn't
watching the pane — so the system **actively nudges**:

- A **native OS notification** fires on **`needs-human`** and **`failed`** only (never on
  `done` / `queued` / `running` — those are a passive badge), carrying project + goal + the
  typed blocking reason, and **deep-links to the parked run's detail** (the thread is one
  click away there).
- On **drain-to-empty or pause** (§6), **one coalesced summary** — "5 processed: 3 done,
  2 need you, 0 failed" — not one notification per item (anti-fatigue). The summary can
  **deep-link into the triage agent** (§7).
- The notification is a **nudge, not the source of truth**: the durable **needs-attention
  list / badge** in the sidebar is authoritative and **re-surfaces on app open**, so a
  missed or cleared notification never loses work.

These reuse the §11 **internal-event stream** (no new event source).

### Streaming and the structured envelope coexist (verified empirically)

A phase must both **stream** (so you can observe/steer it as a thread) and **emit its
typed control envelope** (§3). These coexist via an **output schema constraining the
turn's final message** — verified live and adversarially on **both** providers (the
D2a spike):

- **Claude:** the streaming session takes the schema; the full agentic event sequence
  streams; the payload arrives on the final result. But the mechanism is an
  *encouraged* synthetic tool call — **a turn can end cleanly with the payload simply
  absent** (the explicit retry-error subtype fires only under unusual pressure).
- **Codex:** the app-server takes a per-turn schema; decoding is logit-constrained
  but **validates structure only** — value constraints are silently ignored.

**So "valid-or-explicit-error" is false on both providers.** The engine invariant
(§3): **the only success signal is a present payload that passes the engine's own
full-schema validation.** Absent-or-invalid = envelope-production failure → the §12
feedback-retry-then-park path. Never gate on provider error flags or turn status
alone.

The envelope remains the **constrained final message** of the phase's agentic turn;
intermediate work streams and stays observable; the **narrative** is written to a
file during the turn (§3). Wiring specifics (flags, params, payload locations,
per-turn re-send quirks) live in the decisions log (D2a).

## 11. Scheduler / automations

**Ships in v1** (not a fast-follow). An **automation = a trigger + an optional
condition + an action**, and **the action is always to enqueue an item** (§8).
The scheduler never runs a second executor — it feeds
the queue and the engine (§6) drains it. (A "single-phase," "a script,"
"script → workflow," or "an agent that decides" are all just *workflows*, §3 — so every
action reduces to one item with a chosen workflow + seed variables; an agent-decider is a
workflow that enqueues 0..N via the enqueue tool.)

**Two trigger primitives:**

- **Cron** — time-based; any schedule.
- **Internal event** — the system's own lifecycle (item done / failed / needs-human, a
  phase completed, a gate outcome). A **closed, typed set** the system already emits (the
  §10 run-record stream); carries run context as variables. This is what chains
  automations (one item finishing triggers the next).

**Conditions** (run-if, reusing §4) gate whether a fired trigger actually enqueues.

**Minimal in-pane management + Run now.** Workflow detail (§10) shows an automation's
trigger, enable/disable, and next run, plus a **Run now** button that enqueues through
the normal queue. Anything richer (changing cron, seeds, conditions) is §8
studio-agent work over the automation config, not forms.

**Job continuity notes.** A scheduled/triggered job carries **per-job notes** (a
markdown blob) for cross-run continuity: visible and editable in the UI, injected as
a reserved seed variable, and optionally rewritten by a terminal phase via the §5
**update-job-notes** capability (a no-op rewrite is a normal outcome). Deliberately a
notes file per job, **not a memory subsystem** (the exclusion below stands).

### External sources — poll with authenticated CLIs, no webhooks

External triggers (MR comments, ticket transitions, pushes) are reached by **polling
through the user's already-authenticated CLIs** — **profile-bound commands** (§8:
`gh`, `glab`, `acli` for Jira), never a tool name hardcoded in the engine — **not** by
inbound webhooks. A poll is just a **cron automation whose action workflow queries the
source and enqueues** what's new: a phase shells out to the CLI, computes "new since a
cursor," and enqueues a child item per hit. It is **not a third trigger primitive** — just
cron + a query-and-enqueue phase using the curated **query-source** and **enqueue** tools
(§5).

**The system manages no external credentials.** Auth is delegated to the user's logged-in
CLIs; the system holds no tokens and runs no inbound server. (Requires the user to be
logged into the bound CLIs for those sources to be pollable.)

**Required for correctness:** a **cursor/watermark** per polled source (last-seen id /
`updated >=` timestamp) + **dedup**, so one comment or transition enqueues exactly one
item.

**Trade-off (accepted):** polling reacts on the next tick (minute-scale latency), not
instantly — fine for coding automations. **Webhook ingress** (instant push) is
**deferred**, not deleted: it would reintroduce an inbound endpoint (hard to reach for a
desktop app) and secret/signature management. Revisit only if a real sub-minute-latency
need appears.

**Example (ticket transition):** cron every 5 min → workflow `poll-jira-transitions`: a
phase shells the profile-bound Jira command (`acli`) for issues with a status change and
`updated >= <cursor>`, dedups
against seen keys, and for each new one enqueues an item (workflow `start-ticket`, seeds
`{ticket_key, to_status}`); then advances the cursor.

## 12. Reliability & lifecycle controls

The controls that keep an autonomous, unattended drain from silently wedging, running away,
or losing work. They are **one mechanism plus a few triggers**, not a pile of features.

### The teardown contract (one path, many triggers)

There is **one teardown path**, and everything that stops a phase runs it:

> **stop the turn → release the phase's resource locks (§6) → write a partial envelope →
> route to a terminal / needs-human state with a typed reason.**

Triggers that invoke it: **crash-restart**, **cancel**, **watchdog trip**,
**transient-retry exhaustion**, **budget exceeded**. Specifying it *once* is the point —
five separate release paths would each risk leaking the `live-stack` mutex, which would
deadlock every future item that needs the stack. One path, tested once, makes every trigger
correct for free. What looks like five reliability features is **one mechanism + four
buttons that press it**.

### The triggers

- **Crash recovery (park, don't auto-resume).** A desktop app gets killed mid-run (sleep,
  quit, OS update, crash). On startup, any item in `running` whose current phase has **no
  terminal envelope** runs teardown and **parks `needs-human(interrupted)`**; the human
  re-runs the phase via the existing §7 path. The system does **not** auto-re-run — an
  unattended double-execute is a worse failure than a paused item. (The run record (§10) is
  the recovery journal; no checkpointing, no turn-replay.)

- **Inactivity watchdog.** §2 names "genuinely stuck," but a headless turn can hang
  silently. A phase whose **active turn emits no stream event for T** (a project-profile
  default, per-phase overridable) runs teardown and parks `needs-human(stalled)`. It is
  **not a wall-clock cap** — agent turns legitimately run minutes to hours; a duration cap is
  either uselessly loose or kills good work. It watches the stream the phase already emits;
  no heartbeat protocol. (A phase *waiting on a resource* is not "stalled" — that wait is
  bounded by the holder's guaranteed teardown release, §6.)

- **Cancel (the kill button).** A human stops a running item: kill the subprocess → teardown
  → terminal **`cancelled`** (distinct from `failed`). A core desktop affordance for "this is
  running away" or "wrong goal." The worktree persists like any terminal state (§9).

- **Transient-execution retry.** A phase that **fails to produce an envelope** due to a
  **conservative allowlist** of transient errors (subprocess exit, known provider-overload
  responses, network errors) retries with **backoff, cap ~3**, then parks
  `needs-human(retries-exhausted)`. Anything **not** on the allowlist parks immediately —
  never retried. This is the no-feedback sibling of §4's feedback-carrying loop-back: a 529
  carries no validation signal, it just waits and re-runs. Without it, a blip a 30-second
  backoff would fix drags a human into the loop and defeats the autonomy goal.
  **Distinct from this allowlist:** an envelope **absent or invalid after §3 engine
  post-validation** gets exactly **one feedback-carrying retry turn** ("your envelope
  failed validation: <errors>"), then parks `needs-human(agent-error)` with the
  partial envelope — an agent-quality failure, not a transient; never backoff-retried.

- **Per-item budget.** One **optional** ceiling per item — **tokens / $** if the provider
  reports spend, else **wall-clock** — checked at phase boundaries. On exceed: teardown →
  `needs-human(budget-exhausted)` with spend-so-far. Headless full-access is exactly the
  config that runs away, and a solo dev pays per token. This is the *single* runaway ceiling;
  it subsumes a per-phase turn cap, so there is deliberately **not** a second per-phase knob.

### What this is NOT (kept out on purpose)

No Temporal-style deterministic replay or workflow patching (impossible for non-deterministic
agent turns — record the envelope, never replay the turn). No heartbeat-RPC subsystem (a
local watchdog over the existing stream is enough). No dead-letter queue (the `failed` /
parked states are it), no backfill, no quiet-hours / escalation engine, no per-call approval
prompts (worktree isolation + nothing-auto-merges is the safety model; reserve approval for
irreversible *external* effects like `git push` via a tiny per-tool flag). The `running`
state is simply **always exitable**.

---

## Deliberately out of scope

- **Self-improving workflows / a memory subsystem.** Considered and **excluded** —
  not core to the product. AI-managed memory is historically junk (transient noise,
  hallucinated facts), and the durable knowledge that matters already lives in the
  repo (code, project profile, `CLAUDE.md`-style rules, ADRs) which phases read
  anyway. A "watcher" that proposes workflow tweaks is, if ever wanted, just a later
  §11 automation built on existing primitives — not a subsystem. Rationale and the
  research behind this call: `workflows-system-self-improvement-research.md`.
- **Agent-invoked inter-agent discussions** — post-v1 (§5). Human-initiated
  discussion flows are unaffected.
- **Sub-workflow phase shape** — post-v1 (§3); nothing in v1 forecloses it.
- **Remote mutation of workflows** — remote browsers get **view-only** workflows in
  v1, consistent with AO's existing remote posture. Remote gate-approval is a
  possible later relaxation, explicitly not v1.
- **Webhook ingress** — stays deferred (§11).
