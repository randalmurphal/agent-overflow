// Compact activity rail: both controls, a wide animation, elapsed time and
// tokens fit one fixed-height row; the usage popover retains cost.
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE, advance, asyncAgentAckLine, backgroundTasksChangedLine,
  claudeUsageResult, emit, seedAgentThread, startMock, taskNotificationLine,
  taskStartedLine, taskUpdatedLine, toolResultLine, toolUseLine, waitForGate,
} from './agent-visibility-helpers.js';

test('todos and background controls fit beside a wide spinner and tokens', async ({ harness, page }) => {
  await harness.rpc('UpdateSettings', { spinnerAnimationsEnabled: true, spinnerCompactionAnimation: 'nyan-cat' });
  const tasks = Array.from({ length: 4 }, (_, i) => ({ task_id: `task-${i}`, task_type: 'local_agent', description: `Research source ${i}` }));
  await harness.rpc('HarnessSetScenario', { scenario: {
    version: 1, name: 'compact-rail', provider: 'claude', afterTurns: 'silent',
    turns: [
      { label: 'usage', steps: [emit([claudeUsageResult(1234, 30800)])] },
      { label: 'crowded-rail', steps: [
        emit([
          toolUseLine('msg-todo', 'todo', 'TodoWrite', { todos: Array.from({ length: 6 }, (_, i) => ({
            content: `Research source ${i}`, activeForm: `Researching source ${i}`, status: i < 2 ? 'completed' : 'in_progress',
          })) }),
          toolResultLine('todo', 'Todos updated'),
          ...tasks.flatMap((task, i) => [
            toolUseLine(`msg-${i}`, `agent-${i}`, 'Agent', { description: task.description, subagent_type: 'researcher' }),
            taskStartedLine(task.task_id, `agent-${i}`, task.description),
            asyncAgentAckLine(`agent-${i}`, task.task_id, task.description),
          ]),
          backgroundTasksChangedLine(tasks),
          JSON.stringify({ type: 'system', subtype: 'status', status: 'compacting' }),
        ]),
        { waitSignal: { name: 'finish' } },
        emit([
          ...tasks.flatMap((task, i) => [
            taskUpdatedLine(task.task_id, { status: 'completed', end_time: 1787415964725 }),
            taskNotificationLine(task.task_id, `agent-${i}`, 'Done'),
          ]),
          backgroundTasksChangedLine([]), RESULT_LINE,
        ]),
      ] },
    ],
  } });
  const threadId = await seedAgentThread(harness, 'compact-rail-app', 'Crowded rail');
  await harness.open(page);
  await page.getByTestId('thread-row').click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'report usage', null);
  await harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, 'research sources', null);
  await waitForGate(harness, 'finish');
  const row = page.locator('[data-activity-rail-row]');
  const todos = row.getByRole('button', { name: 'Todos 2/6', exact: true });
  const background = row.getByRole('button', { name: 'Background 4', exact: true });
  await expect(todos.locator('.lucide-list-todo')).toBeVisible();
  await expect(background.locator('.lucide-send-to-back')).toBeVisible();
  await expect(row.locator('.working-sprite')).toHaveAttribute('data-sprite-id', 'nyan-cat');
  await expect(row.getByTestId('usage-chip-trigger')).toHaveText('30.8k');
  const height = (await row.boundingBox())!.height;
  for (const width of [412, 360, 320, 412]) {
    await page.setViewportSize({ width, height: 915 });
    await expect.poll(() => row.evaluate(el => {
      const outer = el.getBoundingClientRect();
      const controls = Array.from(el.querySelectorAll<HTMLElement>('button'));
      return el.scrollWidth <= el.clientWidth + 1 && controls.every(button => {
        const box = button.getBoundingClientRect();
        return button.scrollWidth <= button.clientWidth + 1 && box.left >= outer.left && box.right <= outer.right + 1;
      });
    })).toBe(true);
    expect((await row.boundingBox())!.height).toBeCloseTo(height, 1);
    expect((await row.locator('.working-sprite').boundingBox())!.width).toBeLessThanOrEqual(32);
  }
  await todos.tap();
  await expect(todos).toHaveAttribute('aria-expanded', 'true');
  await background.tap();
  await expect(background).toHaveAttribute('aria-expanded', 'true');
  await row.getByTestId('usage-chip-trigger').tap();
  await expect(page.getByTestId('usage-chip-cost')).toContainText('$');
  await page.getByTestId('chat-header-title').tap();
  await advance(harness, mockId, 'finish');
  await expect(row.getByTestId('activity-rail-working')).toHaveCount(0);
});
