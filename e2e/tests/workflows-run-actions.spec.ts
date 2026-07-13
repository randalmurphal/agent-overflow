import type { Page } from '@playwright/test';
import { rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { test as base, expect, type HarnessMockEvent } from './fixtures.js';
import {
  doneResult,
  humanGateWorkflow,
  pauseWorkflowQueue,
  questionResult,
  seedWorkflowProject,
  setClaudeScenario,
  terminalWorkflow,
  type WorkflowDetail,
  type WorkflowStateEvent,
} from './workflows-helpers.js';

const test = base.extend<{}, { workflowRunActionsWorker: true }>({
  workflowRunActionsWorker: [
    async ({}, use) => await use(true),
    { scope: 'worker', auto: true },
  ],
});

async function openRun(
  page: Page,
  workflowName: string,
  goal: string,
): Promise<void> {
  await page.getByTestId('sidebar-workflows-button').click();
  await expect(page.getByTestId('wf-overview')).toBeVisible();
  await page
    .getByTestId('wf-workflow-row')
    .filter({ hasText: workflowName })
    .click();
  await page.getByTestId('wf-run-open').filter({ hasText: goal }).click();
  await expect(page.getByTestId('wf-run-detail')).toBeVisible();
  await expect(page.getByTestId('wf-title')).toHaveText(goal);
}

test('human gate approval completes in the DOM and discard removes the run', async ({
  harness,
  page,
}) => {
  await pauseWorkflowQueue(harness);
  await setClaudeScenario(harness, 'actions-gate', [
    { steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  const project = await seedWorkflowProject(
    harness,
    'actions-gate',
    [{ name: 'gate-flow', yaml: humanGateWorkflow('gate-flow', 'write') }],
    [
      {
        workflow: 'gate-flow',
        goal: 'Approve this gate',
        target: 'needs-human',
      },
    ],
  );
  const itemId = project.workItemIds[0];

  const seededItems = await harness.rpc<Array<{ id: string }>>(
    'WorkflowListItems',
    '',
  );
  expect(seededItems.map((item) => item.id)).toContain(itemId);
  const catalog = await harness.rpc<{ workflows: Array<{ id: string }> }>(
    'WorkflowListDefinitions',
    project.projectId,
  );
  expect(catalog.workflows.map((workflow) => workflow.id)).toContain(
    'gate-flow',
  );

  await page.goto(harness.url);
  const section = page.locator(
    `[data-testid="workflows-section"][data-project-id="${project.projectId}"]`,
  );
  await section.getByTestId('workflows-section-header').click();
  const sidebarRow = section.locator(`[data-run-id="${itemId}"]`);
  await expect(sidebarRow).toBeVisible();
  await sidebarRow.click();
  await expect(page.getByTestId('wf-run-detail')).toBeVisible();
  await expect(page.getByTestId('wf-run-state')).toHaveText('Review gate');
  await expect(page.getByTestId('wf-approve')).toBeVisible();

  const completed = harness.waitForEvent<WorkflowStateEvent>(
    'workflow:item-state',
    (event) => event.itemId === itemId && event.to === 'done',
  );
  await page.getByTestId('wf-approve').click();
  await completed;
  await expect(page.getByTestId('wf-resolved-receipt')).toContainText(
    'Approved',
  );
  await expect(page.getByTestId('wf-run-state')).toHaveText('Done');

  await page.getByTestId('wf-pane').press('a');
  await expect(page.getByRole('alert').filter({ hasText: 'Workflow action failed' })).toHaveCount(0);
  const guarded = await harness.rpc<{ item: { disposition?: unknown } }>('WorkflowGetItem', itemId);
  expect(guarded.item.disposition || '').toBe('');

  await sidebarRow.click();
  await expect(page.getByTestId('wf-merge')).toBeVisible();
  await expect(page.getByTestId('wf-create-pr')).toBeVisible();
  await page.getByTestId('wf-done-discard').click();
  await expect(page.getByTestId('wf-done-discard')).toContainText(
    'Discard this run?',
  );
  await page.getByTestId('wf-done-discard').click();
  await expect(page.getByTestId('wf-resolved-receipt')).toContainText(
    'Discarded',
  );
  await expect(sidebarRow).toHaveCount(0);
});

test('question answer through the UI resumes the same provider session', async ({
  harness,
  page,
}) => {
  await pauseWorkflowQueue(harness);
  await setClaudeScenario(harness, 'actions-question', [
    {
      label: 'question',
      steps: [
        { emit: { lines: [questionResult('Which option should continue?')] } },
      ],
    },
    {
      label: 'answer',
      steps: [{ emit: { lines: [doneResult({ complete: true })] } }],
    },
  ]);
  const project = await seedWorkflowProject(
    harness,
    'actions-question',
    [
      {
        name: 'question-flow',
        yaml: terminalWorkflow('question-flow', 'done'),
      },
    ],
    [
      {
        workflow: 'question-flow',
        goal: 'Answer this question',
        target: 'needs-human',
      },
    ],
  );
  const itemId = project.workItemIds[0];
  const registered = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) =>
      event.scenario === 'actions-question' &&
      event.report.kind === 'registered',
  );

  await page.goto(harness.url);
  await openRun(page, 'question-flow', 'Answer this question');
  await expect(page.getByTestId('wf-run-state')).toHaveText('Question');
  await expect(page.getByTestId('wf-question')).toBeVisible();
  await page.getByTestId('wf-answer-input').fill('Use option A');

  const secondTurn = harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) =>
      event.mockId === registered.mockId &&
      event.report.kind === 'turn_started' &&
      event.report.turn === 2,
  );
  const completed = harness.waitForEvent<WorkflowStateEvent>(
    'workflow:item-state',
    (event) => event.itemId === itemId && event.to === 'done',
  );
  await page.getByTestId('wf-answer-send').click();
  expect((await secondTurn).mockId).toBe(registered.mockId);
  await completed;
  await expect(page.getByTestId('wf-resolved-receipt')).toContainText(
    'Use option A',
  );
  await expect(page.getByTestId('wf-run-state')).toHaveText('Done');
});

test('watchdog stall renders parked treatment and recovery actions', async ({
  harness,
  page,
}) => {
  await pauseWorkflowQueue(harness);
  await setClaudeScenario(harness, 'actions-stalled', [
    { steps: [{ stall: {} }] },
  ]);
  await seedWorkflowProject(
    harness,
    'actions-stalled',
    [
      {
        name: 'stalled-flow',
        yaml: terminalWorkflow('stalled-flow', 'done'),
      },
    ],
    [
      {
        workflow: 'stalled-flow',
        goal: 'Recover stalled work',
        target: 'needs-human',
      },
    ],
    'reliability:\n  watchdog: 100ms\n  backoff: [1ms]\n',
  );

  await page.goto(harness.url);
  await openRun(page, 'stalled-flow', 'Recover stalled work');
  await expect(page.getByTestId('wf-run-state')).toHaveText('Needs you');
  await expect(page.getByTestId('wf-digest')).toContainText(
    'stopped producing activity',
  );
  await expect(page.getByTestId('wf-parked-continue')).toBeVisible();
  await expect(page.getByTestId('wf-parked-resume')).toBeVisible();
  await expect(page.getByTestId('wf-parked-discard')).toBeVisible();
});

test('done run continues in its linked triage thread, which stays hidden from the sidebar', async ({
  harness,
  page,
}) => {
  await pauseWorkflowQueue(harness);
  await setClaudeScenario(harness, 'actions-done-triage-seed', [
    { steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  const project = await seedWorkflowProject(
    harness,
    'actions-done-triage',
    [{ name: 'done-triage-flow', yaml: terminalWorkflow('done-triage-flow', 'done', 'write') }],
    [{ workflow: 'done-triage-flow', goal: 'Continue completed run', target: 'done' }],
  );
  const itemId = project.workItemIds[0];
  await setClaudeScenario(harness, 'actions-done-triage-open', [
    { steps: [{ emit: { lines: [JSON.stringify({ type: 'result', subtype: 'success', is_error: false })] } }] },
  ]);

  await page.goto(harness.url);
  await openRun(page, 'done-triage-flow', 'Continue completed run');
  await page.getByTestId('wf-done-continue').click();
  await expect(page.locator('[data-pane-kind="thread"]')).toBeVisible();
  const detail = await harness.rpc<{ item: { triageThreadId?: string } }>('WorkflowGetItem', itemId);
  expect(detail.item.triageThreadId).toBeTruthy();
  await expect(page.locator(`[data-ui-surface="chat"][data-thread-id="${detail.item.triageThreadId}"]`)).toBeVisible();
  await expect(page.locator(`[data-sidebar-thread-id="${detail.item.triageThreadId}"]`)).toHaveCount(0);
});

test('disposition-parked merge retries through the UI and returns the run to done', async ({
  harness,
  page,
}) => {
  await pauseWorkflowQueue(harness);
  await setClaudeScenario(harness, 'actions-merge-retry', [
    { steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  const project = await seedWorkflowProject(
    harness,
    'actions-merge-retry',
    [{ name: 'merge-retry-flow', yaml: terminalWorkflow('merge-retry-flow', 'done', 'write') }],
    [{ workflow: 'merge-retry-flow', goal: 'Retry a refused merge', target: 'done' }],
  );
  const itemId = project.workItemIds[0];
  const before = await harness.rpc<{ item: { worktreePath?: string } }>('WorkflowGetItem', itemId);
  expect(before.item.worktreePath).toBeTruthy();
  const dirtyPath = join(before.item.worktreePath!, 'p39-dirty.txt');
  await writeFile(dirtyPath, 'force the first merge to refuse\n');

  await page.goto(harness.url);
  await openRun(page, 'merge-retry-flow', 'Retry a refused merge');
  const parked = harness.waitForEvent<WorkflowStateEvent>(
    'workflow:item-state',
    (event) => event.itemId === itemId && event.to === 'needs-human' && event.reason === 'disposition',
  );
  await page.getByTestId('wf-merge').click();
  await parked;
  await expect(page.getByTestId('wf-run-state')).toHaveText('Disposition');
  await expect(page.getByTestId('wf-merge')).toBeVisible();
  await rm(dirtyPath);

  const done = harness.waitForEvent<WorkflowStateEvent>(
    'workflow:item-state',
    (event) => event.itemId === itemId && event.to === 'done',
  );
  await page.getByTestId('wf-merge').click();
  await done;
  await expect(page.getByTestId('wf-run-state')).toHaveText('Done');
  await expect(page.getByTestId('wf-resolved-receipt')).toContainText('Merged to');
  await expect(page.getByTestId('wf-disposition-receipt')).toBeVisible();
});

test('needs-attention sweep advances with j/k and finishes at all-clear', async ({
  harness,
  page,
}) => {
  await pauseWorkflowQueue(harness);
  await setClaudeScenario(harness, 'actions-sweep', [
    { steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  await seedWorkflowProject(
    harness,
    'actions-sweep',
    [
      {
        name: 'sweep-flow',
        yaml: terminalWorkflow('sweep-flow', 'done', 'write'),
      },
    ],
    [
      { workflow: 'sweep-flow', goal: 'Sweep first run', target: 'done' },
      { workflow: 'sweep-flow', goal: 'Sweep second run', target: 'done' },
    ],
  );
  await harness.waitForEvent('provider:turn_completed');
  await harness.waitForEvent('provider:turn_completed');

  await page.goto(harness.url);
  await page.getByTestId('sidebar-workflows-button').click();
  await page.getByTestId('wf-dispose-count').click();
  await expect(page.getByTestId('wf-sweep-counter')).toHaveText('1 of 2');
  await expect(page.getByTestId('wf-title')).toHaveText('Sweep first run');

  await page.getByTestId('wf-pane').press('j');
  await expect(page.getByTestId('wf-title')).toHaveText('Sweep second run');
  await page.getByTestId('wf-pane').press('k');
  await expect(page.getByTestId('wf-title')).toHaveText('Sweep first run');

  await page.getByTestId('wf-done-discard').click();
  await page.getByTestId('wf-done-discard').click();
  await expect(page.getByTestId('wf-title')).toHaveText('Sweep second run');
  await page.getByTestId('wf-done-discard').click();
  await page.getByTestId('wf-done-discard').click();
  await expect(page.getByTestId('wf-all-clear')).toBeVisible();
  await expect(
    page.getByText('Nothing needs you', { exact: true }),
  ).toBeVisible();
});

test('full review opens beside workflows and closes with its source pane', async ({
  harness,
  page,
}) => {
  await pauseWorkflowQueue(harness);
  await setClaudeScenario(harness, 'actions-review', [
    { steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  const project = await seedWorkflowProject(
    harness,
    'actions-review',
    [
      {
        name: 'review-flow',
        yaml: terminalWorkflow('review-flow', 'done', 'write'),
      },
    ],
    [
      {
        workflow: 'review-flow',
        goal: 'Review completed work',
        target: 'done',
      },
    ],
  );

  await page.goto(harness.url);
  await openRun(page, 'review-flow', 'Review completed work');
  await page.getByTestId('wf-open-full-review').click();
  const companion = page.locator(
    '[data-testid="companion-pane-review"][data-companion-source-pane-id="workflows"]',
  );
  await expect(companion).toBeVisible();
  await expect(companion.getByTestId('review-pane')).toBeVisible();
  await expect(companion.getByTestId('review-empty')).toBeVisible();
  const detail = await harness.rpc<WorkflowDetail>('WorkflowGetItem', project.workItemIds[0]);
  const expectedPhaseThread = [...detail.phases].reverse().find((phase) => phase.threadId)?.threadId;
  expect(expectedPhaseThread).toBeTruthy();
  await expect(companion).toHaveAttribute('data-review-thread-id', expectedPhaseThread!);

  await page.getByTestId('wf-close').click();
  await expect(page.getByTestId('wf-pane')).toHaveCount(0);
  await expect(companion).toHaveCount(0);
});
