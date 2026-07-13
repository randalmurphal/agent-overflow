import { test as base, expect, type HarnessMockEvent } from './fixtures.js';
import {
  doneResult,
  enqueueWorkflow,
  humanGateWorkflow,
  pauseWorkflowQueue,
  seedWorkflowProject,
  setClaudeScenario,
  startOneWorkflow,
  terminalWorkflow,
  waitForWorkflowState,
  type WorkflowDetail,
} from './workflows-helpers.js';

const test = base.extend<{}, { workflowSidebarWorker: true }>({
  workflowSidebarWorker: [
    async ({}, use) => await use(true),
    { scope: 'worker', auto: true },
  ],
});

test('quiet workflow footer stays present without an attention badge', async ({
  harness,
  page,
}) => {
  await seedWorkflowProject(harness, 'sidebar-quiet', [
    {
      name: 'quiet-flow',
      yaml: terminalWorkflow('quiet-flow', 'done'),
    },
  ]);

  await page.goto(harness.url);
  await expect(page.getByTestId('workflows-footer')).toBeVisible();
  await expect(page.getByTestId('workflows-footer-attention')).toHaveCount(0);
});

test('parked roll-up orders live runs and excludes cancelled and phase threads', async ({
  harness,
  page,
}) => {
  await pauseWorkflowQueue(harness);
  await setClaudeScenario(harness, 'sidebar-settled', [
    { steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  const project = await seedWorkflowProject(
    harness,
    'sidebar-ordering',
    [
      { name: 'review-flow', yaml: humanGateWorkflow('review-flow') },
      {
        name: 'failed-flow',
        yaml: terminalWorkflow('failed-flow', 'failed'),
      },
      {
        name: 'plain-flow',
        yaml: terminalWorkflow('plain-flow', 'done'),
      },
    ],
    [
      {
        workflow: 'review-flow',
        goal: 'A needs-you run',
        target: 'needs-human',
      },
      { workflow: 'plain-flow', goal: 'E done to dispose', target: 'done' },
    ],
  );
  const [needsYouId, doneId] = project.workItemIds;

  const failed = await enqueueWorkflow(
    harness,
    project.projectId,
    'failed-flow',
    'B failed run',
  );
  await startOneWorkflow(harness);
  await waitForWorkflowState(
    harness,
    failed.id,
    'failed',
    'check-failed-genuine',
  );

  await setClaudeScenario(harness, 'sidebar-running', [
    {
      steps: [
        { waitSignal: { name: 'hold-sidebar-running' } },
        { emit: { lines: [doneResult({ complete: true })] } },
      ],
    },
  ]);
  const running = await enqueueWorkflow(
    harness,
    project.projectId,
    'plain-flow',
    'C running run',
  );
  await startOneWorkflow(harness);
  const runningMock = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) =>
      event.scenario === 'sidebar-running' &&
      event.report.kind === 'registered',
  );
  await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) =>
      event.mockId === runningMock.mockId &&
      event.report.kind === 'waiting_signal' &&
      event.report.detail === 'hold-sidebar-running',
  );
  await pauseWorkflowQueue(harness);

  const queued = await enqueueWorkflow(
    harness,
    project.projectId,
    'plain-flow',
    'D queued run',
  );
  const cancelled = await enqueueWorkflow(
    harness,
    project.projectId,
    'plain-flow',
    'Cancelled run',
  );
  await harness.rpc('WorkflowRemoveQueuedItem', cancelled.id);
  await waitForWorkflowState(harness, cancelled.id, 'cancelled');

  const doneDetail = await harness.rpc<WorkflowDetail>(
    'WorkflowGetItem',
    doneId,
  );
  const phaseThreadId = doneDetail.phases[0]?.threadId;
  if (!phaseThreadId) {
    throw new Error('done workflow phase did not persist a thread id');
  }
  const phaseThread = await harness.rpc<{ title: string }>(
    'GetThread',
    phaseThreadId,
  );
  await page.goto(harness.url);
  await expect(page.getByTestId('workflows-footer-attention')).toHaveText('2');
  await page.getByTestId('sidebar-workflows-button').click();
  await expect(page.getByTestId('wf-overview')).toBeVisible();
  await expect(page.getByTestId('wf-title')).toHaveText('Workflows');
  const section = page.locator(
    `[data-testid="workflows-section"][data-project-id="${project.projectId}"]`,
  );
  await expect(section.getByTestId('workflows-section-attention')).toHaveText(
    '2',
  );
  await section.getByTestId('workflows-section-header').click();

  const rows = section.getByTestId('workflow-sidebar-run');
  await expect(rows).toHaveCount(5);
  await expect
    .poll(async () =>
      rows.evaluateAll((elements) =>
        elements.map((element) => element.getAttribute('data-run-id')),
      ),
    )
    .toEqual([needsYouId, failed.id, running.id, queued.id, doneId]);
  await expect(section.locator(`[data-run-id="${cancelled.id}"]`)).toHaveCount(
    0,
  );

  const doneRow = section.locator(`[data-run-id="${doneId}"]`);
  await expect(doneRow.getByText('done', { exact: true })).toBeVisible();
  await expect(doneRow.getByText('Needs you', { exact: true })).toHaveCount(0);
  await expect(doneRow.getByTestId('workflow-sidebar-goal')).toHaveClass(
    /text-fg-hint/,
  );

  await section.locator(`[data-run-id="${queued.id}"]`).click();
  await expect(page.getByTestId('wf-run-detail')).toBeVisible();
  await expect(page.getByTestId('wf-title')).toHaveText('D queued run');

  await doneRow.click();
  await expect(page.getByTestId('wf-title')).toHaveText('E done to dispose');
  await page.getByTestId('wf-phase-row').click();
  const threadPane = page.locator('[data-pane-kind="thread"]');
  await expect(
    threadPane.getByText(phaseThread.title, { exact: true }),
  ).toBeVisible();
  await expect(
    page.locator(`[data-sidebar-thread-id="${phaseThreadId}"]`),
  ).toHaveCount(0);

  await page.getByTestId('sidebar-thread-search').fill(phaseThread.title);
  await expect(
    page.locator(`[data-sidebar-thread-id="${phaseThreadId}"]`),
  ).toHaveCount(0);
});
