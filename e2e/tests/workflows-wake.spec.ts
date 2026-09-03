import { existsSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

import { test, expect, type HarnessMockEvent } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import {
  assistantText,
  callChildren,
  doneResult,
  seedWorkflow,
  seedWorkflowProject,
  setClaudeScenario,
  setCodexScenario,
  setGlobalPause,
  singlePhaseWorkflow,
  start,
  stuckEnvelope,
  waitForWorkflowProviderInput,
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
    // A thread with no items yet answers `null`, not `[]`. Polling before the
    // first item lands is the normal case here — the wake is delivered off the
    // engine's command loop — so treat it as "nothing yet" and keep waiting
    // rather than crashing on the first poll and losing the timeout entirely.
    seen = (await harness.rpc<ThreadItem[] | null>('ListItems', threadId, true)) ?? [];
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
    {
      steps: [
        {
          emit: {
            lines: [
              // The phase is read-only, so this message IS its narrative: the
              // runner recovers it into the attempt's file, and the wake can
              // then carry a reference that resolves.
              assistantText('I checked both callers and neither needs the flag'),
              doneResult({ complete: true }),
            ],
          },
        },
      ],
    },
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
  // The narrative reference resolves because the runner recovered the phase's
  // message into the file. A read-only phase never writes one itself, so before
  // that recovery this reference pointed at a path nothing had created.
  expect(wake).toContain('"narrative"');
  // A wake is a pointer, never a dump of the run record.
  expect(wake).not.toContain('gate_trace');
  expect(wake).not.toContain('neither needs the flag');

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
  const firstInput = await waitForWorkflowProviderInput(harness, 'claude');
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

  await harness.rpc('WorkflowResumeItem', item.id, '', false);
  const resumedInput = await waitForWorkflowProviderInput(harness, 'claude');
  expect(resumedInput.sessionRef).toBe(firstInput.sessionRef);
  expect(resumedInput.input).toContain('Resume the current workflow phase');
  expect(resumedInput.input).not.toContain('<workflow-system-instructions>');
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
    'GitListBranches',
    { projectId: project.projectId, workspacePath: '' },
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

// A campaign is one run per wave: the root's last phase calls the same workflow
// again, so the tree grows downwards and the run a human is watching is almost
// never the one that stops. These two behaviours are the pair that makes that
// shape supervisable — a stop that lands at a wave boundary instead of mid-turn
// (D36), and a park deep in the tree that reports at the ROOT's bound thread.
const campaignYaml = `id: campaign-flow
name: Campaign flow
inputs:
  goal:
    schema:
      type: string
outputs:
  more:
    from: wave.more
phases:
  - id: wave
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: campaign-flow.md
    access: read-only
    inputs:
      goal:
        schema:
          type: string
    outputs:
      more:
        schema:
          type: boolean
    gate:
      routes:
        - when:
            eq:
              ref: wave.more
              value: true
          to: advance
        - to: done
  - id: advance
    shape: call
    call: campaign-flow
    args:
      goal: goal
    max_depth: 4
    gate:
      routes:
        - to: done
cleanup: manual
`;

// heldWave registers the next wave's mock and returns once it is parked on the
// signal, which is the only moment a spec can act on a run tree with nothing
// in flight and nothing yet decided.
async function heldWave(harness: HarnessApp, seen: Set<string>): Promise<string> {
  const registered = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) =>
      event.scenario === 'campaign-wave' &&
      event.report.kind === 'registered' &&
      !seen.has(event.mockId),
  );
  seen.add(registered.mockId);
  await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) =>
      event.mockId === registered.mockId &&
      event.report.kind === 'waiting_signal' &&
      event.report.detail === 'hold-wave',
  );
  return registered.mockId;
}

test('a soft stop parks a campaign at its next call boundary and the root hears about it', async ({
  harness,
}) => {
  // Every wave holds on the same signal, so each one is released by the spec
  // rather than by timing. The scenario is swapped for the terminating one
  // before the last wave's session exists.
  await setClaudeScenario(harness, 'campaign-wave', [
    {
      steps: [
        { waitSignal: { name: 'hold-wave' } },
        { emit: { lines: [doneResult({ more: true })] } },
      ],
    },
  ]);
  const project = await seedWorkflow(
    harness,
    'workflow-campaign-project',
    'campaign-flow',
    campaignYaml,
  );

  await setGlobalPause(harness, true);
  const root = await start(harness, project.projectId, 'campaign-flow');
  const thread = await harness.rpc<{ id: string }>('CreateThread', {
    projectId: project.projectId,
    title: 'Campaign origin',
    provider: 'claude',
    model: 'claude-opus-4-7',
    mode: 'chat',
  });
  await harness.rpc('WorkflowBindThread', root.id, thread.id);
  await setGlobalPause(harness, false);

  // Wave 1 runs to its call boundary with no request standing, so the root
  // calls: the stop must not fire on a boundary that was crossed before it was
  // asked for.
  const seen = new Set<string>();
  const firstWave = await heldWave(harness, seen);
  await harness.rpc('HarnessMockCommand', firstWave, { type: 'advance', name: 'hold-wave' });

  // Wave 2 exists and is mid-turn. Arming here proves the request never
  // interrupts work in flight — this turn finishes before anything stops.
  const secondWave = await heldWave(harness, seen);
  const child = await harness.rpc<WorkflowDetail>('WorkflowGetItem', root.id);
  const rootChildren = await callChildren(harness, root.id);
  expect(rootChildren).toHaveLength(1);
  const childId = rootChildren[0]!.itemId;
  expect(child.callPhaseIds).toEqual(['advance']);

  await harness.rpc('WorkflowRequestSoftStop', root.id, true);
  // Setting it twice is one request, not two.
  await harness.rpc('WorkflowRequestSoftStop', root.id, true);
  const armed = await harness.rpc<WorkflowDetail>('WorkflowGetItem', root.id);
  expect(armed.item.softStop).toBe(true);
  expect(armed.item.state).toBe('running');

  await harness.rpc('HarnessMockCommand', secondWave, { type: 'advance', name: 'hold-wave' });

  // The turn that was in flight finished, and THEN the boundary refused to call.
  await waitForWorkflowState(harness, childId, 'needs-human', 'checkpoint');
  const parked = await harness.rpc<WorkflowDetail>('WorkflowGetItem', childId);
  expect(parked.phases.map((phase) => phase.phaseId)).toEqual(['wave', 'advance']);
  expect(parked.phases[0]?.status).toBe('completed');
  expect(parked.phases[1]?.status).toBe('parked');
  expect(await callChildren(harness, childId)).toHaveLength(0);

  // The root is still running — it is waiting on the call it made — so the park
  // reaches a human through the root's bound thread, not the child's silence.
  const wake = await waitForUserMessage(harness, thread.id, (summary) =>
    summary.includes('Call chain:'),
  );
  expect(wake).toContain(`Run "${root.id}"`);
  expect(wake).toContain(`Call chain: "${root.id}" → "${childId}".`);
  expect(wake).toContain('needs-human (checkpoint)');
  expect(wake).toContain('the stop that was asked for, not a failure');
  // The literal command, against the CHILD's id (D38): the run to act on is one
  // the reader has never seen, and "resume" is one of four control verbs it
  // could otherwise be mapped onto.
  expect(wake).toContain(
    `\`agent-overflow run resume "${childId}"\` takes the call it skipped, or leave it parked.`,
  );

  // The boundary consumed the request, so a resume continues the campaign
  // instead of stopping at the very next wave again.
  const afterPark = await harness.rpc<WorkflowDetail>('WorkflowGetItem', root.id);
  expect(afterPark.item.softStop).toBe(false);
  expect(afterPark.item.state).toBe('running');

  await setClaudeScenario(harness, 'campaign-wave', [
    { steps: [{ emit: { lines: [doneResult({ more: false })] } }] },
  ]);
  await harness.rpc('WorkflowResumeItem', childId, '', false);
  await waitForWorkflowState(harness, root.id, 'done');

  const resumed = await harness.rpc<WorkflowDetail>('WorkflowGetItem', childId);
  expect(resumed.item.state).toBe('done');
  // Resume took the edge the stop skipped: wave 3 exists and it is the child's.
  const resumedChildren = await callChildren(harness, childId);
  expect(resumedChildren).toHaveLength(1);
  const grandchild = await harness.rpc<WorkflowDetail>(
    'WorkflowGetItem',
    resumedChildren[0]!.itemId,
  );
  expect(grandchild.item.state).toBe('done');
  expect(grandchild.item.callDepth).toBe(2);
  expect(await callChildren(harness, grandchild.item.id)).toHaveLength(0);
});
