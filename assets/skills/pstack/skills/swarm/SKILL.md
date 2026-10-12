---
name: swarm
description: "Fan out N parallel workers, drain them, and return one report. Use for /swarm, 'swarm this', or parallel coverage, races, gauntlets, and exploration."
disable-model-invocation: true
---

# Swarm

Fan out N parallel workers. They may cover separate slices, race the same brief, or mix both. The parent waits, aggregates, and returns one report.

## Start

Open a todolist with one entry per phase before launching anything.

1. Frame
2. Fan out
3. Aggregate
4. Report

## Phase A: Frame

1. State the done predicate and the artifact or report the swarm must return.
2. Choose the shape. Partition into slices, race N workers on identical briefs, or mix both. For a race or mixed shape, declare `first pass`, `rank all`, or `best-of` before spawning.
3. Set N from the user or derive it from the shape. N is total workers, not how many run at once.
4. Pick the worker model from the `swarm workers` line in the pstack settings file (`~/.agents/pstack-models.md`, written by the **setup-pstack** skill). If the file or that line is missing, use `inherit-parent`. Resolve it as the **setup-pstack** skill's *Reading a role* section says. For a model race, name each arm's model up front.
5. Give each worker its own writable output when it writes. When workers verify or measure commits, each brief names the exact SHAs. A measurement brief also names the method (sample count, what one sample is, order). The worker records both in its result.

## Phase B: Fan out

Spawn all N workers in one message as native subagents that run without blocking you, each on the step 4 model. A worker on the other provider is a thread (see **Spawning**). Each worker that writes gets its own worktree: the subagent tool's worktree isolation where it has one, or a `thread_spawn` with `worktree` when the worker writes on a branch it must push.

When a worker must start from a pushed branch other than the default, name the branch in its brief, or pass it as the thread's `worktree`.

**Spawning.** A role on your own provider runs as a native subagent (`Agent` in Claude, `spawn_agent` in Codex). A role on the other provider runs as a thread through `thread_spawn` (see **setup-pstack**, *Reading a role*). Keep the prompt and the model. Drop parameters your tool doesn't have. If your provider has no subagent tool in this turn, run each worker yourself, one after another.

Every brief stands alone. Include the goal, scope, exact slice or race arm, how to verify, and what to report. Reports use `PASS`, `ISSUES`, or `BLOCKED` with evidence. A worker that can prove a defect reports `ISSUES` and lists every issue it can prove, not only the first.

If a worker drops out, proceed with N-1 and note it.

## Phase C: Aggregate

Read the terminal results. Drop a result that does not record the SHAs and method its brief names, and respawn that worker once. After a second miss, record a gap. A gap does not count as a pass. For coverage, every required slice needs a result. For a race, apply the selection rule declared up front. Use first pass, rank all, or best-of. Do not paste raw worker dumps.

Keep a compact result table, one-line evidenced issues, and explicit gaps or dropouts.

## Phase D: Report

Return one consolidated in-chat report with the table, issue one-liners, gaps or dropouts, and the race rule when used.
