// A background agent's FOREGROUND Bash the CLI moved to the background
// while it ran (claude-wire.md §E2b): `task_updated{is_backgrounded:true}`
// puts the command on the tray, then its result decides what it is.
//
//   moved ack  - "did not complete within its Ns timeout and was moved to
//                the background (ID: ...)": the command keeps running on
//                the tray under its agent until its own terminal takes it
//                off.
//   real output - the command finished as it was moved: the result settles
//                the row in place and it leaves the tray at once.
import type { Page } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';
import { test, expect } from './fixtures.js';
import {
  type ScenarioStep,
  RESULT_LINE,
  advance,
  asyncAgentAckLine,
  backgroundTasksChangedLine,
  claudeScenario,
  emit,
  seedAgentThread,
  startMock,
  taskNotificationLine,
  taskStartedLine,
  taskUpdatedLine,
  textLines,
  toolResultLine,
  toolUseLine,
  waitForGate,
} from './agent-visibility-helpers.js';

const COMMAND = 'go test ./...';
const MOVED_ACK =
  'Command did not complete within its 300s timeout and was moved to the background (ID: task-shell). Output is being written to: /tmp/tasks/task-shell.output. You will be notified when it completes.';

const agent = { task_id: 'task-bg', task_type: 'local_agent', description: 'test runner' };
const shell = { task_id: 'task-shell', task_type: 'local_bash', description: COMMAND };

// The launch of the background agent, then its foreground Bash up to the
// moment the CLI moves it: the patch lands before the result, as on the wire.
function launchAndMove(): ScenarioStep[] {
  return [
    emit([
      ...textLines('msg-lead', 'Launching the test runner.'),
      toolUseLine('msg-launch', 'tu-bg', 'Agent', {
        description: 'test runner',
        subagent_type: 'general-purpose',
        prompt: 'Run the tests and report.',
      }),
      taskStartedLine('task-bg', 'tu-bg', 'test runner'),
      asyncAgentAckLine('tu-bg', 'task-bg', 'test runner'),
      backgroundTasksChangedLine([agent]),
      RESULT_LINE,
    ]),
    { waitSignal: { name: 'move' } },
    emit([
      toolUseLine('msg-s1', 'tu-shell', 'Bash', { command: COMMAND, timeout: 300000 }, 'tu-bg'),
      taskStartedLine('task-shell', 'tu-shell', COMMAND, { taskType: 'local_bash', ownedBySubagent: true }),
      taskUpdatedLine('task-shell', { is_backgrounded: true }),
      backgroundTasksChangedLine([agent, shell]),
    ]),
  ];
}

async function openTray(harness: HarnessApp, page: Page, title: string): Promise<string> {
  const threadId = await seedAgentThread(harness, `${title}-app`, title);
  await harness.open(page);
  await page.getByTestId('thread-row-title').getByText(title, { exact: true }).click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'run the tests', null);
  await harness.waitForEvent('provider:turn_completed');
  await page.getByTestId('activity-rail-background-toggle').click();
  await expect(page.locator('[data-testid="background-task-tray-row"][data-row-id="tu-bg"]')).toHaveCount(1);
  return mockId;
}

test('a moved foreground shell that acks the move runs on the tray until its terminal', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('moved-shell-ack', [
      ...launchAndMove(),
      emit([toolResultLine('tu-shell', MOVED_ACK, { parentToolUseId: 'tu-bg' })]),
      { waitSignal: { name: 'done' } },
      emit([
        taskUpdatedLine('task-shell', { status: 'completed', end_time: 1787419845000 }),
        taskNotificationLine('task-shell', 'tu-shell', `Background command "${COMMAND}" completed (exit code 0)`, { uuid: 'shell-1' }),
        backgroundTasksChangedLine([agent]),
      ]),
    ]),
  });
  const mockId = await openTray(harness, page, 'Moved shell ack');
  const shellRow = page.locator('[data-testid="background-task-tray-row"][data-row-id="tu-shell"]');

  await waitForGate(harness, 'move');
  await advance(harness, mockId, 'move');
  await expect(shellRow).toHaveCount(1);
  await expect(shellRow).toHaveAttribute('data-depth', '1');
  await expect(page.getByTestId('activity-rail-background-running-label')).toHaveText('2 running');

  // The ack is not a result: the command still runs.
  await waitForGate(harness, 'done');
  await expect(shellRow).toHaveCount(1);
  await expect(page.getByTestId('activity-rail-background-running-label')).toHaveText('2 running');

  await advance(harness, mockId, 'done');
  await expect(shellRow).toHaveCount(0);
  await expect(page.getByTestId('activity-rail-background-running-label')).toHaveText('1 running');
});

test('a moved foreground shell that answers with its output leaves the tray', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('moved-shell-output', [
      ...launchAndMove(),
      { waitSignal: { name: 'output' } },
      emit([
        toolResultLine('tu-shell', 'ok  \tagent-overflow/internal/store\t12.1s', { parentToolUseId: 'tu-bg' }),
        backgroundTasksChangedLine([agent]),
      ]),
    ]),
  });
  const mockId = await openTray(harness, page, 'Moved shell output');
  const shellRow = page.locator('[data-testid="background-task-tray-row"][data-row-id="tu-shell"]');

  await waitForGate(harness, 'move');
  await advance(harness, mockId, 'move');
  await expect(shellRow).toHaveCount(1);
  await expect(page.getByTestId('activity-rail-background-running-label')).toHaveText('2 running');

  await waitForGate(harness, 'output');
  await advance(harness, mockId, 'output');
  await expect(shellRow).toHaveCount(0);
  await expect(page.getByTestId('activity-rail-background-running-label')).toHaveText('1 running');
});
