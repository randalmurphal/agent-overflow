import { existsSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

import { test, expect, type HarnessMockEvent } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import {
  doneResult,
  seedWorkflow,
  seedWorkflowProject,
  setClaudeScenario,
  setCodexScenario,
  setGlobalPause,
  singlePhaseWorkflow,
  start,
  stuckEnvelope,
  waitForWorkflowState,
  type WorkflowDetail,
} from './workflows-helpers.js';

// Thread binding, wake, pause/resume, and discard (spec §5, decisions D17/D23)
// over the real engine and real git checkouts. Every surface here is RPC-only:
// a run reports back into a conversation thread, a human can stop and continue
// it on the session it parked on, and a discard destroys exactly what its
// preview said it would.

interface ThreadItem {
  id: string;
  kind: string;
  role: string;
  summary: string;
}

interface DiscardWorktree {
  itemId: string;
  unitId?: string;
  path: string;
  branch: string;
  base: string;
  present: boolean;
  registered: boolean;
  dirtyFiles: string[];
  dirtyFileCount: number;
  unmergedCommitCount: number;
  error?: string;
}

interface DiscardPreview {
  itemId: string;
  members: string[];
  liveMembers: string[];
  worktrees: DiscardWorktree[];
}

// waitForUserMessage polls the thread timeline for an injected wake. The wake
// is delivered off the engine's command loop, so it lands after the state event
// a spec awaits rather than with it.
async function waitForUserMessage(
  harness: HarnessApp,
  threadId: string,
  match: (summary: string) => boolean,
): Promise<string> {
  const deadline = Date.now() + 15_000;
  let seen: ThreadItem[] = [];
  while (Date.now() < deadline) {
    seen = await harness.rpc<ThreadItem[]>('ListItems', threadId);
    const wake = seen.find((item) => item.kind === 'user_text' && match(item.summary));
    if (wake) return wake.summary;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error(
    `thread ${threadId} never received the expected user message: ${JSON.stringify(seen)}`,
  );
}

// A workflow that declares an output, because a wake reports the run's declared
// outputs rather than the phase envelope it came from.
const wakeFlowYaml = `id: wake-flow
name: Wake flow
inputs:
  goal:
    schema:
      type: string
outputs:
  verdict:
    from: run.complete
phases:
  - id: run
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: wake-flow.md
    access: read-only
    inputs:
      goal:
        schema:
          type: string
    outputs:
      complete:
        schema:
          type: boolean
    gate:
      routes:
        - to: done
cleanup: manual
`;

test('a bound run wakes its origin thread when it rests', async ({ harness }) => {
  await setClaudeScenario(harness, 'wake-bound', [
    { steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  const project = await seedWorkflow(
    harness,
    'workflow-wake-project',
    'wake-flow',
    wakeFlowYaml,
  );
  // The binding needs the run id, which only exists once the run starts. Global
  // pause holds the first phase so the spec can bind before anything rests.
  await setGlobalPause(harness, true);
  const item = await start(harness, project.projectId, 'wake-flow');
  const thread = await harness.rpc<{ id: string; mode: string }>('CreateThread', {
    projectId: project.projectId,
    title: 'Wake origin',
    provider: 'claude',
    model: 'claude-opus-4-7',
    mode: 'chat',
  });

  const bound = await harness.rpc<{ id: string; originThreadId: string }>(
    'WorkflowBindThread',
    item.id,
    thread.id,
  );
  expect(bound.originThreadId).toBe(thread.id);

  await setGlobalPause(harness, false);
  await waitForWorkflowState(harness, item.id, 'done');

  const wake = await waitForUserMessage(harness, thread.id, (summary) =>
    summary.includes(`Run "${item.id}"`),
  );
  expect(wake).toContain('untrusted run data');
  expect(wake).toContain('is done');
  expect(wake).toContain('Outputs:');
  expect(wake).toContain('"verdict": "true"');
  expect(wake).toContain('nothing is waiting on a reply');
  // A wake is a pointer, never a dump of the run record.
  expect(wake).not.toContain('gate_trace');

  // Unbinding is the off switch: the next resting transition reports nowhere.
  const unbound = await harness.rpc<{ originThreadId?: string }>(
    'WorkflowUnbindThread',
    item.id,
  );
  expect(unbound.originThreadId ?? '').toBe('');
});

test('a paused run resumes on the provider session it parked on', async ({ harness }) => {
  // Turn 1 is the one the pause interrupts; turn 2 is what the resumed attempt
  // sends to the very same session, which is the point of the spec.
  await setClaudeScenario(harness, 'wake-pause', [
    {
      label: 'interrupted',
      steps: [
        { waitSignal: { name: 'hold-running' } },
        { emit: { lines: [doneResult({ complete: true })] } },
      ],
    },
    { label: 'resumed', steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  const project = await seedWorkflow(
    harness,
    'workflow-pause-project',
    'pause-flow',
    singlePhaseWorkflow('pause-flow', '        - to: done'),
  );
  const item = await start(harness, project.projectId, 'pause-flow');
  const registered = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) => event.scenario === 'wake-pause' && event.report.kind === 'registered',
  );
  await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) =>
      event.mockId === registered.mockId &&
      event.report.kind === 'waiting_signal' &&
      event.report.detail === 'hold-running',
  );

  // Pause stops the turn now rather than at the next phase boundary — that is
  // what makes it usable before a quit.
  await harness.rpc('WorkflowPauseItem', item.id);
  await waitForWorkflowState(harness, item.id, 'needs-human', 'paused');
  await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) => event.mockId === registered.mockId && event.report.kind === 'turn_interrupted',
  );
  const parked = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(parked.item.reason).toBe('paused');
  const parkedThread = parked.phases[0]?.threadId ?? '';
  expect(parkedThread).toBeTruthy();

  await harness.rpc('WorkflowResumeItem', item.id, '');
  // The continuation is turn 2 of the session that parked — not a new session
  // replaying the phase, which is what keeps the provider's history intact.
  const secondTurn = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) =>
      event.mockId === registered.mockId &&
      event.report.kind === 'turn_started' &&
      event.report.turn === 2,
  );
  expect(secondTurn.mockId).toBe(registered.mockId);
  await waitForWorkflowState(harness, item.id, 'done');

  const resumed = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(resumed.item.state).toBe('done');
  expect(resumed.phases).toHaveLength(2);
  expect(resumed.phases[1]?.threadId).toBe(parkedThread);
});

// A fan-out whose codex unit fails parks the run with the claude unit's writing
// checkout still on disk — the state a human is actually looking at when they
// decide whether the work is worth keeping.
const discardFanOutYaml = `id: discard-flow
name: Discard flow
inputs:
  goal:
    schema:
      type: string
phases:
  - id: port
    shape: fan-out
    outputs:
      report:
        schema:
          type: string
    fan_out:
      - id: alpha
        provider: claude
        model: claude-opus-4-7
        prompt: alpha.md
        access: write
        outputs:
          report:
            schema:
              type: string
      - id: beta
        provider: codex
        model: gpt-5.5
        prompt: beta.md
        access: read-only
    join:
      id: merge
      provider: claude
      model: claude-opus-4-7
      prompt: merge.md
      access: read-only
    gate:
      routes:
        - to: done
cleanup: manual
`;

test('discard previews the loss and then removes every checkout and branch', async ({
  harness,
}) => {
  await setClaudeScenario(harness, 'discard-claude', [
    { steps: [{ emit: { lines: [doneResult({ report: 'ok' })] } }] },
  ]);
  await setCodexScenario(harness, 'discard-codex-stuck', stuckEnvelope('beta slice does not build'));
  const project = await seedWorkflowProject(
    harness,
    'workflow-discard-project',
    [
      {
        name: 'discard-flow',
        yaml: discardFanOutYaml,
        prompts: {
          'alpha.md': 'Port the alpha slice and return the envelope.',
          'beta.md': 'Port the beta slice and return the envelope.',
          'merge.md': 'Consolidate {{units}} and return the envelope.',
        },
      },
    ],
    [],
    'base_branch: main\n',
  );

  const item = await start(harness, project.projectId, 'discard-flow');
  await waitForWorkflowState(harness, item.id, 'needs-human', 'unit-failed');

  const parked = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  const alpha = parked.units.find((unit) => unit.unitId === 'alpha');
  if (!alpha?.worktreePath || !alpha.branch) {
    throw new Error(`alpha unit has no checkout: ${JSON.stringify(parked.units)}`);
  }
  const runWorktree = parked.item.worktreePath ?? '';
  expect(runWorktree).toBeTruthy();
  // Uncommitted work in the unit's checkout exists nowhere else; the preview is
  // the only place a human can see it before it is gone.
  writeFileSync(join(alpha.worktreePath, 'unsaved.txt'), 'work in progress\n');

  const preview = await harness.rpc<DiscardPreview>('WorkflowDiscardPreview', item.id);
  expect(preview.members).toEqual([item.id]);
  expect(preview.liveMembers).toEqual([]);
  const unitRow = preview.worktrees.find((row) => row.unitId === 'alpha');
  if (!unitRow) {
    throw new Error(`preview has no alpha row: ${JSON.stringify(preview.worktrees)}`);
  }
  expect(unitRow.present).toBe(true);
  expect(unitRow.registered).toBe(true);
  expect(unitRow.branch).toBe(alpha.branch);
  // A unit branch is measured against the run's branch, not the repo base.
  expect(unitRow.base).toBe(parked.item.branch);
  expect(unitRow.dirtyFileCount).toBe(1);
  expect(unitRow.dirtyFiles.join(' ')).toContain('unsaved.txt');
  expect(unitRow.error ?? '').toBe('');
  expect(preview.worktrees.some((row) => row.path === runWorktree)).toBe(true);
  // Previewing is read-only.
  expect(existsSync(alpha.worktreePath)).toBe(true);

  const receipt = await harness.rpc<{
    action: string;
    discarded?: {
      members: string[];
      removedWorktrees: string[];
      deletedBranches: string[];
    };
  }>('WorkflowDiscardItem', item.id);
  expect(receipt.action).toBe('discarded');
  expect(receipt.discarded?.members).toEqual([item.id]);
  expect(receipt.discarded?.removedWorktrees).toContain(alpha.worktreePath);
  expect(receipt.discarded?.deletedBranches).toContain(alpha.branch);
  expect(receipt.discarded?.deletedBranches).toContain(parked.item.branch);

  expect(existsSync(alpha.worktreePath)).toBe(false);
  expect(existsSync(runWorktree)).toBe(false);
  const branches = await harness.rpc<Array<{ name: string }>>(
    'GitListBranchesForProject',
    project.projectId,
  );
  const names = branches.map((branch) => branch.name);
  expect(names).not.toContain(alpha.branch);
  expect(names).not.toContain(parked.item.branch);
  expect(names).toContain('main');

  // The run record survives its checkouts; only the pointers are dropped.
  const after = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(after.item.worktreePath ?? '').toBe('');
  expect(after.units.every((unit) => !unit.worktreePath)).toBe(true);
  const empty = await harness.rpc<DiscardPreview>('WorkflowDiscardPreview', item.id);
  expect(empty.worktrees).toEqual([]);
});
