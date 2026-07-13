import { test as base, expect } from './fixtures.js';
import {
  pauseWorkflowQueue,
  seedWorkflowProject,
  terminalWorkflow,
} from './workflows-helpers.js';

const test = base.extend<{}, { workflowIntakeWorker: true }>({
  workflowIntakeWorker: [
    async ({}, use) => await use(true),
    { scope: 'worker', auto: true },
  ],
});

test('intake validates the goal and queues a run into Up next and the sidebar', async ({
  harness,
  page,
}) => {
  await pauseWorkflowQueue(harness);
  const project = await seedWorkflowProject(harness, 'intake-project', [
    {
      name: 'intake-flow',
      yaml: terminalWorkflow('intake-flow', 'done'),
    },
  ]);

  await page.goto(harness.url);
  await page.getByTestId('sidebar-workflows-button').click();
  await expect(page.getByTestId('wf-overview')).toBeVisible();
  await page.getByTestId('wf-new-run').click();
  await expect(page.getByTestId('wf-intake-dialog')).toBeVisible();
  await page
    .getByTestId('wf-intake-workflow')
    .filter({ hasText: 'intake-flow' })
    .click();

  await expect(page.getByTestId('wf-intake-error')).toHaveText('Enter a goal');
  await expect(page.getByTestId('wf-intake-submit')).toBeDisabled();

  await page.getByTestId('wf-intake-goal').fill('Queue from intake');
  await page.getByTestId('wf-seed-goal').fill('Queue from intake');
  await expect(page.getByTestId('wf-intake-error')).toHaveCount(0);

  await page.getByTestId('wf-intake-submit').click();
  await expect(page.getByTestId('wf-intake-dialog')).toHaveCount(0);
  await expect(page.getByTestId('wf-up-next')).toBeVisible();
  await expect(page.getByTestId('wf-queue-open')).toHaveText(
    'Queue from intake',
  );

  const section = page.locator(
    `[data-testid="workflows-section"][data-project-id="${project.projectId}"]`,
  );
  await section.getByTestId('workflows-section-header').click();
  await expect(section.getByTestId('workflow-sidebar-run')).toContainText(
    'Queue from intake',
  );
});

test('intake persists an edited base-branch override on the queued item', async ({
  harness,
  page,
}) => {
  await pauseWorkflowQueue(harness);
  const project = await seedWorkflowProject(harness, 'intake-base-project', [
    { name: 'intake-base-flow', yaml: terminalWorkflow('intake-base-flow', 'done') },
  ]);

  await page.goto(harness.url);
  await page.getByTestId('sidebar-workflows-button').click();
  await page.getByTestId('wf-new-run').click();
  await page.getByTestId('wf-intake-workflow').filter({ hasText: 'intake-base-flow' }).click();
  await page.getByTestId('wf-intake-goal').fill('Queue with an override');
  await page.getByTestId('wf-seed-goal').fill('Queue with an override');
  await page.getByTestId('wf-intake-base-branch').fill('release/p39');
  await page.getByTestId('wf-intake-submit').click();
  await expect(page.getByTestId('wf-intake-dialog')).toHaveCount(0);

  const items = await harness.rpc<Array<{ id: string; goal: string }>>('WorkflowListItems', project.projectId);
  const item = items.find((entry) => entry.goal === 'Queue with an override');
  expect(item).toBeTruthy();
  const detail = await harness.rpc<{ item: { baseBranch?: string } }>('WorkflowGetItem', item!.id);
  expect(detail.item.baseBranch).toBe('release/p39');
});
