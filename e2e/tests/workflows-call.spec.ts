import { test, expect } from './fixtures.js';
import {
  doneResult,
  seedWorkflowProject,
  setClaudeScenario,
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

  expect(detail.children).toHaveLength(1);
  const child = detail.children[0];
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
  expect(childDetail.children).toHaveLength(0);
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
  expect(root.children).toHaveLength(1);

  const second = await harness.rpc<WorkflowDetail>('WorkflowGetItem', root.children[0]!.itemId);
  expect(second.item.callDepth).toBe(1);
  expect(second.item.state).toBe('done');
  expect(second.phases[0]?.outputEnvelope).toMatchObject({ outputs: { depth: 2, more: true } });
  expect(second.children).toHaveLength(1);

  const third = await harness.rpc<WorkflowDetail>('WorkflowGetItem', second.children[0]!.itemId);
  expect(third.item.callDepth).toBe(2);
  expect(third.item.state).toBe('done');
  // The deepest level never entered its call phase, so the recursion ended
  // because the workflow decided to, not because a bound refused it.
  expect(third.phases.map((phase) => phase.phaseId)).toEqual(['count']);
  expect(third.phases[0]?.outputEnvelope).toMatchObject({ outputs: { depth: 3, more: false } });
  expect(third.children).toHaveLength(0);

  // Each level's call envelope carries the run it called, and every level saw
  // the same workspace — the counter is one file, incremented three times.
  expect(root.phases[1]?.outputEnvelope).toMatchObject({ status: 'done', outputs: { depth: 2 } });
  expect(second.phases[1]?.outputEnvelope).toMatchObject({ status: 'done', outputs: { depth: 3 } });
  expect(second.item.worktreePath ?? '').toBe(root.item.worktreePath ?? '');
  expect(third.item.worktreePath ?? '').toBe(root.item.worktreePath ?? '');
});
