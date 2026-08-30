# Multi-agent campaigns with workflows

How to run a long, unattended, many-wave effort (a language port, a
framework migration, a test-coverage sweep) on the workflows system.
The mechanics are in [`../specs/workflows-system.md`](../specs/workflows-system.md);
this is the authoring layer above them: which shapes to reach for, why
each one exists, and how to drive the thing while it runs.

The shipped reference is a **pair** of starters, the campaign spine and
the task lane it calls. Scaffold both and edit them:

```
agent-overflow workflow new port-one-task --id port-one-task --project <slug>
agent-overflow workflow new port-campaign --id my-campaign  --project <slug>
agent-overflow workflow validate --id my-campaign --project <slug>
```

Scaffold the **lane first**, and under its own id: the spine's fan-out
calls it by name, and `workflow new` renames only the definition it is
creating. (It does rename a definition's calls to *itself*, which is how
the spine's next-wave edge follows `--id`.) If you rename the lane, edit
the one `call:` line in the spine to match. `workflow validate` names it
if you forget.

## One campaign is one call tree

A campaign is **one root run**. Each wave is one traversal of the spine's
graph, and the spine's last phase **calls itself** for the next wave, so
wave N+1 is a child run of wave N, every wave shares the root's worktree
and branch, and the whole campaign is a single tree you can pause, stop,
or discard as a unit (§3a).

```
root run ── wave 1 ── plan ─► implement ─► verify ─► next-wave ──┐
                        │        │                                │
                        │        └─ unit ─► port-one-task (child, own worktree)
                        │           unit ─► port-one-task
                        │           unit ─► port-one-task
                        │                                        │
                        └─ done (planner reported complete)      │
                                                                 ▼
                                              wave 2 ── plan ─► implement ─► …
```

Nothing chains it from outside: no automation, no cron, no
alternating-trigger pair. The loop is a call edge, which is the primitive
the spec gives for "loop with an exit condition" (§3a). Each wave is a
fresh child run with fresh loop counters, fresh budget accounting of its
own, and a **freshly resolved definition**.

### The spine, phase by phase

| Phase | Driver | Job |
|---|---|---|
| `plan` | agent, **Claude**, read-only | Read the campaign's state and either schedule this wave's tasks or report `complete`. Emits `tasks[]`, the next wave's number, whether a checkpoint is due, and the handoff to the next planner. |
| `implement` | fan-out over `plan.tasks`, **every unit is a call** | One `port-one-task` child run per task, each in its own sub-worktree on its own branch. Join is a **tool** that git-merges the branches. |
| `resolve-conflicts` | agent, write | The merge's fallthrough: the gate routes here on **any** non-passed merge, conflicts or not. A merge that failed for another reason still needs a human-shaped repair before `verify` sees the tree. It reads `implement.conflicts`. Unresolvable conflicts park. |
| `verify` | tool (`build-and-test`) | The integration gate: the whole wave, merged, on the campaign branch. |
| `integration-fix` | agent, write | Only on a red verify: what breaks when the tasks meet. Loops back to `verify`, bounded at 2, then parks. |
| `next-wave` | **call: itself** | The next wave. Skipped entirely when the planner reported `complete`. |

### The lane, phase by phase

| Phase | Driver | Job |
|---|---|---|
| `implement` | agent, **Codex**, write | Implement one task inside the unit's sub-worktree. |
| `review` | fan-out, **two lenses on two models** | A Codex *fidelity* lens and a Claude *consequence* lens, both told to refute. The join adjudicates their claims against the branch. |
| `fix` | agent, Codex, write | Applies the adjudicated findings, then loops back to `review` (bounded at 2). A fix is a change nobody has reviewed yet. |

**Fable plans, GPT works, both review.** The planner is Claude because
choosing what a campaign does next is the judgment call. The lanes are
Codex. The review phase runs one of each, because two copies of one model
miss the same things twice.

Two structural facts do most of the work:

- **The unit IS the isolation boundary, and the child is the work**
  (D35). A call-bound unit gets its own sub-worktree and runs a whole
  workflow inside it, so per-task review and fix happen on the task's own
  branch, before the merge, and the spine's `verify` is left doing the
  only job that needs the merged tree.
- **The implement join is a command, not an agent.** Nothing the lane
  *said* crosses into the campaign's next phase. The join emits the merged
  tree, the exit status, and its accounting of the wave: `merged[]` and
  `blocked[]`, the lanes it could not take with the reason each. Those two
  lists are the one thing `resolve-conflicts` needs and the one thing a merge
  tool can state as fact rather than as an account of its reasoning; the
  lane's prose still crosses nothing. Split context is a property of the graph,
  not of prompt wording. `implement.passed` is the tool driver's own output; the
  gate reads it and the phase never declares it.

## Patterns, and why

**Adversarial review.** Reviewers are prompted to *break* the change, not
to approve it. An approval prompt gets approval: the cheapest way to
satisfy "does this look right" is to say yes. Pair it with **split
context**: a reviewer sees the branch and the task entry, never the
implementer's reasoning. A reviewer handed the rationale is grading an
argument, and a plausible argument is exactly what a wrong change comes
with.

**Decorrelate by model *and* by lens.** Identical reviewers find the same
things and miss the same things, so the second one costs tokens and buys
nothing. The lane's two reviewers differ on both axes: different model,
and different question (did the behavior land / what does this do to
everything else). The adjudicating join then treats every claim as a
**lead, not a verdict** and reproduces it against the branch before it
becomes a finding. Otherwise the fix phase spends the task on things
that were never wrong.

**Review after the fix, too.** The lane loops `fix → review`, not `fix →
done`. The fix is a change like any other and nobody has looked at it.

**Loop until dry, not for N waves.** Discovery of unknown size does not
finish on a schedule. The campaign keeps calling waves while the planner
schedules work and ends when it reports `complete`, which is expressed
as the run finishing *without* taking the self-call, so the whole tree
unwinds wave by wave. A fixed wave count either stops early with a tail
of missed work or burns budget on empty waves.

**Completeness critic.** `plan` is the next round's critic: it reads the
tree the last wave left and asks what is still missing. Everything that
landed is in the workspace (the campaign branch is shared by every
wave), so the planner's evidence is the repository, not a report. What the
repository *cannot* show (work deliberately deferred, ordering decisions,
risks noticed and not addressed) travels in `carry-forward`, the one
string each planner writes for the next one.

**No silent caps.** Every phase that bounds its own coverage reports the
bound: `plan` emits `carry-forward`, the review join emits `unreviewed`.
A phase that quietly truncates reads downstream as full coverage, and a
gap that looks like coverage survives every later wave. The engine takes
the same line: `max_fan_out_width` **refuses** an over-wide expansion
rather than running the first N (D29).

**Humans change rules, not code.** Do not hand-fix what a wave produced.
The next wave regresses it and you never learn the rule was wrong. Change
what the campaign *decides*; see steering, next.

## Steering a running campaign

Three knobs, and the difference between them is *when* they take effect.

**Prompt files: next wave.** Every call resolves its target from disk,
definition **and prompt bodies**, per invocation (§3a). So editing
`my-campaign-plan.md` while wave 7 runs changes wave 8. The wave in
flight keeps what it started with, which is what freezing is for.

**Repo context files: next wave.** The planner is told to read the
project's own campaign notes (a porting document, a checklist, a tracking
file) out of the workspace. They are ordinary files on the campaign
branch, editable any time, and the next planner reads whatever is there.
This is the fastest steering path and the one that leaves a record in the
repository.

**Workflow inputs: campaign start only.** `campaign-goal`, `max-tasks`,
`job-notes`, and `checkpoint-every` are seeded at the root and passed
down the self-call unchanged. They are the campaign's standing brief; to
change one, stop the tree and start a new campaign from the branch it
built.

Two more controls exist for *stopping*, not steering:

**Stop after this wave.** `agent-overflow run soft-stop <root-run-id>`
arms a standing request on the root; the next wave boundary parks the run
`needs-human(checkpoint)` instead of calling (D36). Nothing in flight is
interrupted. The wave that is running finishes. `--clear` withdraws the
request. `agent-overflow run resume <run-id>` takes the call the park
skipped. A workflow with no call edge has no boundary to stop at, so the
request is accepted and simply never fires; the campaign spine has one,
at `next-wave`.

**Scheduled checkpoints.** `--seed checkpoint-every=5` makes the planner
mark every fifth wave, and `verify`'s gate turns that into a **human
route**: approve continues into the next wave, reject carries your note
back into a fresh plan for this wave (bounded at 2). `0` never asks. It
is the standing-appointment version of soft-stop, for a campaign you want
to look at periodically without watching it.

## Operating a running campaign

**From a chat session.** Type `/workflow` anywhere in a message: it
expands at send into the project's workflow sources, the available
definitions, the active runs, and whether `agent-overflow` resolved on
PATH (D31). That block is what an agent reads before it types anything.
Every verb below works from any session in the project.

```
agent-overflow run start my-campaign --seed campaign-goal='...' \
  --seed max-tasks=6 --seed wave-number=1 --seed checkpoint-every=0
agent-overflow run list --active
agent-overflow run status <run-id>
agent-overflow run output <run-id>          # declared workflow outputs
agent-overflow run soft-stop <run-id> [--clear]
agent-overflow run pause <run-id>
agent-overflow run resume <run-id> [--phase <id>]
agent-overflow run retry-failed-units <run-id> [--note '...']
agent-overflow run retry-unit <run-id> <unit-id> [--note '...']
agent-overflow run rerun <run-id> [--guidance '...']
agent-overflow run cancel <run-id>
```

`wave-number` starts at 1 and every later wave is handed its number by
the wave before it. Keep `max-tasks` at or under the project's
`max_fan_out_width`. A plan that overshoots costs the whole wave.

**Monitoring from the bound thread.** A run started from a chat session
is bound to that thread (§5), and the binding is on the ROOT. When
anything in the tree needs a human, the wake message arrives in that
thread and **names the run to act on**, which matters here, because the
run that parked is usually a descendant many waves down and the verb has
to be aimed at it, not at the root you started. Descendant runs never
bind threads and never notify on their own; they surface through the
root's binding and through the run tree in the overlay.

**Which run takes which verb.**

| Verb | Aim it at |
|---|---|
| `soft-stop`, `pause`, `cancel` | the **root**. All three act on the whole tree, and the engine refuses them on a called run, naming the run that called it. |
| `resume`, `retry-unit`, `retry-failed-units`, `rerun`, answering a question | the run that **parked**: the id in the wake message. |

**Usage limits.** A wide fan-out hits a provider wall and most of its
units fail against it within a minute. The wave parks
`needs-human(unit-failed)`. Wait for the reset or switch account, then
repair the whole attempt in one command:

```
agent-overflow run retry-failed-units <run-id> --note 'quota reset'
```

One engine command, not N (D33): the failed set is collected before the
first write, so no other command sees a half-repaired fan-out. Repaired
units re-queue through normal admission: twenty units against a provider
bound of two starts two. Units under human takeover are left alone. The
single-unit `run retry-unit` stays for the other shape: one unit failing
on its own merits. From a bound thread the credential that reaches these
verbs is the thread's own, which is why a descendant's park is actionable
from the thread that started the campaign at all.

**Take over.** A phase you want to drive yourself: open its thread from
the run tree and send a message. That interrupts the turn and hands you
the worktree; finish with **Complete** (one schema-attached finalize turn
becomes the phase's envelope) or discard and re-run the phase. A call
unit has no session to steer. Take over the **child run's** phase
instead.

**A parked descendant stops the campaign.** A wave waiting on a parked
child is `running` and holds nothing; the tree simply does not advance
until you clear the park. That is the intended behavior for
`campaign-stalled`, `merge-unresolved`, `wave-verification-failed`, and
`review-unresolved`.

**Disposition is manual.** The campaign branch is the deliverable, and
nothing merges it to a base branch on its own. The starter sets `cleanup:
manual`; every wave shares the root's worktree, so a long campaign is one
checkout, not forty.

**Bounds worth setting before you walk away.**

| Knob | Where | Default |
|---|---|---|
| `max_fan_out_width` | project profile | **32.** Absolute ceiling on units per fan-out attempt. Refuses, never truncates (D29). Per project, never per workflow. |
| `reliability.per_item_budget` | project profile | **None.** Exactly one of `tokens`, `usd`, `wall_clock`. Budgets are enforced against the **root** across the whole tree (§12), so one ceiling bounds the entire campaign. An unattended campaign is exactly the case that wants one. |
| `capacities.provider:codex` / `provider:claude` | project profile | 2. How many agent turns run at once, across every wave and lane. Throttles; does not refuse. |
| `reliability.watchdog` | project profile | Inactivity kill for a silent phase; parks `stalled`. |

`max_fan_out_width` and the capacity bound are different statements and
both stand: inside the ceiling but over capacity is pacing, over the
ceiling is a refusal to start. An expansion wider than the capacity its
units will contend on says so in the app log at expansion time. A wave
of eight against a provider bound of two runs two at a time, which from
the outside looks like nothing more than a slow provider.

**Depth.** The spine declares `max_depth: 200` on its self-call (the
campaign's wave ceiling) under the engine's absolute `MaxCallDepth` of
256, with room for each wave's lanes below it. Raise it and you approach
a limit that parks `needs-human(wiring-error)`; a campaign that long
wants a new root anyway.

## Bindings the starters need

`port-campaign` resolves two names through the project profile:

| Name | Kind | Contract |
|---|---|---|
| `build-and-test` | check | The integration gate on the merged campaign branch. Exit status is the whole answer. |
| `merge-unit-branches` | command | Merges the fan-out's unit **branches** into the item branch. A lane's work has to be COMMITTED on its branch by join time or the merge consumes nothing. Must write the phase's two declared outputs to `$AO_ENVELOPE`: `outputs.merged[]` (the unit ids it took) and `outputs.blocked[]` (`{unit, reason}` for every unit it did not). Non-zero exit when a human still has to land something. Bind `"{{units}}"` as an argv element to receive the units as JSON: `id`, `status`, `branch`, `worktree`, `commitsAhead`, `dirty` per entry. |

The join declares `accounts_for_units: true`, so those two lists are not a
reporting convention. The engine post-validates a `done` join envelope against
the exact set of unit ids the join was shown, and every one of them must appear
in exactly one list, named once, with a non-blank reason where it is blocked. A
unit a human **dropped** is in that set: the engine shows dropped lanes to the
join so it can say what it did not receive, so the script accounts for one in
`blocked` with a reason naming the drop, and does *not* exit non-zero for it.
The drop was the decision, and routing the wave to `resolve-conflicts` over it
would ask somebody to reverse themselves. An entry the engine could read no id
from is the one thing left out of both lists, because naming it would be refused;
the reference script reports it on stderr and fails the gate instead.

**The commit contract, and how a merge script checks it.** Every writing
element (phase, work unit, and join alike) is told by the system prompt
suffix to leave its work committed on its branch before it finishes, because
nothing in the engine ever commits and everything downstream reads the branch:
later phases resume on it, unit worktrees are cut from it, this join merges it,
and a done join then retires the unit checkouts. The `units` JSON carries the
two facts that say whether a lane honored it:

| Field | Meaning |
|---|---|
| `commitsAhead` | Commits on the unit's branch that the item branch does not have. `0` means the lane produced nothing to merge. Absent for a unit that never got a branch (never started, dropped early). |
| `dirty` | Whether the unit's worktree still holds uncommitted or untracked files. Absent once the checkout has been retired. Absent is "no answer", not "clean". |

A merge script decides what to do with them: refuse the wave, skip an empty
lane, auto-commit a dirty one on its branch before merging, or account for it in
`blocked[]`. Doing nothing is also a choice: worktree retirement after a done
join is non-force, so a lane that left work uncommitted keeps its checkout and
is named on the `workflow:error` channel instead of having it deleted.

Capacity: `validation-slot`, held by `verify`.

`port-one-task` binds **nothing**. Its phases are all agent phases, so
the only resource they take is the implicit `provider:<name>` bound every
agent phase acquires.

A bound command that writes no envelope fails post-validation rather than
advancing on an invented contract. Required outputs are never
synthesized.
