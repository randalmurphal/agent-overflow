# Workflows System — Specification (Agent Overflow)

> Status: **In progress** — built one decision at a time via discussion.
> This is a WHAT spec (behavior / intent), not a HOW spec. Implementation
> notes appear only where they constrain the what.
>
> Lineage: based on `orc`'s workflow/phase model where it fits; executes on
> Agent Overflow's existing thread/provider runtime.

## 1. Purpose

A system that drives a user-curated, prioritized **queue** of work items, each
through a **defined multi-phase workflow**, autonomously to completion —
pausing only at explicit gates that need a human.

- The **queue is the heart**: the user fills it, prioritizes it, starts it, and
  it drains. An individual item is just a queue of n=1; there is no separate
  one-off execution path.

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
  `failed` (work failed) so the two read differently on the board.

**Every `failed` / `needs-human` / `cancelled` transition carries a typed reason** (gate,
question, stuck, stalled, budget-exhausted, retries-exhausted, check-failed-genuine,
agent-error, wiring-error, interrupted) — recorded in the run record (§10) and shown on the
card, so a stopped item is never a silent dead end.

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
    phase's envelope. Nesting is allowed but bounded.

  ("Long-running" is **not** a shape: a normal phase runs until it emits *done*,
  however long that takes — even waiting on an external event is just a tool phase
  that polls.)

**Sub-workflows are inline composition only** — a reusable nested workflow that runs
as part of the phase (same work-item; worktree per §9 — its own sub-worktree when it is
a parallel writer), may run in parallel with other units in a fan-out, and flows its
result up when complete. **Decomposition is not a phase
shape**: spawning independent work is an agent *action* via a granted tool (§5), not
workflow structure.

**Phase output — control envelope + narrative file.**

- Every phase emits a **structured control envelope** (system-owned): status, gate
  signals, typed **output variables**, and references to any files it wrote. This is
  the typed channel — it becomes downstream variables and gate inputs.
- The **narrative** (what it did, reasoning, decisions) is written to a **file** in a
  system-dictated location and referenced from the envelope. The system never parses
  it; humans and later phases read it on demand.

**Typed, validatable variables.** Variables are typed; most required, some optional.
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

**Outcomes:** **advance** (to a named next phase) · **loop back** (bounded, carries
feedback; exhausting retries → needs-human) · **park** (needs-human).

**What a gate reads to decide:**

- a **deterministic check's** result — a tool/script phase (tests, lint, build):
  pass/fail + variables,
- an **ai judgment** — an agent phase that assesses readiness,
- a **human** decision — approve/reject, or an answer that injects variables and
  resumes the phase,
- **auto** — the prior phase simply succeeded.

**Conditions are first-class.** Transitions are conditional (run-if over variables),
with **short-circuit** (a failed check skips the rest). Anything a gate seems to "do"
— run a script, investigate a failure — is actually a **phase the gate routes to**.
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

Built-in action tools the system can grant a phase include:

- **enqueue a work-item** — decomposition / spinning off independent work that is
  *not* part of this run (file a follow-up; break a large goal into separate items);
- **open an inter-agent discussion** — via AO's native discussions, an agent can
  question another agent (e.g. a reviewer asks the worker "why this decision, did you
  consider X?"). Most workflow systems can't offer this; AO can, natively;
- **schedule a job / automation** — hand work to the §11 scheduler;
- **report back** — the outbound twin of §11's poller: a terminal phase posts the item's
  result (status / comment) to the ticket or MR it came from, by shelling the user's
  authenticated `gh` / `glab` / Jira CLI. Same delegate-to-host model — no stored creds — so
  a scheduler-driven loop *closes* (work gets done **and** the human watching the ticket
  learns).

**Side effects are idempotent across re-runs (surface-and-skip).** Loop-back (§4), take-over
discard+re-run (§7), and crash-recovery re-runs (§12) all re-enter a phase, so a first-party
side-effecting tool (enqueue / schedule / report-back) **records what it did per (item,
phase)**; on a re-run the system **shows the prior effect and defaults to skip** rather than
silently re-firing (no duplicate Jira ticket, no double-enqueue). It does *not* try to make
arbitrary MCP side effects idempotent — those it simply flags as "will replay" to the human.

**Both.** Any **MCP server can be configured** (once set up) and granted to phases,
**plus** a **curated set of first-party tools** for interacting with the system
itself (enqueue work-item, open inter-agent discussion, schedule, …).

## 6. Running the queue — concurrency & resources

- The user **fills** the queue and **sets item priority** (manual ordering).
- A run is **explicitly started** (not always-on). It **drains by priority** until
  empty or stopped, and is either **bounded** ("process N, then stop") or
  drain-to-empty.
- Items that hit **needs-human park aside**; the rest keep draining.

**Concurrency & resources:**

- A run has a **global concurrency cap** (how many items run at once) — a user knob.
- A **phase declares the resources it needs**; a **resource has a capacity** (a mutex
  is capacity 1; "3 testcontainer slots" is capacity 3). A per-phase concurrency cap
  is just a resource named after that phase.
- A resource lock is **phase-scoped**: acquired on phase entry, released on exit —
  including on failure or park (never leaked). An item holds `live-stack` only during
  the phase that needs it, not its whole run.
- **Workflow declares needs; project/host declares capacities.** `live-stack`
  capacity 1 is an environment fact for a project, not part of the workflow — a
  workflow stays portable across projects.
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

**Every phase runs as a normal AO thread** in the item's worktree, so one rule governs all
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
**genuinely stuck**. It surfaces (sidebar + board, §10) with its **blocking reason**, the
phase's **narrative file**, and the **current variables**. Resolve by:

- **Gate decision** — approve / reject (optional note). A click; the gate routes. No thread
  interaction needed.
- **Answer a question** — the phase is already yielded, waiting. Send the answer; it
  injects as a variable and the phase **continues its own schema'd turn** to the envelope.
  No interrupt, no finalize turn.
- **Take over** — drive it yourself, exactly as above (steer free-form → Complete or
  discard + re-run).

| Phase turn state | Sending a message → |
|---|---|
| In-flight (running) | interrupt → yields → steer free-form → **Complete** (finalize turn re-adds schema) or discard + re-run |
| Yielded on a question (parked) | answer injects → phase continues its own schema'd turn → envelope |

**One contract, one schema.** Whether a phase finishes on its own, is completed via a
finalize turn, or has its fields filled by hand, the result is the *same* typed envelope
constrained by the *same* output schema. The gate never needs to know which produced it.

## 8. Intake — creating items and choosing a workflow

**An item, minimally:** a **project** + a **goal/prompt** + a **chosen workflow** + a
**base branch** + optional **seed variables**. The **project is required** — this app
controls many repos; it scopes the item for display and is the repo its worktree is cut
from.

**Producers of that same shape:** by hand in the AO UI; an agent via the **enqueue
tool** (§5 decomposition); the **§11 scheduler** on a trigger. Manual is
just the n=1 case; all three emit the identical item.

**Workflow is chosen explicitly at enqueue** — you curate the queue, so you name the
workflow. Attribute-based **routing** (type/labels → workflow) is a convenience that
rides in with the scheduler (a trigger must pick a workflow without a human); the core
stays explicit.

**Seeds** come from whatever the producer has: a ticket's fields (Jira / GitHub
import), what you typed, the project profile.

### Workflow scope and the project profile

A **project profile** holds the per-project **bindings**: the repo + base branch, the
concrete **check commands** (the deterministic build/test/lint of §3), the **resource
capacities** (`live-stack` = 1, of §6), the **MCP / tool configs**, the **reliability
defaults** (the §12 inactivity timeout and optional per-item budget), and **secret
references**.

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

**One item = one primary worktree + branch**, cut from the project profile's base branch
at start (base overridable at intake). It **persists through every terminal state** (done,
failed, parked, cancelled) — it is the artifact you act on: inspect it, **work in a thread in that
worktree** (steer a running/parked phase's own thread, §7; or **fork a fresh thread** to
continue a *done* item), or merge it. All manual. **Nothing
auto-merges.** **Cleanup is a disposition you take** (merge or discard), never automatic —
the system never reclaims a worktree out from under you.

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
the only git merge that ever happens is *you* disposing of the item's primary branch.
Sub-worktrees are **ephemeral inputs to the join**, disposed after.

**Parallel-writer fan-out vs decomposition** — they look alike but differ in *what
survives*:

- **Decomposition (§5 tool)** spins off **independent deliverables** — each its own item,
  own primary worktree/branch, surfaced and disposed separately by you.
- **Parallel-writer fan-out (a §3 shape)** spawns **throwaway explorations** inside one
  item; their only output is what the join synthesizes. They never surface as separate
  deliverables.

## 10. The surface — sidebar, board, and per-phase threads

**Sidebar (repurposed AO sidebar, existing priority system).** Per-project, shows what
**needs attention**, what's **running**, what **completed** — using AO's existing
priority ordering. Expand an item to see the **phases that ran / are running** and open
into any of their **threads**.

**Main "workflows" page — a kanban board.** Items as cards in columns by state
(queued / running / needs-human / done / failed), grouped and filterable by project.
Click a card to open the item and view **per-phase detail** (envelope, variables, gate
outcome, narrative file).

**Every phase is a normal AO thread.** Open a phase's thread from the item or the sidebar
to **observe, steer, or just watch**; steering follows the turn-state interaction model in
§7 (sending a message interrupts an in-flight turn). Continuing a *done* item forks a fresh
thread in the same worktree (§9).

**The run record is persisted.** The board and the expandable history are reads over a
**persisted run record** — ordered phases, each phase's envelope + gate decision + narrative
reference, timings, resource waits, the **frozen workflow snapshot** the run executed under
(§8), a **typed reason** on every `failed` / `needs-human` / `cancelled` transition, and a
per-phase **intervention** field (the messy §7 paths — take-over, complete-by-hand,
discard+re-run — kind / when / note). It is also the **recovery journal** (§12): an item in
`running` whose current phase has no terminal envelope is what crash recovery looks for on
restart.

### Notifications — the park-aside model needs a nudge

Park-aside (§6/§7) assumes the human comes back, but on a desktop app the user isn't
watching the board — so the system **actively nudges**:

- A **native OS notification** fires on **`needs-human`** and **`failed`** only (never on
  `done` / `queued` / `running` — those are a passive badge), carrying project + goal + the
  typed blocking reason, and **deep-links to the parked phase thread**.
- On a run stopping (bounded or drained), **one coalesced summary** — "5 processed: 3 done,
  2 need you, 0 failed" — not one notification per item (anti-fatigue).
- The notification is a **nudge, not the source of truth**: the durable **needs-attention
  list / badge** in the sidebar is authoritative and **re-surfaces on app open**, so a
  missed or cleared notification never loses work.

These reuse the §11 **internal-event stream** (no new event source).

### Streaming and the structured envelope coexist (verified empirically)

A phase must both **stream** (so you can observe/steer it as a thread) and **emit its
typed control envelope** (§3). These coexist via **constrained-decoding output schema** —
verified live on **both** providers, not inferred:

- **Claude:** `claude -p --output-format stream-json --verbose --json-schema <schema>`
  streams the full agentic event sequence (tool-use events included); the schema binds
  **only the final result message** ("after the agent completes its workflow"); the
  validated object arrives on the final `result` event's `structured_output` field. On
  repeated mismatch it returns the result subtype `error_max_structured_output_retries`
  — so the envelope is **valid-or-explicit-error, never silently malformed**.
- **Codex:** the app-server `turn/start` request accepts a per-turn **`outputSchema`**
  ("Optional JSON Schema used to constrain the final assistant message for this turn").
  A live app-server spike streamed `item/agentMessage/delta` events and returned a final
  `item/completed` that validated against the schema — in one turn.

So the envelope is the **constrained final message** of the phase's agentic turn;
intermediate work streams and stays observable; the **narrative** is written to a file
during the turn (§3). This is a stronger guarantee than a tool call, which only hopes the
model invokes it.

**Net-new wiring (small — the capability is proven, not missing):**

1. **Claude:** pass `--json-schema` into the streaming session and parse
   `structured_output` off the final `result` line. (AO's streaming session does not pass
   the flag today; its result parser already recognizes the retry-error subtype.)
2. **Codex:** add `outputSchema` to the `turn/start` params AO already builds at
   `internal/provider/codex/session.go:330-355` (no schema passed there today; AO uses
   `--output-schema` only in the one-shot `codex exec` path).

**One nuance to verify under load:** both runs prove the final message is schema-*valid*;
neither formally proves strict logit-level constrained decoding vs. validate-and-retry. A
short spike with a hard/large schema + off-distribution prompt should confirm the
guarantee holds adversarially before depending on it.

## 11. Scheduler / automations

An **automation = a trigger + an optional condition + an action**, and **the action is
always to enqueue an item** (§8). The scheduler never runs a second executor — it feeds
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

### External sources — poll with authenticated CLIs, no webhooks

External triggers (MR comments, ticket transitions, pushes) are reached by **polling
through the user's already-authenticated CLIs** — `glab` / `gh` / the Jira CLI — **not** by
inbound webhooks. A poll is just a **cron automation whose action workflow queries the
source and enqueues** what's new: a phase shells out to the CLI, computes "new since a
cursor," and enqueues a child item per hit. It is **not a third trigger primitive** — just
cron + a query-and-enqueue phase using the curated **query-source** and **enqueue** tools
(§5).

**The system manages no external credentials.** Auth is delegated to the user's logged-in
CLIs; the system holds no tokens and runs no inbound server. (Requires the user to be
logged into `glab` / `gh` / Jira for those sources to be pollable.)

**Required for correctness:** a **cursor/watermark** per polled source (last-seen id /
`updated >=` timestamp) + **dedup**, so one comment or transition enqueues exactly one
item.

**Trade-off (accepted):** polling reacts on the next tick (minute-scale latency), not
instantly — fine for coding automations. **Webhook ingress** (instant push) is
**deferred**, not deleted: it would reintroduce an inbound endpoint (hard to reach for a
desktop app) and secret/signature management. Revisit only if a real sub-minute-latency
need appears.

**Example (ticket transition):** cron every 5 min → workflow `poll-jira-transitions`: a
phase shells the Jira CLI for issues with a status change and `updated >= <cursor>`, dedups
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
