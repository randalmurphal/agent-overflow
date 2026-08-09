import { test as base, expect } from './fixtures.js';
import {
  doneResult,
  retryableFailureWorkflow,
  seedWorkflowProject,
  setClaudeScenario,
  setGlobalPause,
  startWorkflow,
  waitForEnginePause,
  waitForWorkflowState,
  type WorkflowDetail,
} from './workflows-helpers.js';

const test = base.extend<{}, { workflowRerunWorker: true }>({
  workflowRerunWorker: [
    async ({}, use) => await use(true),
    { scope: 'worker', auto: true },
  ],
});

test('failed run reruns with guidance and completes', async ({ harness }) => {
  // Starting under the global pause is how a spec stages a scenario before any
  // provider session exists: the run is admitted and persisted `running` with
  // its first phase held — held is not parked, and there is no queued state.
  await setGlobalPause(harness, true);
  await setClaudeScenario(harness, 'rerun-failed', [
    { steps: [{ emit: { lines: [doneResult({ complete: false })] } }] },
  ]);
  const project = await seedWorkflowProject(harness, 'rerun', [
    { name: 'rerun-flow', yaml: retryableFailureWorkflow('rerun-flow') },
  ]);
  const item = await startWorkflow(
    harness,
    project.projectId,
    'rerun-flow',
    'Retry a genuine failure',
  );
  const held = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(held.item.state).toBe('running');
  expect(held.item.reason ?? '').toBe('');
  expect(held.phases).toHaveLength(1);
  expect(held.phases[0].status).toBe('running');
  expect(held.phases[0].threadId ?? '').toBe('');

  await setGlobalPause(harness, false);
  await waitForEnginePause(harness, false);
  await waitForWorkflowState(harness, item.id, 'failed', 'check-failed-genuine');

  await setClaudeScenario(harness, 'rerun-done', [
    { steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  const restarted = waitForWorkflowState(harness, item.id, 'running');
  await harness.rpc('WorkflowRerunItem', item.id, '', false);
  await restarted;
  await waitForWorkflowState(harness, item.id, 'done');
});
