// A nested async agent that outlives the agent that launched it
// (claude-wire.md §Background task ownership, fixture D). The parent's
// stop is final while the child runs. The tray keeps the child, moved up
// to a root once its parent leaves. The parent's card is a snapshot up to
// the parent's stop, so the child's stop is its own card in the main
// timeline, after the parent's, and the parent's card never changes.
import { test, expect } from './fixtures.js';
import {
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
  toolUseLine,
  waitForGate,
} from './agent-visibility-helpers.js';

test('a nested agent that outlives its parent gets its own card in the main timeline', async ({ harness, page }) => {
  const outer = { task_id: 'task-outer', task_type: 'local_agent', description: 'outer reviewer' };
  const inner = { task_id: 'task-inner', task_type: 'local_agent', description: 'inner sleeper' };
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('nested-outlives-parent', [
      emit([
        ...textLines('msg-lead', 'Launching the outer reviewer.'),
        toolUseLine('msg-outer', 'tu-outer', 'Agent', { description: 'outer reviewer', subagent_type: 'general-purpose' }),
        taskStartedLine('task-outer', 'tu-outer', 'outer reviewer'),
        asyncAgentAckLine('tu-outer', 'task-outer', 'outer reviewer'),
        toolUseLine('msg-inner', 'tu-inner', 'Agent', { description: 'inner sleeper', subagent_type: 'general-purpose' }, 'tu-outer'),
        taskStartedLine('task-inner', 'tu-inner', 'inner sleeper'),
        asyncAgentAckLine('tu-inner', 'task-inner', 'inner sleeper', 'tu-outer'),
        backgroundTasksChangedLine([outer, inner]),
        RESULT_LINE,
      ]),
      { waitSignal: { name: 'outer-end' } },
      emit([
        ...textLines('msg-outer-report', 'OUTER DONE', 'tu-outer'),
        taskUpdatedLine('task-outer', { status: 'completed', end_time: 1787415964000 }),
        taskNotificationLine('task-outer', 'tu-outer', 'OUTER DONE', {
          usage: { total_tokens: 9000, tool_uses: 1, duration_ms: 2000 },
          uuid: 'outer-1',
        }),
        backgroundTasksChangedLine([inner]),
      ]),
      { waitSignal: { name: 'inner-end' } },
      emit([
        ...textLines('msg-inner-report', 'INNER DONE', 'tu-inner'),
        taskUpdatedLine('task-inner', { status: 'completed', end_time: 1787415994000 }),
        taskNotificationLine('task-inner', 'tu-inner', 'INNER DONE', {
          usage: { total_tokens: 7000, tool_uses: 1, duration_ms: 30000 },
          uuid: 'inner-1',
        }),
        backgroundTasksChangedLine([]),
      ]),
    ]),
  });

  const threadId = await seedAgentThread(harness, 'nested-outlives-app', 'Nested outlives parent');
  await harness.open(page);
  await page.getByTestId('thread-row-title').getByText('Nested outlives parent', { exact: true }).click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'review deeply', null);
  await harness.waitForEvent('provider:turn_completed');

  const timeline = page.locator(
    '[data-testid="message-timeline-scroll"]:not([data-testid="agent-pane-timeline"] [data-testid="message-timeline-scroll"])',
  );
  const cards = timeline.getByTestId('subagent-group');
  await page.getByTestId('activity-rail-background-toggle').click();
  const outerRow = page.locator('[data-testid="background-task-tray-row"][data-row-id="tu-outer"]');
  const innerRow = page.locator('[data-testid="background-task-tray-row"][data-row-id="tu-inner"]');
  await expect(innerRow).toHaveAttribute('data-depth', '1');

  // The parent ends: its card lands, the child runs on as a tray root.
  await waitForGate(harness, 'outer-end');
  await advance(harness, mockId, 'outer-end');
  await expect(cards).toHaveCount(1);
  const outerCard = cards.nth(0);
  await expect(outerCard).toHaveAttribute('data-anchor-id', 'complete:tu-outer');
  await expect(outerCard.getByTestId('subagent-group-preview')).toHaveText('OUTER DONE');
  await expect(outerRow).toHaveCount(0);
  await expect(innerRow).toHaveAttribute('data-depth', '0');
  await expect(page.getByTestId('activity-rail-background-running-label')).toHaveText('1 running');

  // The child ends: its own card, after the parent's, which is unchanged.
  await waitForGate(harness, 'inner-end');
  await advance(harness, mockId, 'inner-end');
  await expect(cards).toHaveCount(2);
  const innerCard = cards.nth(1);
  await expect(innerCard).toHaveAttribute('data-anchor-id', 'complete:tu-inner');
  await expect(innerCard.getByTestId('subagent-group-preview')).toHaveText('INNER DONE');
  await expect(outerCard).toHaveAttribute('data-anchor-id', 'complete:tu-outer');
  await expect(outerCard.getByTestId('subagent-group-preview')).toHaveText('OUTER DONE');
  await expect(innerRow).toHaveCount(0);
});
