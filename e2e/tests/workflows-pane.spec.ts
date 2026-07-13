import { test as base, expect } from './fixtures.js';
import {
  pauseWorkflowQueue,
  seedWorkflowProject,
  terminalWorkflow,
  type WorkflowStateEvent,
} from './workflows-helpers.js';

const test = base.extend<{}, { workflowPaneWorker: true }>({
  workflowPaneWorker: [
    async ({}, use) => await use(true),
    { scope: 'worker', auto: true },
  ],
});

test('pane navigation, persistence, queue toggle, and queued cancel use the real SPA', async ({
  harness,
  page,
}) => {
  await pauseWorkflowQueue(harness);
  const project = await seedWorkflowProject(
    harness,
    'pane-navigation',
    [
      {
        name: 'pane-flow',
        yaml: terminalWorkflow('pane-flow', 'done'),
      },
    ],
    [{ workflow: 'pane-flow', goal: 'Queued pane run', target: 'queued' }],
  );
  const itemId = project.workItemIds[0];

  await page.goto(harness.url);
  await page.getByTestId('sidebar-workflows-button').click();
  await expect(page.getByTestId('wf-overview')).toBeVisible();
  await expect(page.getByTestId('wf-workflow-row')).toContainText('pane-flow');
  await expect(page.getByTestId('wf-up-next')).toBeVisible();
  await expect(page.getByTestId('wf-queue-open')).toHaveText('Queued pane run');
  await expect(page.getByTestId('wf-queue-toggle')).toHaveText('▶ Paused');

  await page.getByTestId('wf-workflow-row').click();
  await expect(page.getByTestId('wf-workflow-detail')).toBeVisible();
  await expect(page.getByTestId('wf-title')).toHaveText('pane-flow');
  await page.getByTestId('wf-run-open').click();
  await expect(page.getByTestId('wf-run-detail')).toBeVisible();
  await expect(page.getByTestId('wf-title')).toHaveText('Queued pane run');
  await expect(page.getByTestId('wf-crumb-0')).toHaveText('Workflows');
  await expect(page.getByTestId('wf-crumb-1')).toHaveText('pane-flow');

  await page.reload();
  await expect(page.getByTestId('wf-run-detail')).toBeVisible();
  await expect(page.getByTestId('wf-title')).toHaveText('Queued pane run');

  await page.getByTestId('wf-crumb-1').click();
  await expect(page.getByTestId('wf-workflow-detail')).toBeVisible();
  await page.getByTestId('wf-crumb-0').click();
  await expect(page.getByTestId('wf-overview')).toBeVisible();

  const queueRow = page
    .getByTestId('wf-queue-row')
    .filter({ hasText: 'Queued pane run' });
  await queueRow.hover();
  await queueRow.getByTestId('wf-queue-cancel').click();
  await expect(queueRow.getByTestId('wf-queue-cancel')).toHaveText('cancel?');
  const removed = harness.waitForEvent<WorkflowStateEvent>(
    'workflow:item-state',
    (event) => event.itemId === itemId && event.to === 'cancelled',
  );
  await queueRow.getByTestId('wf-queue-cancel').click();
  await removed;
  await expect(queueRow).toHaveCount(0);

  await page.getByTestId('wf-queue-toggle').click();
  await expect(page.getByTestId('wf-queue-toggle')).toHaveText('❚❚ Active');
  await expect
    .poll(async () => {
      const settings = await harness.rpc<{ workflowQueueActive: boolean }>(
        'GetSettings',
      );
      return settings.workflowQueueActive;
    })
    .toBe(true);
});
