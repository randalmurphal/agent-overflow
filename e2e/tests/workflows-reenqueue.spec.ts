import { test as base } from './fixtures.js';
import {
  doneResult,
  enqueueWorkflow,
  pauseWorkflowQueue,
  retryableFailureWorkflow,
  seedWorkflowProject,
  setClaudeScenario,
  startOneWorkflow,
  waitForWorkflowState,
} from './workflows-helpers.js';

const test = base.extend<{}, { workflowReenqueueWorker: true }>({
  workflowReenqueueWorker: [
    async ({}, use) => await use(true),
    { scope: 'worker', auto: true },
  ],
});

test('failed run re-enqueues with guidance and completes', async ({ harness }) => {
  await pauseWorkflowQueue(harness);
  await setClaudeScenario(harness, 'reenqueue-failed', [
    { steps: [{ emit: { lines: [doneResult({ complete: false })] } }] },
  ]);
  const project = await seedWorkflowProject(harness, 'reenqueue', [
    { name: 'reenqueue-flow', yaml: retryableFailureWorkflow('reenqueue-flow') },
  ]);
  const item = await enqueueWorkflow(
    harness,
    project.projectId,
    'reenqueue-flow',
    'Retry a genuine failure',
  );
  await startOneWorkflow(harness);
  await waitForWorkflowState(
    harness,
    item.id,
    'failed',
    'check-failed-genuine',
  );

  await setClaudeScenario(harness, 'reenqueue-done', [
    { steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  const queued = waitForWorkflowState(harness, item.id, 'queued');
  await harness.rpc('WorkflowReenqueueFailedItem', item.id);
  await queued;
  await startOneWorkflow(harness);
  await waitForWorkflowState(harness, item.id, 'done');
});
