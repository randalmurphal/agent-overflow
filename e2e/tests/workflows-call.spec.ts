import { test, expect } from './fixtures.js';
import {
  callChildren,
  doneEnvelope,
  doneResult,
  seedWorkflowProject,
  setClaudeScenario,
  setCodexScenario,
  start,
  waitForWorkflowState,
  type WorkflowDetail,
} from './workflows-helpers.js';

// A `shape: call` phase invokes another workflow (spec §3a). The child is a
// real run — its own row, its own phases, its own threads — linked into the
// caller's tree, and it executes in the caller's workspace rather than
// provisioning one of its own (§9). These specs drive the real engine and
// runner end to end.

// The caller writes, so the run gets a worktree; the child must land in that
// same worktree instead of cutting a second one.
const callerYaml = `id: call-parent
name: Call parent
inputs:
  goal:
    schema:
      type: string
phases:
  - id: prepare
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: prepare.md
    access: write
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
        - to: audit
  - id: audit
    shape: call
    call: call-child
    args:
      subject: goal
    gate:
      routes:
        - when:
            eq:
              ref: audit.verdict
              value: true
          to: report
        - park: child-said-no
  - id: report
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: report.md
    access: read-only
    inputs:
      audit.verdict:
        schema:
          type: boolean
    outputs:
      complete:
        schema:
          type: boolean
    gate:
      routes:
        - to: done
cleanup: manual
`;

// The child's declared outputs are the caller's whole downstream surface: the
// call phase's envelope carries exactly these names.
const childYaml = `id: call-child
name: Call child
inputs:
  subject:
    schema:
      type: string
outputs:
  verdict:
    from: inspect.complete
phases:
  - id: inspect
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: inspect.md
    access: read-only
    inputs:
      subject:
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

test('a call phase runs its child in the caller workspace and completes on the child outputs', async ({
  harness,
}) => {
  await setClaudeScenario(harness, 'call-parent', [
    { steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  const project = await seedWorkflowProject(
    harness,
    'workflow-call-project',
    [
      {
        name: 'call-parent',
        yaml: callerYaml,
        prompts: {
          'prepare.md': 'Prepare {{goal}} and return the envelope.',
          'report.md': 'Report on {{audit.verdict}} and return the envelope.',
        },
      },
      {
        name: 'call-child',
        yaml: childYaml,
        prompts: { 'inspect.md': 'Inspect {{subject}} and return the envelope.' },
      },
    ],
    [],
    'base_branch: main\n',
  );

  const item = await start(harness, project.projectId, 'call-parent');
  await waitForWorkflowState(harness, item.id, 'done');

  const detail = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(detail.phases.map((phase) => phase.phaseId)).toEqual(['prepare', 'audit', 'report']);
  // The call phase runs no turn of its own, so it holds no thread; its envelope
  // is synthesized from the child's declared outputs.
  const audit = detail.phases[1];
  expect(audit?.threadId ?? '').toBe('');
  expect(audit?.status).toBe('completed');
  expect(audit?.outputEnvelope).toMatchObject({ status: 'done', outputs: { verdict: true } });

  const children = await callChildren(harness, item.id);
  expect(children).toHaveLength(1);
  const child = children[0];
  expect(child?.workflowId).toBe('call-child');
  expect(child?.state).toBe('done');
  expect(child?.parentPhaseId).toBe('audit');
  expect(child?.parentAttempt).toBe(1);
  expect(child?.callDepth).toBe(1);

  // The child is an ordinary run with its own phase thread, and it points back
  // at the exact invocation that created it.
  const childDetail = await harness.rpc<WorkflowDetail>('WorkflowGetItem', child!.itemId);
  expect(childDetail.item.parentItemId).toBe(item.id);
  expect(childDetail.item.parentPhaseId).toBe('audit');
  expect(childDetail.item.callDepth).toBe(1);
  expect(childDetail.phases.map((phase) => phase.phaseId)).toEqual(['inspect']);
  expect(childDetail.phases[0]?.threadId).toBeTruthy();
  expect(childDetail.phases[0]?.threadId).not.toBe(detail.phases[0]?.threadId);

  // Workspace flows down: one worktree for the tree, cut by the root.
  expect(detail.item.worktreePath).toBeTruthy();
  expect(childDetail.item.worktreePath).toBe(detail.item.worktreePath);
  expect(childDetail.item.branch).toBe(detail.item.branch);
  expect(await callChildren(harness, child!.itemId)).toHaveLength(0);
});

// A self-calling workflow whose recursion terminates on its own, inside the
// `max_depth` its call edge declares. The tool check counts its invocations in
// a file in the workspace, which is also the proof that every level of the
// recursion ran in the *same* workspace.
const recursiveYaml = `id: call-recurse
name: Call recurse
inputs:
  goal:
    schema:
      type: string
outputs:
  depth:
    from: count.depth
phases:
  - id: count
    driver: tool
    check: depth
    outputs:
      more:
        schema:
          type: boolean
      depth:
        schema:
          type: number
    gate:
      routes:
        - when:
            eq:
              ref: count.more
              value: true
          to: again
        - to: done
  - id: again
    shape: call
    call: call-recurse
    args:
      goal: goal
    max_depth: 3
    gate:
      routes:
        - to: done
cleanup: manual
`;

const depthScript = [
  '#!/bin/sh',
  '# One character per invocation, in the run tree\'s shared workspace.',
  'counter=".ao-call-depth"',
  'printf x >> "$counter"',
  'depth=$(wc -c < "$counter" | tr -d " \\n")',
  'if [ "$depth" -lt 3 ]; then more=true; else more=false; fi',
  'printf \'{"status":"done","outputs":{"more":%s,"depth":%s},"question":null,"reason":null}\' "$more" "$depth" > "$AO_ENVELOPE"',
  '',
].join('\n');

test('a bounded self-call recursion completes inside its declared max_depth', async ({
  harness,
}) => {
  const project = await seedWorkflowProject(
    harness,
    'workflow-call-recurse-project',
    [{ name: 'call-recurse', yaml: recursiveYaml, prompts: {} }],
    [],
    'checks:\n  depth: ["sh", "scripts/depth.sh"]\n',
    { 'scripts/depth.sh': depthScript },
  );

  const item = await start(harness, project.projectId, 'call-recurse');
  await waitForWorkflowState(harness, item.id, 'done');

  // Three levels ran: the root recursed twice and the third stopped itself,
  // one traversal short of the edge's bound.
  const root = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(root.phases.map((phase) => phase.phaseId)).toEqual(['count', 'again']);
  expect(root.phases[0]?.outputEnvelope).toMatchObject({ outputs: { depth: 1, more: true } });
  const rootChildren = await callChildren(harness, item.id);
  expect(rootChildren).toHaveLength(1);

  const second = await harness.rpc<WorkflowDetail>('WorkflowGetItem', rootChildren[0]!.itemId);
  expect(second.item.callDepth).toBe(1);
  expect(second.item.state).toBe('done');
  expect(second.phases[0]?.outputEnvelope).toMatchObject({ outputs: { depth: 2, more: true } });
  const secondChildren = await callChildren(harness, second.item.id);
  expect(secondChildren).toHaveLength(1);

  const third = await harness.rpc<WorkflowDetail>('WorkflowGetItem', secondChildren[0]!.itemId);
  expect(third.item.callDepth).toBe(2);
  expect(third.item.state).toBe('done');
  // The deepest level never entered its call phase, so the recursion ended
  // because the workflow decided to, not because a bound refused it.
  expect(third.phases.map((phase) => phase.phaseId)).toEqual(['count']);
  expect(third.phases[0]?.outputEnvelope).toMatchObject({ outputs: { depth: 3, more: false } });
  expect(await callChildren(harness, third.item.id)).toHaveLength(0);

  // Each level's call envelope carries the run it called, and every level saw
  // the same workspace — the counter is one file, incremented three times.
  expect(root.phases[1]?.outputEnvelope).toMatchObject({ status: 'done', outputs: { depth: 2 } });
  expect(second.phases[1]?.outputEnvelope).toMatchObject({ status: 'done', outputs: { depth: 3 } });
  expect(second.item.worktreePath ?? '').toBe(root.item.worktreePath ?? '');
  expect(third.item.worktreePath ?? '').toBe(root.item.worktreePath ?? '');
});

// A fan-out whose units are call edges: each unit runs a whole sub-workflow in
// its own sub-worktree, and the join consolidates them (spec §3, §3a, §9). This
// is the campaign shape — the isolation belongs to the unit, and what runs
// inside it is a child run rather than a turn.
const callFanOutYaml = `id: call-fanout
name: Call fan-out
inputs:
  goal:
    schema:
      type: string
phases:
  - id: plan
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: plan.md
    access: read-only
    inputs:
      goal:
        schema:
          type: string
    outputs:
      sections:
        schema:
          type: array
          items:
            type: string
    gate:
      routes:
        - to: wave
  - id: wave
    shape: fan-out
    over: plan.sections
    as: section
    unit:
      id: wave-unit
      call: call-fanout-child
      args:
        subject: section
    join:
      id: merge
      provider: codex
      model: gpt-5.5
      prompt: merge.md
      access: read-only
    inputs:
      plan.sections:
        schema:
          type: array
          items:
            type: string
    gate:
      routes:
        - to: done
cleanup: manual
`;

// The child writes, which is what gives the whole tree a worktree and every
// unit a sub-worktree to cut from it.
const callFanOutChildYaml = `id: call-fanout-child
name: Call fan-out child
inputs:
  subject:
    schema:
      type: string
phases:
  - id: edit
    driver: agent
    provider: codex
    model: gpt-5.5
    prompt: edit.md
    access: write
    inputs:
      subject:
        schema:
          type: string
    gate:
      routes:
        - to: done
cleanup: manual
`;

test('a call-bound fan-out unit runs its child in the unit sub-worktree', async ({ harness }) => {
  await setClaudeScenario(harness, 'call-fanout-plan', [
    { steps: [{ emit: { lines: [doneResult({ sections: ['alpha', 'beta'] })] } }] },
  ]);
  // Neither the child's phase nor the join declares outputs, so one
  // control-only envelope answers every codex turn in this run.
  await setCodexScenario(harness, 'call-fanout-units', doneEnvelope({}));
  const project = await seedWorkflowProject(
    harness,
    'workflow-call-fanout-project',
    [
      {
        name: 'call-fanout',
        yaml: callFanOutYaml,
        prompts: {
          'plan.md': 'Plan {{goal}} and return the sections.',
          'merge.md': 'Consolidate {{units}} and return the envelope.',
        },
      },
      {
        name: 'call-fanout-child',
        yaml: callFanOutChildYaml,
        prompts: { 'edit.md': 'Port {{subject}} and return the envelope.' },
      },
    ],
    [],
    'base_branch: main\n',
  );

  const item = await start(harness, project.projectId, 'call-fanout');
  await waitForWorkflowState(harness, item.id, 'done');

  const detail = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(detail.units.map((unit) => unit.unitId)).toEqual(['wave-unit-0', 'wave-unit-1', 'merge']);
  expect(detail.units.map((unit) => unit.status)).toEqual(['done', 'done', 'done']);

  const units = detail.units.filter((unit) => unit.kind === 'unit');
  // A call unit holds no session of its own — its work is a child run — but it
  // is isolated exactly like a writing agent unit is.
  for (const unit of units) {
    expect(unit.threadId ?? '').toBe('');
    expect(unit.branch?.startsWith(`${detail.item.branch}-`)).toBe(true);
  }
  expect(units[0]?.branch).not.toBe(units[1]?.branch);

  // One child per unit, each naming the unit that called it.
  const unitChildren = await callChildren(harness, item.id);
  expect(unitChildren).toHaveLength(2);
  const childByUnit = new Map(unitChildren.map((child) => [child.parentUnitId, child]));
  expect([...childByUnit.keys()].sort()).toEqual(['wave-unit-0', 'wave-unit-1']);

  const worktrees = new Set<string>();
  for (const unit of units) {
    const child = childByUnit.get(unit.unitId);
    expect(child?.state).toBe('done');
    expect(child?.parentPhaseId).toBe('wave');
    expect(child?.callDepth).toBe(1);
    const childDetail = await harness.rpc<WorkflowDetail>('WorkflowGetItem', child!.itemId);
    expect(childDetail.item.parentUnitId).toBe(unit.unitId);
    // The child ran in its unit's sub-worktree, not in the caller's: isolation
    // is introduced by fan-out and a child provisions nothing of its own.
    expect(childDetail.item.branch).toBe(unit.branch);
    expect(childDetail.item.worktreePath).toBeTruthy();
    expect(childDetail.item.worktreePath).not.toBe(detail.item.worktreePath);
    expect(childDetail.item.baseBranch).toBe(detail.item.branch);
    worktrees.add(childDetail.item.worktreePath ?? '');
    expect(childDetail.phases.map((phase) => phase.phaseId)).toEqual(['edit']);
    expect(childDetail.phases[0]?.threadId).toBeTruthy();
  }
  expect(worktrees.size).toBe(2);

  // The join is the attempt's own turn, and its envelope is the phase's.
  const merge = detail.units.find((unit) => unit.kind === 'join');
  expect(merge?.threadId).toBeTruthy();
  expect(detail.phases[1]?.threadId).toBe(merge?.threadId);
  expect(detail.phases[1]?.outputEnvelope).toMatchObject({ status: 'done' });
});
