// An agent pane's own background tray (docs/specs/agent-visibility.md,
// §Background tray). The pane lists the background rows under the agent
// it shows, the same rows the thread's tray nests under it: its shells,
// its Monitor, its nested agent and that agent's shell, with depth counted
// from the agent. Rows outside the agent (the main thread's own shell)
// stay off it. A listed agent's open button descends the pane into it. A
// row's Stop stops that task. Stop All stops only the pane's rows: the
// nested agent with the shell it owns, and the agent's own commands; the
// thread's tray keeps the main shell and the agent.
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  asyncAgentAckLine,
  backgroundTasksChangedLine,
  claudeScenario,
  emit,
  seedAgentThread,
  shellBackgroundAckLine,
  startMock,
  taskStartedLine,
  textLines,
  toolResultLine,
  toolUseLine,
} from './agent-visibility-helpers.js';

const WATCH_ACK =
  'Monitor started (task task-watch, timeout 1800000ms). You will be notified on each event. Keep working — do not poll or sleep.';

test("an agent pane's tray lists the agent's rows and stops only them", async ({ harness, page }) => {
  const outer = { task_id: 'task-outer', task_type: 'local_agent', description: 'outer worker' };
  const inner = { task_id: 'task-inner', task_type: 'local_agent', description: 'inner worker' };
  const main = { task_id: 'task-main', task_type: 'local_bash', description: 'main watch' };
  const sh1 = { task_id: 'task-sh1', task_type: 'local_bash', description: 'outer build' };
  const sh2 = { task_id: 'task-sh2', task_type: 'local_bash', description: 'outer serve' };
  const watch = { task_id: 'task-watch', task_type: 'local_bash', description: 'gate done' };
  const ish = { task_id: 'task-ish', task_type: 'local_bash', description: 'inner tail' };
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('agent-pane-tray', [
      emit([
        ...textLines('msg-lead', 'Starting the main watch and the outer worker.'),
        toolUseLine('msg-main', 'tu-main', 'Bash', { command: 'watch main', run_in_background: true }),
        taskStartedLine('task-main', 'tu-main', 'main watch', { taskType: 'local_bash' }),
        toolResultLine('tu-main', 'Command running in background with ID: task-main.', {
          toolUseResult: { stdout: '', stderr: '', interrupted: false, backgroundTaskId: 'task-main' },
        }),
        toolUseLine('msg-outer', 'tu-outer', 'Agent', { description: 'outer worker', subagent_type: 'outer-worker' }),
        taskStartedLine('task-outer', 'tu-outer', 'outer worker'),
        asyncAgentAckLine('tu-outer', 'task-outer', 'outer worker'),
        toolUseLine('msg-sh1', 'tu-sh1', 'Bash', { command: 'make build', run_in_background: true }, 'tu-outer'),
        taskStartedLine('task-sh1', 'tu-sh1', 'outer build', { taskType: 'local_bash', ownedBySubagent: true }),
        shellBackgroundAckLine('tu-sh1', 'task-sh1', 'tu-outer'),
        toolUseLine('msg-sh2', 'tu-sh2', 'Bash', { command: 'make serve', run_in_background: true }, 'tu-outer'),
        taskStartedLine('task-sh2', 'tu-sh2', 'outer serve', { taskType: 'local_bash', ownedBySubagent: true }),
        shellBackgroundAckLine('tu-sh2', 'task-sh2', 'tu-outer'),
        toolUseLine('msg-watch', 'tu-watch', 'Monitor', { command: 'until [ -f /tmp/gate ]; do sleep 5; done', description: 'gate done', timeout_ms: 1800000 }, 'tu-outer'),
        taskStartedLine('task-watch', 'tu-watch', 'gate done', { taskType: 'local_bash', ownedBySubagent: true }),
        toolResultLine('tu-watch', WATCH_ACK, { parentToolUseId: 'tu-outer' }),
        toolUseLine('msg-inner', 'tu-inner', 'Agent', { description: 'inner worker', subagent_type: 'inner-worker' }, 'tu-outer'),
        taskStartedLine('task-inner', 'tu-inner', 'inner worker'),
        asyncAgentAckLine('tu-inner', 'task-inner', 'inner worker', 'tu-outer'),
        toolUseLine('msg-ish', 'tu-ish', 'Bash', { command: 'tail -f log', run_in_background: true }, 'tu-inner'),
        taskStartedLine('task-ish', 'tu-ish', 'inner tail', { taskType: 'local_bash', ownedBySubagent: true }),
        shellBackgroundAckLine('tu-ish', 'task-ish', 'tu-inner'),
        backgroundTasksChangedLine([main, outer, sh1, sh2, watch, inner, ish]),
        RESULT_LINE,
      ]),
    ]),
  });

  const threadId = await seedAgentThread(harness, 'agent-pane-tray-app', 'Agent pane tray');
  await harness.open(page);
  await page.getByTestId('thread-row-title').getByText('Agent pane tray', { exact: true }).click();
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'start the work', null);
  await harness.waitForEvent('provider:turn_completed');

  const mainBody = page.getByTestId('activity-rail-background-body');
  const mainRow = (id: string) => mainBody.locator(`[data-testid="background-task-tray-row"][data-row-id="${id}"]`);
  await page.getByTestId('activity-rail-background-toggle').click();
  await expect(mainBody.getByTestId('background-task-tray-row')).toHaveCount(7);
  await expect(mainRow('tu-ish')).toHaveAttribute('data-depth', '2');
  await mainRow('tu-outer').getByTestId('background-task-tray-row-open').click();

  // The outer agent's pane: its rows, rebased to its depth, nothing else.
  const pane = page.getByTestId('companion-pane-agent-body');
  await expect(pane.getByTestId('agent-pane-breadcrumb-current')).toContainText('Outer Worker');
  await expect(pane.getByTestId('agent-pane-background-count')).toHaveText('5');
  await expect(pane.getByTestId('agent-pane-background-pulse')).toBeVisible();
  await expect(pane.getByTestId('agent-pane-background-body')).toHaveCount(0);
  await pane.getByTestId('agent-pane-background-toggle').click();
  const paneBody = pane.getByTestId('agent-pane-background-body');
  const paneRows = paneBody.getByTestId('background-task-tray-row');
  const paneRow = (id: string) => paneBody.locator(`[data-testid="background-task-tray-row"][data-row-id="${id}"]`);
  await expect(paneRows).toHaveCount(5);
  expect(await paneRows.evaluateAll((rows) => rows.map((row) => `${row.getAttribute('data-row-id')}@${row.getAttribute('data-depth')}`)))
    .toEqual(['tu-sh1@0', 'tu-sh2@0', 'tu-watch@0', 'tu-inner@0', 'tu-ish@1']);
  await expect(paneBody.getByTestId('agent-pane-background-running-label')).toHaveText('5 running');

  // A listed agent's open button descends the pane into it; the open tray
  // follows the pane's scope.
  await paneRow('tu-inner').getByTestId('background-task-tray-row-open').click();
  await expect(pane.getByTestId('agent-pane-breadcrumb-current')).toContainText('Inner Worker');
  await expect(pane.getByTestId('agent-pane-background-count')).toHaveText('1');
  await expect(paneRows).toHaveCount(1);
  await expect(paneRow('tu-ish')).toHaveAttribute('data-depth', '0');
  await pane.getByTestId('agent-pane-breadcrumb-entry').filter({ hasText: 'Outer Worker' }).click();
  await expect(pane.getByTestId('agent-pane-breadcrumb-current')).toContainText('Outer Worker');
  await expect(paneRows).toHaveCount(5);

  // One row's Stop stops that task, on both trays.
  await paneBody.locator('[data-row-stop-id="tu-sh1"]').click();
  await expect(paneRow('tu-sh1')).toHaveCount(0);
  await expect(mainRow('tu-sh1')).toHaveCount(0);
  await expect(pane.getByTestId('agent-pane-background-count')).toHaveText('4');

  // Stop All stops the pane's rows and nothing else.
  await paneBody.getByTestId('agent-pane-background-stop-all').click();
  await expect(pane.getByTestId('agent-pane-background-toggle')).toHaveCount(0);
  await expect(paneBody).toHaveCount(0);
  await expect(mainBody.getByTestId('background-task-tray-row')).toHaveCount(2);
  await expect(mainRow('tu-main')).toHaveCount(1);
  await expect(mainRow('tu-outer')).toHaveCount(1);
  await expect(page.getByTestId('activity-rail-background-running-label')).toHaveText('2 running');
});
