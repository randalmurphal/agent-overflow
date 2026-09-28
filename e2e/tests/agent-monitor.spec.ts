// A background agent's own Monitor (claude-wire.md §E7). On a sidechain
// the Monitor ack carries no `tool_use_result`, only its text, so the
// text is what says the watch runs. The watch sits on the tray under its
// agent, the agent's stop parks on it ("Waiting on 1 background
// command"), and its terminal takes it off the tray and wakes the agent.
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  agentWakeLine,
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

const WATCH = 'until [ -f /tmp/gate.done ]; do sleep 5; done';
const WATCH_ACK =
  'Monitor started (task task-watch, timeout 1800000ms). You will be notified on each event. Keep working — do not poll or sleep.';
const REPORT = 'Gate started; waiting on the watch before I confirm.';
const WATCH_DONE = 'Monitor timed out after 1800000ms';

test("an agent's Monitor runs on the tray under it, parks the agent and leaves on its terminal", async ({ harness, page }) => {
  const agent = { task_id: 'task-bg', task_type: 'local_agent', description: 'gate watcher' };
  const watch = { task_id: 'task-watch', task_type: 'local_bash', description: 'gate done' };
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('agent-monitor', [
      emit([
        ...textLines('msg-lead', 'Launching the gate watcher.'),
        toolUseLine('msg-launch', 'tu-bg', 'Agent', {
          description: 'gate watcher',
          subagent_type: 'general-purpose',
          prompt: 'Watch the gate and report.',
        }),
        taskStartedLine('task-bg', 'tu-bg', 'gate watcher'),
        asyncAgentAckLine('tu-bg', 'task-bg', 'gate watcher'),
        backgroundTasksChangedLine([agent]),
        RESULT_LINE,
      ]),
      { waitSignal: { name: 'park' } },
      emit([
        toolUseLine('msg-s1', 'tu-watch', 'Monitor', { command: WATCH, description: 'gate done', timeout_ms: 1800000 }, 'tu-bg'),
        taskStartedLine('task-watch', 'tu-watch', 'gate done', { taskType: 'local_bash', ownedBySubagent: true }),
        toolResultLine('tu-watch', WATCH_ACK, { parentToolUseId: 'tu-bg' }),
        backgroundTasksChangedLine([agent, watch]),
        ...textLines('msg-s2', REPORT, 'tu-bg'),
        taskUpdatedLine('task-bg', { status: 'completed', end_time: 1787419835322 }),
        taskNotificationLine('task-bg', 'tu-bg', REPORT, {
          outputFile: '${CWD}/gate-output.jsonl',
          usage: { total_tokens: 12000, tool_uses: 1, duration_ms: 2100 },
          uuid: 'park-1',
        }),
        backgroundTasksChangedLine([watch]),
      ]),
      { waitSignal: { name: 'wake' } },
      emit([
        taskUpdatedLine('task-watch', { status: 'completed', end_time: 1787419845000 }),
        taskNotificationLine('task-watch', 'tu-watch', WATCH_DONE, { uuid: 'watch-1' }),
        backgroundTasksChangedLine([]),
        agentWakeLine('task-bg', 'gate watcher', {
          taskId: 'task-watch', toolUseId: 'tu-watch', status: 'completed', summary: WATCH_DONE,
        }),
        backgroundTasksChangedLine([agent]),
      ]),
    ]),
  });

  const threadId = await seedAgentThread(harness, 'agent-monitor-app', 'Agent monitor');
  await harness.open(page);
  await page.getByTestId('thread-row-title').getByText('Agent monitor', { exact: true }).click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'watch the gate', null);
  await harness.waitForEvent('provider:turn_completed');
  await page.getByTestId('activity-rail-background-toggle').click();
  const agentRow = page.locator('[data-testid="background-task-tray-row"][data-row-id="tu-bg"]');
  const watchRow = page.locator('[data-testid="background-task-tray-row"][data-row-id="tu-watch"]');
  await expect(agentRow).toHaveCount(1);

  await waitForGate(harness, 'park');
  await advance(harness, mockId, 'park');
  await expect(watchRow).toHaveCount(1);
  await expect(watchRow).toHaveAttribute('data-depth', '1');
  await expect(agentRow.getByTestId('background-task-tray-row-status')).toHaveAttribute('data-run-state', 'parked');
  await expect(agentRow.getByTestId('background-task-tray-row-activity')).toHaveText('Waiting on 1 background command');
  await expect(page.getByTestId('activity-rail-background-running-label')).toHaveText('1 running');

  await waitForGate(harness, 'wake');
  await advance(harness, mockId, 'wake');
  await expect(watchRow).toHaveCount(0);
  await expect(agentRow.getByTestId('background-task-tray-row-status')).toHaveAttribute('data-run-state', 'running');
});
