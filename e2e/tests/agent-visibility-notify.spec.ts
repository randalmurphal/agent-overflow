// Agent visibility — the NOTIFICATION criterion
// (docs/specs/agent-visibility.md § "Success criteria", item 7, Q11):
//
//   Top-level completions notify; nested completions do not.
//
// Two backgrounded agents finish in the same turn: one launched by the
// main thread, one launched by that agent. Only the first is entitled to
// a bell; the second updates its card and says nothing.
//
// SURFACE NOTE. The bell IS the persisted `notification` row — nothing
// sends an OS notification for a background completion (`notifyOS` has
// three callers, none of them this path), and the row is deliberately
// hidden from the timeline once its completed lifecycle sibling exists
// (utils/notificationFilter.ts, user ruling 2026-08-22), which for an
// agent is always. So the BELL is asserted where it lives rather than in
// the DOM. What IS asserted in the DOM is the row the bell's hiding
// depends on: the top-level completion sibling, rendered at the
// completion point as a compact agent row. An earlier version of this
// spec asserted only SQLite, which is how the grouping pass dropping that
// row shipped unnoticed (2026-08-22).
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  asyncAgentAckLine,
  backgroundTasksChangedLine,
  claudeScenario,
  emit,
  itemMeta,
  listItems,
  seedAgentThread,
  startMock,
  taskNotificationLine,
  taskStartedLine,
  taskUpdatedLine,
  textLines,
  toolUseLine,
} from './agent-visibility-helpers.js';

test('a top-level background completion writes a bell and a nested one does not', async ({
  harness,
  page,
}) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('notify-depth', [
      emit([
        ...textLines('msg-lead', 'Launching the outer runner.'),
        toolUseLine('msg-top', 'tu-top', 'Agent', {
          description: 'outer runner',
          subagent_type: 'top-runner',
        }),
        taskStartedLine('task-top', 'tu-top', 'outer runner'),
        asyncAgentAckLine('tu-top', 'task-top', 'outer runner'),
        backgroundTasksChangedLine([
          { task_id: 'task-top', task_type: 'local_agent', description: 'outer runner' },
        ]),

        // The outer agent launches its own.
        toolUseLine('msg-nested', 'tu-nested', 'Agent', {
          description: 'nested runner',
          subagent_type: 'nested-runner',
        }, 'tu-top'),
        taskStartedLine('task-nested', 'tu-nested', 'nested runner', { ownedBySubagent: true }),
        asyncAgentAckLine('tu-nested', 'task-nested', 'nested runner', 'tu-top'),
        backgroundTasksChangedLine([
          { task_id: 'task-top', task_type: 'local_agent', description: 'outer runner' },
          { task_id: 'task-nested', task_type: 'local_agent', description: 'nested runner' },
        ]),
        RESULT_LINE,

        // Both terminals, innermost first.
        taskUpdatedLine('task-nested', { status: 'completed', end_time: 1787419835322 }),
        taskNotificationLine('task-nested', 'tu-nested', 'Nested runner finished.', {
          usage: { total_tokens: 8421, tool_uses: 4, duration_ms: 1900 },
        }),
        taskUpdatedLine('task-top', { status: 'completed', end_time: 1787419835999 }),
        taskNotificationLine('task-top', 'tu-top', 'Outer runner finished.', {
          usage: { total_tokens: 30500, tool_uses: 9, duration_ms: 4100 },
        }),
        backgroundTasksChangedLine([]),
      ]),
    ]),
  });

  const threadId = await seedAgentThread(harness, 'notify-app', 'Nested notify');
  await page.goto(harness.url);
  await page.getByText('Nested notify').click();
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'run both', null);
  await harness.waitForEvent('provider:turn_completed');

  // --- One bell, and it belongs to the top-level launch -------------
  await expect
    .poll(async () => {
      const items = await listItems(harness, threadId);
      // Both agents must have settled first, or "no nested bell" would
      // just mean "not yet".
      const settled = items.filter(
        (i) => i.completionOf === 'tu-top' || i.completionOf === 'tu-nested',
      );
      if (settled.length !== 2) return null;
      return items
        .filter((i) => i.kind === 'notification')
        .map((i) => ({
          id: i.id,
          taskId: itemMeta(i).task_id,
          parentId: i.parentId ?? '',
        }));
    })
    .toEqual([
      {
        id: 'task-notification:task-top:notify-task-top',
        taskId: 'task-top',
        parentId: '',
      },
    ]);

  // --- The completion is IN the transcript, where it completed --------
  // The bell is hidden on the strength of this row existing. It renders
  // as a compact agent row (the launch is a card; its completion is a
  // leaf at the completion point, after the turn's prose), settled, and
  // named after the agent the card named.
  const timeline = page.getByTestId('message-timeline-scroll');
  const completionRow = timeline.locator('[data-item-id="complete:tu-top"]');
  await expect(completionRow).toHaveCount(1);
  await expect(completionRow.getByTestId('agent-row-preview')).toContainText('Top Runner');
  // Settled: a completed row renders no status dot at all (the indicator
  // exists only for running / backgrounded / failed states).
  await expect(completionRow.getByTestId('agent-row-status')).toHaveCount(0);
  await expect(completionRow.getByTestId('agent-row-open-pane')).toHaveCount(1);
  // The nested completion sits inside the outer card, never at top level.
  await expect(timeline.locator('[data-item-id="complete:tu-nested"]')).toHaveCount(0);

  // --- The nested completion is not silent about ITSELF -------------
  // "Nested completions do not notify" is a claim about the bell only:
  // the card still folds in the notification's final usage.
  const topCard = timeline.getByTestId('subagent-group').first();
  await expect(topCard.getByTestId('subagent-group-label')).toContainText('Top Runner');
  await topCard.getByTestId('subagent-group-toggle').first().click();
  const nestedCard = topCard
    .getByTestId('subagent-group-body')
    .first()
    .getByTestId('subagent-group')
    .first();
  await expect(nestedCard.getByTestId('subagent-group-label')).toContainText('Nested Runner');
  await expect(nestedCard.getByTestId('subagent-group-tools')).toHaveText('4 tools');
  await expect(topCard.getByTestId('subagent-group-tools').first()).toHaveText('9 tools');
});
