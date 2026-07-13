import { test as base, expect } from './fixtures.js';
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

const test = base.extend<{}, { workflowReenqueueUIWorker: true }>({
  workflowReenqueueUIWorker: [
    async ({}, use) => await use(true),
    { scope: 'worker', auto: true },
  ],
});

test('failed run re-enqueues from the UI and drains to done', async ({ harness, page }) => {
  await pauseWorkflowQueue(harness);
  await setClaudeScenario(harness, 'reenqueue-ui-failed', [
    { steps: [{ emit: { lines: [doneResult({ complete: false })] } }] },
  ]);
  const project = await seedWorkflowProject(harness, 'reenqueue-ui', [
    { name: 'reenqueue-flow', yaml: retryableFailureWorkflow('reenqueue-flow') },
  ]);
  const item = await enqueueWorkflow(
    harness,
    project.projectId,
    'reenqueue-flow',
    'Retry through the UI',
  );
  await startOneWorkflow(harness);
  await waitForWorkflowState(harness, item.id, 'failed', 'check-failed-genuine');
  await setClaudeScenario(harness, 'reenqueue-ui-done', [
    { steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);

  await page.goto(harness.url);
  await page.getByTestId('sidebar-workflows-button').click();
  await page.getByTestId('wf-workflow-row').filter({ hasText: 'reenqueue-flow' }).click();
  await page.getByTestId('wf-run-open').filter({ hasText: 'Retry through the UI' }).click();
  await expect(page.getByTestId('wf-run-state')).toHaveText('Failed');

  const queued = waitForWorkflowState(harness, item.id, 'queued');
  await page.getByTestId('wf-resume').click();
  await queued;
  await expect(page.getByTestId('wf-resolved-receipt')).toContainText(
    'Re-enqueued with the diagnosis as guidance — position 1',
  );
  await startOneWorkflow(harness);
  await waitForWorkflowState(harness, item.id, 'done');
  await expect(page.getByTestId('wf-run-state')).toHaveText('Done');
});
