import { test, expect, type HarnessMockEvent } from './fixtures.js';
import {
  doneResult,
  questionResult,
  seedWorkflow,
  setClaudeScenario,
  singlePhaseWorkflow,
  start,
  type WorkflowDetail,
  type WorkflowStateEvent,
} from './workflows-helpers.js';

test('workflow run completes two phases from a direct start', async ({ harness }) => {
  await setClaudeScenario(harness, 'workflow-chain', [
    { steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  const yaml = `id: chain-flow
name: Chain flow
inputs:
  goal:
    schema:
      type: string
phases:
  - id: first
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: chain-flow.md
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
        - to: second
  - id: second
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: chain-flow.md
    access: read-only
    inputs:
      first.complete:
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
  const project = await seedWorkflow(harness, 'workflow-chain-project', 'chain-flow', yaml);
  const item = await start(harness, project.projectId, 'chain-flow');

  await harness.waitForEvent<WorkflowStateEvent>(
    'workflow:item-state',
    (event) => event.itemId === item.id && event.to === 'done',
  );
  const detail = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(detail.item.state).toBe('done');
  expect(detail.phases.map((phase) => phase.phaseId)).toEqual(['first', 'second']);
  for (const phase of detail.phases) {
    expect(phase.status).toBe('completed');
    expect(phase.outputEnvelope).toBeTruthy();
    expect(phase).not.toHaveProperty('gateTrace');
  }
});

test('human workflow gate parks and approves deterministically', async ({ harness }) => {
  await setClaudeScenario(harness, 'workflow-human-gate', [
    {
      steps: [
        { waitSignal: { name: 'release-gate-envelope' } },
        { emit: { lines: [doneResult({ complete: true })] } },
      ],
    },
  ]);
  const yaml = `id: human-flow
name: Human flow
inputs:
  goal:
    schema:
      type: string
phases:
  - id: prepare
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: human-flow.md
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
        - to: review
  - id: review
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: human-flow.md
    access: read-only
    inputs:
      prepare.complete:
        schema:
          type: boolean
    outputs:
      complete:
        schema:
          type: boolean
    gate:
      routes:
        - human:
            approve: done
            reject:
              loop: prepare
              max: 1
cleanup: manual
`;
  const project = await seedWorkflow(
    harness,
    'workflow-human-project',
    'human-flow',
    yaml,
  );
  const item = await start(harness, project.projectId, 'human-flow');
  const prepareMock = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) => event.scenario === 'workflow-human-gate' && event.report.kind === 'registered',
  );
  await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) =>
      event.mockId === prepareMock.mockId &&
      event.report.kind === 'waiting_signal' &&
      event.report.detail === 'release-gate-envelope',
  );
  await harness.rpc('HarnessMockCommand', prepareMock.mockId, {
    type: 'advance',
    name: 'release-gate-envelope',
  });
  const reviewMock = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) =>
      event.scenario === 'workflow-human-gate' &&
      event.report.kind === 'registered' &&
      event.mockId !== prepareMock.mockId,
  );
  await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) =>
      event.mockId === reviewMock.mockId &&
      event.report.kind === 'waiting_signal' &&
      event.report.detail === 'release-gate-envelope',
  );
  await harness.rpc('HarnessMockCommand', reviewMock.mockId, {
    type: 'advance',
    name: 'release-gate-envelope',
  });
  await harness.waitForEvent<WorkflowStateEvent>(
    'workflow:item-state',
    (event) => event.itemId === item.id && event.to === 'needs-human' && event.reason === 'gate',
  );

  await harness.rpc('WorkflowResolveGate', item.id, 'approve', 'approved by e2e');
  await harness.waitForEvent<WorkflowStateEvent>(
    'workflow:item-state',
    (event) => event.itemId === item.id && event.to === 'done',
  );
  const detail = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(detail.phases).toHaveLength(2);
  expect(detail.phases[1].status).toBe('completed');
  expect(detail.phases[1]).not.toHaveProperty('gateTrace');
});

test('workflow question answer continues on the same mock session', async ({ harness }) => {
  await setClaudeScenario(harness, 'workflow-question', [
    { label: 'question', steps: [{ emit: { lines: [questionResult('Which option?')] } }] },
    { label: 'answer', steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  const project = await seedWorkflow(
    harness,
    'workflow-question-project',
    'question-flow',
    singlePhaseWorkflow('question-flow', '        - to: done'),
  );
  const item = await start(harness, project.projectId, 'question-flow');
  const registered = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) => event.scenario === 'workflow-question' && event.report.kind === 'registered',
  );
  await harness.waitForEvent<WorkflowStateEvent>(
    'workflow:item-state',
    (event) =>
      event.itemId === item.id && event.to === 'needs-human' && event.reason === 'question',
  );
  const before = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);

  await harness.rpc('WorkflowAnswerQuestion', item.id, 'Use option A');
  const secondTurn = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) =>
      event.mockId === registered.mockId &&
      event.report.kind === 'turn_started' &&
      event.report.turn === 2,
  );
  expect(secondTurn.mockId).toBe(registered.mockId);
  await harness.waitForEvent<WorkflowStateEvent>(
    'workflow:item-state',
    (event) => event.itemId === item.id && event.to === 'done',
  );
  const after = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(after.phases).toHaveLength(2);
  expect(after.phases[1].threadId).toBe(before.phases[0].threadId);
});

test('workflow watchdog parks a stalled mock without a test sleep', async ({ harness }) => {
  await setClaudeScenario(harness, 'workflow-watchdog', [
    { steps: [{ stall: {} }] },
  ]);
  const project = await seedWorkflow(
    harness,
    'workflow-watchdog-project',
    'watchdog-flow',
    singlePhaseWorkflow('watchdog-flow', '        - to: done'),
    'reliability:\n  watchdog: 100ms\n  backoff: [1ms]\n',
  );
  const item = await start(harness, project.projectId, 'watchdog-flow');

  const registered = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) => event.scenario === 'workflow-watchdog' && event.report.kind === 'registered',
  );

  await harness.waitForEvent<WorkflowStateEvent>(
    'workflow:item-state',
    (event) =>
      event.itemId === item.id && event.to === 'needs-human' && event.reason === 'stalled',
  );
  await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) => event.mockId === registered.mockId && event.report.kind === 'turn_interrupted',
  );
  const detail = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(detail.item.reason).toBe('stalled');
});

test('workflow cancel interrupts a waitSignal-held mock turn', async ({ harness }) => {
  await setClaudeScenario(harness, 'workflow-cancel', [
    {
      steps: [
        { waitSignal: { name: 'hold-running' } },
        { emit: { lines: [doneResult({ complete: true })] } },
      ],
    },
  ]);
  const project = await seedWorkflow(
    harness,
    'workflow-cancel-project',
    'cancel-flow',
    singlePhaseWorkflow('cancel-flow', '        - to: done'),
  );
  const item = await start(harness, project.projectId, 'cancel-flow');
  const registered = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) => event.scenario === 'workflow-cancel' && event.report.kind === 'registered',
  );
  await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) =>
      event.mockId === registered.mockId &&
      event.report.kind === 'waiting_signal' &&
      event.report.detail === 'hold-running',
  );

  await harness.rpc('WorkflowCancelItem', item.id);
  await harness.waitForEvent<WorkflowStateEvent>(
    'workflow:item-state',
    (event) => event.itemId === item.id && event.to === 'cancelled',
  );
  await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) => event.mockId === registered.mockId && event.report.kind === 'turn_interrupted',
  );
  const detail = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(detail.item.state).toBe('cancelled');
});
