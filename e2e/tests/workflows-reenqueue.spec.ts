import { test as base } from './fixtures.js';
import {
  doneResult,
  enqueueWorkflow,
  pauseWorkflowQueue,
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
  const workflow = `id: reenqueue-flow
name: reenqueue-flow
inputs:
  goal:
    schema:
      type: string
phases:
  - id: run
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: reenqueue-flow.md
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
        - when:
            eq:
              ref: run.complete
              value: false
          to: failed
        - to: done
`;
  const project = await seedWorkflowProject(harness, 'reenqueue', [
    { name: 'reenqueue-flow', yaml: workflow },
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
