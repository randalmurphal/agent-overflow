# Multi-agent campaigns with workflows

How to run a long, unattended, many-wave effort — a language port, a
framework migration, a test-coverage sweep — on the workflows system.
The mechanics are in [`../specs/workflows-system.md`](../specs/workflows-system.md);
this is the authoring layer above them: which shapes to reach for, why
each one exists, and how to drive the thing while it runs.

The shipped reference is the `port-campaign` starter
(`internal/workflow/starters/content/port-campaign/`). Scaffold your own
copy and edit it:

```
agent-overflow workflow new port-campaign --id my-campaign --project <slug>
agent-overflow workflow validate --id my-campaign --project <slug>
```

## The campaign wave

A campaign is not one run. **One run is one wave**, and waves chain
through an automation. There is no wave-level loop primitive and no
second orchestration engine — the loop lives in the scheduler (D34).

```
survey ─► plan ─┬─► implement ─┬─► review ─┬─► verify ─┬─► done
 tool     agent │   fan-out    │  fan-out  │   tool    │
                │      │       │     │     │           ├─► loop review (max 2)
                │      └─► resolve-  │     └─► fix ────┘
                │          conflicts │         agent   └─► park wave-verification-failed
                │              └─► park merge-unresolved
                └─► park campaign-complete / campaign-stalled
```

| Phase | Driver | Job |
|---|---|---|
| `survey` | tool (`wave-survey`) | Run the build/tests; emit failures as structured data. A red tree is not an error here — it is the queue. |
| `plan` | agent, read-only | Turn survey failures plus the next unported slice into a typed `tasks[]` array. Reports what it left out. |
| `implement` | fan-out over `plan.tasks` | One Codex unit per task, each in its own sub-worktree/branch. Join is a **tool** that git-merges the branches. |
| `resolve-conflicts` | agent, write | Only on a dirty merge. Bounded: unresolvable conflicts park. |
| `review` | fan-out, 3 static lenses | Read-only reviewers told to **refute**. Join adjudicates claims against the tree. |
| `fix` | agent, write | Applies only confirmed findings, severity-ordered. |
| `verify` | tool (`build-and-test`) | The deterministic gate. A red verify loops back to `review` rather than to `fix` — something got past the reviewers, so re-arm them. Bounded at 2, then parks. |

Two structural facts do most of the work:

- **The implement join is a command, not an agent.** Nothing an
  implementer *said* crosses into review — only the merged tree and the
  commit it is diffed from. Split context is a property of the graph,
  not of prompt wording.
- **`implement.passed` is the tool driver's own output.** The gate reads
  it; the phase never declares it. `passed` and `exit-code` are reserved
  on any phase whose envelope comes from a command — declaring one is a
  validation finding.

## Patterns, and why

**Adversarial verify.** Reviewers are prompted to *break* the change,
not to approve it. An approval prompt gets approval: the cheapest way to
satisfy "does this look right" is to say yes. Refutation has a cost
function that points at defects. Pair it with **split context** —
reviewers see the artifact, never the author's reasoning. A reviewer
handed the implementer's rationale is grading an argument, and a
plausible argument is exactly what a wrong change comes with.

**Perspective-diverse review.** Three reviewers with *different lenses*
(port fidelity / failure modes / cross-entry integration), not three
copies of one reviewer. Identical reviewers correlate: they find the
same things and miss the same things, so the second and third cost
tokens and buy nothing. Lenses decorrelate what gets looked at. The
adjudicating join then treats every claim as a **lead, not a verdict**
and reproduces it in the tree before it becomes a finding — otherwise
the fix phase spends the wave on things that were never wrong.

**Loop until dry.** Discovery of unknown size does not finish on a
schedule. The campaign keeps firing waves while `plan` reports work, and
stops when `scheduled` and `deferred` are both zero — which parks
`campaign-complete` and halts the chain, because a parked run blocks
every later fire. A fixed wave count either stops early with a tail of
missed work or burns budget on empty waves. If you want the stricter
form, require *K consecutive* empty waves before stopping: carry the
count in the automation's job notes and have `plan` read it.

**Completeness critic.** `plan` is the next round's critic: it looks at
the tree the last wave left and asks what is still missing. That is why
`survey` runs *first* — the wave's own build failures are re-derived
from the workspace rather than plumbed across runs. Anything you want
carried between waves that is not visible in the repo has to travel
through job notes.

**No silent caps.** Every phase that bounds its own coverage reports the
bound: `plan` emits `deferred` (count) and `dropped` (prose); the review
join emits `unreviewed`. A phase that quietly truncates reads downstream
as full coverage, and a gap that looks like coverage survives every
later wave. The system takes the same line at the engine level:
`max_fan_out_width` **refuses** an over-wide expansion rather than
running the first N (D29).

**Compiler errors as the work queue.** `survey` is a bound command that
writes `failures[]` to `$AO_ENVELOPE`, so a red tree arrives as typed
data the planner sorts, not as prose a model has to re-read. Failures
sort ahead of new work: nothing new is worth starting while the compiler
disagrees with the port.

**Humans edit rules, not code.** The three knobs are the automation's
seeds (`campaign-goal`, `max-tasks`), its **job notes** (injected as
`job-notes` into every wave, editable in the overlay or via
`agent-overflow notes set`), and the prompt files. Change how the
campaign *decides*; do not hand-fix the code it produces, or the next
wave regresses it and you never learn the rule was wrong.

## Chaining the waves

**Automations are per-project rows, not files** — they live in SQLite,
so they cannot ship inside a starter. The starter ships the wave; this
is the wiring.

**The only automation-creating surface today is `agent-overflow
schedule`**, and it creates a cron trigger with seeds. The overlay
lists automations, toggles them, and offers **Run now**; it has no
create/edit form, so event triggers and run-if conditions — both fully
supported by the scheduler — have no user-reachable authoring path yet.
Everything below the cron recipe describes what the engine will do once
one exists.

Shipped recipe — **cron + skip-if-running**, from any agent session in
the project:

```
agent-overflow schedule my-campaign \
  --cron '*/10 * * * *' \
  --name 'Port campaign' \
  --seed campaign-goal='Port ai-foundations from Python to Go; ...' \
  --seed max-tasks=8
```

Why this shape:

- **skip-if-running is automatic and per-automation.** A fire while the
  previous run is `running` *or* `needs-human` is skipped and recorded
  on the row. Waves never double up, and a parked wave halts the
  campaign until a human clears it — which is what `campaign-complete`,
  `campaign-stalled`, `merge-unresolved`, and
  `wave-verification-failed` all rely on.
- **An automation cannot chain itself on an event.** A run started by
  automation A whose completion re-matches A's trigger is refused as
  `self-chain` (`internal/workflow/scheduler/fire.go`). Cycles across
  *two* automations are legal, so an event chain needs a pair — see
  below.
- **The tick leaves room for disposition.** Auto-merge and the
  scheduler's event feed are both queued off the same terminal
  transition and race. A cron tick is at least a minute out; a local
  merge is milliseconds.

**Set `disposition: auto-merge` in the project profile.** Each run cuts
its worktree from the base branch, so without it every wave starts from
the same commit and replans the same work. The starter sets
`cleanup: auto`, which discards a worktree only *after* a disposition
has landed — 40 waves otherwise means 40 worktrees.

Lower-latency variant, **once an authoring surface exists** — two event
automations that alternate. Both carry this trigger and both start the
campaign workflow; whichever one started the finished run
self-chain-skips, so exactly one fires:

```json
{"kind":"event","on":"item-done","workflowId":"my-campaign"}
```

Two caveats, both real: seed the chain with **Run now** on one of the
pair (a manual start belongs to neither, so *both* fire and you get two
concurrent waves), and the auto-merge race above is live. Reach for it
only when wave latency matters more than the extra discipline.

**What a run-if can and cannot ask.** The condition is evaluated against
exactly the map the run would be seeded with — the automation's stored
seeds plus the reserved `trigger` and `job-notes`. It cannot read the
previous run's outputs. "Is there work left" therefore lives in the
graph, not the condition: work remaining ends the wave `done` and the
next fire proceeds; nothing remaining parks and every later fire skips.

What the condition *is* good for is an arming switch you can flip
without deleting the automation:

```json
{"eq":{"ref":"campaign-armed","value":true}}
```

paired with `--seed campaign-armed=true`. Automation seeds are not
type-checked against the workflow's declared inputs (only agent-started
runs are), so a seed the condition reads and no phase declares is inert
to the run. Flip it to `false` and every fire records `condition false`
on the row instead of starting a wave. Until an editing surface lands,
the same effect is one click: disable the automation in the overlay.

## Operating a running campaign

**From a chat session.** Type `/workflow` in the composer: it expands at
send into the project's workflow sources, the available definitions, the
active runs, and whether `agent-overflow` resolved on PATH (D31). That
block is what an agent reads before it types anything. Every verb below
works from any session in the project.

```
agent-overflow run list --active
agent-overflow run status <run-id>
agent-overflow run output <run-id>          # declared workflow outputs
agent-overflow run pause <run-id>
agent-overflow run resume <run-id> [--phase <id>]
agent-overflow run retry-failed-units <run-id> [--note '...']
agent-overflow run cancel <run-id>
```

**Pause / fix / resume.** Pause interrupts the in-flight turn and parks
the run `needs-human(paused)`; resume continues on the sessions it
parked on, or re-enters a phase you name with `--phase`. A paused run is
`needs-human`, so the automation skips while you work — the campaign
stops for exactly as long as you do. Definitions are **frozen at run
start**: a prompt edit reaches the next run, never the one you are
watching. To put an edit into flight, cancel the wave; the next tick
starts a fresh one on the edited files.

**Usage limits.** A wide fan-out hits a provider wall and most of its
units fail against it within a minute. The wave parks
`needs-human(unit-failed)`. Wait for the reset or switch account, then
repair the whole attempt in one command:

```
agent-overflow run retry-failed-units <run-id> --note 'quota reset'
```

One engine command, not N (D33): the failed set is collected before the
first write, so no other command sees a half-repaired fan-out. Repaired
units re-queue through normal admission — twenty units against a
provider bound of two starts two. Units under human takeover are left
alone. The single-unit `run retry-unit` stays for the other shape: one
unit failing on its own merits.

**Bounds worth setting before you walk away.**

| Knob | Where | Default |
|---|---|---|
| `max_fan_out_width` | project profile | **32.** Absolute ceiling on units per fan-out attempt. Refuses, never truncates (D29). Per project, never per workflow. Minimum 1; there is no unlimited setting. |
| `reliability.per_item_budget` | project profile | **None.** Exactly one of `tokens`, `usd`, `wall_clock`. Unset means *no ceiling* — an unattended campaign is exactly the case that wants one. |
| `capacities.provider:codex` | project profile | 2. How many agent turns run at once. Throttles; does not refuse. |
| `reliability.watchdog` | project profile | Inactivity kill for a silent phase; parks `stalled`. |

`max_fan_out_width` and the capacity bound are different statements and
both stand: inside the ceiling but over capacity is pacing, over the
ceiling is a refusal to start. Keep the workflow's own `max-tasks` seed
at or under the ceiling — a plan that overshoots costs the whole wave.

## Bindings the starter needs

`port-campaign` resolves three names through the project profile:

| Name | Kind | Contract |
|---|---|---|
| `build-and-test` | check | The deterministic gate. Exit status is the whole answer. |
| `wave-survey` | command | Must write `$AO_ENVELOPE` with `outputs.failures[]` (`{kind, location, detail}`) and `outputs.summary`. Non-zero exit on a red tree is expected. |
| `merge-unit-branches` | command | Merges the fan-out's unit branches into the item branch. Must write `outputs.conflicts[]` and `outputs.diff-base` (captured **before** the first merge). Non-zero exit on conflict. Bind `"{{units}}"` as an argv element to receive the units as JSON. |

Capacities: `campaign-workers`, `review-workers`, `validation-slot`.

A bound command that writes no envelope fails post-validation rather
than advancing on an invented contract — required outputs are never
synthesized.
