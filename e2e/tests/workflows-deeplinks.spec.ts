import { test as base, expect } from './fixtures.js';
import {
  doneResult,
  humanGateWorkflow,
  pauseWorkflowQueue,
  seedWorkflowProject,
  setClaudeScenario,
  terminalWorkflow,
} from './workflows-helpers.js';

interface NotificationActivation {
  kind: string;
  workItemId?: string;
  projectId?: string;
}

const workflowItemTest = base.extend<{}, { workflowItemDeepLinkWorker: true }>({
  workflowItemDeepLinkWorker: [
    async ({}, use) => await use(true),
    { scope: 'worker', auto: true },
  ],
});

const deadItemTest = base.extend<{}, { deadItemDeepLinkWorker: true }>({
  deadItemDeepLinkWorker: [
    async ({}, use) => await use(true),
    { scope: 'worker', auto: true },
  ],
});

const triageTest = base.extend<{}, { triageDeepLinkWorker: true }>({
  triageDeepLinkWorker: [
    async ({}, use) => await use(true),
    { scope: 'worker', auto: true },
  ],
});

workflowItemTest(
  'cold workflow-item notification opens the run inside the attention sweep',
  async ({ harness, page }) => {
    await pauseWorkflowQueue(harness);
    await setClaudeScenario(harness, 'deeplink-item', [
      { steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
    ]);
    const project = await seedWorkflowProject(
      harness,
      'deeplink-item',
      [{ name: 'notify-flow', yaml: humanGateWorkflow('notify-flow') }],
      [
        {
          workflow: 'notify-flow',
          goal: 'Open notified run',
          target: 'needs-human',
        },
      ],
    );
    const itemId = project.workItemIds[0];

    const activation = harness.waitForEvent<NotificationActivation>(
      'notification:activated',
      (target) =>
        target.kind === 'workflow-item' && target.workItemId === itemId,
    );
    await expect(
      harness.rpc('HarnessNotify', 'Needs attention', 'Open workflow run', {
        kind: 'workflow-item',
        workItemId: itemId,
      }),
    ).rejects.toThrow(/method_error: OS notifications are unavailable/);
    await activation;

    await page.goto(harness.url);
    await expect(page.getByTestId('wf-run-detail')).toBeVisible();
    await expect(page.getByTestId('wf-title')).toHaveText('Open notified run');
    await expect(page.getByTestId('wf-sweep-counter')).toHaveText('1 of 1');
  },
);

deadItemTest(
  'dead workflow-item notification shows an error and falls back to overview',
  async ({ harness, page }) => {
    await pauseWorkflowQueue(harness);
    await seedWorkflowProject(harness, 'deeplink-dead', [
      {
        name: 'dead-flow',
        yaml: terminalWorkflow('dead-flow', 'done'),
      },
    ]);

    const activation = harness.waitForEvent<NotificationActivation>(
      'notification:activated',
      (target) =>
        target.kind === 'workflow-item' &&
        target.workItemId === 'dead-work-item',
    );
    await expect(
      harness.rpc(
        'HarnessNotify',
        'Needs attention',
        'Open missing workflow run',
        {
          kind: 'workflow-item',
          workItemId: 'dead-work-item',
        },
      ),
    ).rejects.toThrow(/method_error: OS notifications are unavailable/);
    await activation;

    await page.goto(harness.url);
    await expect(
      page.getByText('This workflow run no longer exists.', { exact: true }),
    ).toBeVisible();
    await expect(page.getByTestId('wf-overview')).toBeVisible();
    await expect(page.getByTestId('wf-title')).toHaveText('Workflows');
  },
);

triageTest(
  'cold triage notification opens its hidden workflow-triage thread',
  async ({ harness, page }) => {
    await pauseWorkflowQueue(harness);
    const project = await seedWorkflowProject(harness, 'deeplink-triage', [
      {
        name: 'triage-flow',
        yaml: terminalWorkflow('triage-flow', 'done'),
      },
    ]);

    const activation = harness.waitForEvent<NotificationActivation>(
      'notification:activated',
      (target) =>
        target.kind === 'workflow-triage-agent' &&
        target.projectId === project.projectId,
    );
    await expect(
      harness.rpc('HarnessNotify', 'Workflow summary', 'Open triage agent', {
        kind: 'workflow-triage-agent',
        projectId: project.projectId,
      }),
    ).rejects.toThrow(/method_error: OS notifications are unavailable/);
    await activation;

    const turnCompleted = harness.waitForEvent('provider:turn_completed');
    await page.goto(harness.url);
    await turnCompleted;
    const threadPane = page.locator('[data-pane-kind="thread"]');
    await expect(
      threadPane.getByText('Workflow triage agent', { exact: true }),
    ).toBeVisible();
    const threadId = await threadPane
      .locator('[data-ui-surface="chat"]')
      .getAttribute('data-thread-id');
    if (!threadId) {
      throw new Error('triage deep link did not open a persisted chat thread');
    }
    const triageThread = await harness.rpc<{
      mode: string;
      projectId: string;
    }>('GetThread', threadId);
    expect(triageThread.mode).toBe('workflow-triage');
    expect(triageThread.projectId).toBe(project.projectId);

    const sidebar = page.getByTestId('sidebar');
    const triageSidebarRows = sidebar
      .getByTestId('thread-row-title')
      .filter({ hasText: 'Workflow triage agent' });
    await expect(triageSidebarRows).toHaveCount(0);

    await page
      .getByTestId('sidebar-thread-search')
      .fill('Workflow triage agent');
    await expect(triageSidebarRows).toHaveCount(0);
  },
);
