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
// depends on: the top-level completion sibling, at which the agent's CARD
// renders (the launch row is the immutable spawn record — ruling
// 2026-08-23). An earlier version of this spec asserted only SQLite,
// which is how the grouping pass dropping that row shipped unnoticed
// (2026-08-22).
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
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
  toolResultLine,
  toolUseLine,
  waitForGate,
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
  await harness.open(page);
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
  // The bell is hidden on the strength of the completion rendering. The
  // launch row stays where it was as the immutable spawn record — label,
  // the backgrounded indicator icon (no text pill), the open-in-pane
  // door, no duration — and the agent's CARD sits at the completion point, after
  // the turn's prose: status, duration, tool count, the transcript.
  const timeline = page.getByTestId('message-timeline-scroll');
  const spawnRow = timeline.locator('[data-item-id="tu-top"]');
  await expect(spawnRow.getByTestId('agent-row-preview')).toContainText('Top Runner');
  await expect(spawnRow.getByTestId('agent-row-status')).toHaveAttribute('data-state', 'backgrounded');
  await expect(spawnRow.getByText('background', { exact: true })).toHaveCount(0);
  await expect(spawnRow.getByTestId('agent-row-duration')).toHaveText('');
  await expect(spawnRow.getByTestId('agent-row-open-pane')).toHaveCount(1);
  // The open-pane door sits LEFT of the timestamp (AGENTS.md Row Contract).
  await spawnRow.hover();
  const [doorBox, timeBox] = await Promise.all([
    spawnRow.getByTestId('agent-row-open-pane').boundingBox(),
    spawnRow.getByTestId('agent-row-time').boundingBox(),
  ]);
  expect(doorBox!.x + doorBox!.width).toBeLessThanOrEqual(timeBox!.x);

  // Exactly one top-level card, and it is below the spawn row and the
  // prose written while the agent ran. The completion row has no leaf of
  // its own any more: the card IS the completion point.
  await expect(timeline.getByTestId('subagent-group')).toHaveCount(1);
  const topCard = timeline.getByTestId('subagent-group').first();
  await expect(topCard.getByTestId('subagent-group-label')).toContainText('Top Runner');
  await expect(topCard).toHaveAttribute('data-background', 'true');
  await expect(topCard.getByTestId('subagent-group-duration')).not.toHaveText('');
  await expect(topCard.getByTestId('subagent-group-tools').first()).toHaveText('9 tools');
  await expect(timeline.locator('[data-item-id="complete:tu-top"]')).toHaveCount(0);
  const [spawnBox, cardBox] = await Promise.all([spawnRow.boundingBox(), topCard.boundingBox()]);
  expect(cardBox!.y).toBeGreaterThan(spawnBox!.y + spawnBox!.height - 1);
  // The nested completion sits inside the outer card, never at top level.
  await expect(timeline.locator('[data-item-id="complete:tu-nested"]')).toHaveCount(0);

  // --- The nested agent stays out of the inline digest ---------------
  // "Nested completions do not notify" covers the bell; the nested
  // agent's card is also absent HERE by design — the digest never
  // recursively embeds child agents (ed6d2b40). Its transcript lives in
  // the agent pane, reached through the top agent's pane.
  await topCard.getByTestId('subagent-group-toggle').first().click();
  const topBody = topCard.getByTestId('subagent-group-body').first();
  await expect(topBody).toBeVisible();
  await expect(topBody.getByTestId('subagent-group')).toHaveCount(0);
  // What DOES render for the nested launch is its spawn row — the same
  // immutable agent row the main timeline gets, door included.
  const nestedSpawnRow = topBody.locator('[data-item-id="tu-nested"]');
  await expect(nestedSpawnRow.getByTestId('agent-row-preview')).toContainText('Nested Runner');
  await expect(nestedSpawnRow.getByTestId('agent-row-status')).toHaveAttribute('data-state', 'backgrounded');
});

// The card that lands at the completion sibling must know how many rows
// its transcript has WITHOUT a page read: while the agent ran collapsed,
// the pane folded its settled rows out of memory, and a completed card
// reads its saved aggregates rather than the live fold. Those aggregates
// are stamped on the sibling at write time (triage
// completionMetaWithSubagentAggregates). A bare sibling rendered "No
// child entries captured" for a 144-row transcript (2026-09-17).
test('a background agent’s card lands with its entry count and hydrates on expand', async ({
  harness,
  page,
}) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('count-on-landing', [
      emit([
        ...textLines('msg-lead', 'Launching the shard reviewer.'),
        toolUseLine('msg-bg', 'tu-bg', 'Agent', {
          description: 'shard reviewer',
          subagent_type: 'shard-reviewer',
          prompt: 'Review the first shard.',
        }),
        taskStartedLine('task-bg', 'tu-bg', 'shard reviewer'),
        asyncAgentAckLine('tu-bg', 'task-bg', 'shard reviewer'),
        backgroundTasksChangedLine([
          { task_id: 'task-bg', task_type: 'local_agent', description: 'shard reviewer' },
        ]),
        RESULT_LINE,
      ]),
      // The agent's sidechain streams while nothing renders it: no card
      // yet (a detached launch's card is its completion), no pane open,
      // so every settled row folds out of memory as it lands.
      { waitSignal: { name: 'stream' } },
      emit([
        ...textLines('msg-s1', 'Reading the first shard.', 'tu-bg'),
        toolUseLine('msg-s2', 'tu-read', 'Read', { file_path: '${CWD}/README.md' }, 'tu-bg'),
        toolResultLine('tu-read', '# fixture', { parentToolUseId: 'tu-bg' }),
        ...textLines('msg-s3', 'Shard reviewed: nothing drifted.', 'tu-bg'),
      ]),
      { waitSignal: { name: 'settle' } },
      // The envelope's summary is the report. Completion never reads the
      // output file, so it is never written.
      emit([
        taskUpdatedLine('task-bg', { status: 'completed', end_time: 1787419835322 }),
        taskNotificationLine('task-bg', 'tu-bg', 'Shard reviewed: nothing drifted.', {
          outputFile: '${CWD}/shard-output.jsonl',
          usage: { total_tokens: 12000, tool_uses: 1, duration_ms: 2100 },
        }),
        backgroundTasksChangedLine([]),
      ]),
    ]),
  });

  const threadId = await seedAgentThread(harness, 'count-app', 'Count on landing');
  await harness.open(page);
  await page.getByText('Count on landing').click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'review the shard', null);
  await harness.waitForEvent('provider:turn_completed');

  const timeline = page.getByTestId('message-timeline-scroll');
  await expect(timeline.locator('[data-item-id="tu-bg"]').getByTestId('agent-row-status')).toHaveAttribute('data-state', 'backgrounded');
  await expect(timeline.getByTestId('subagent-group')).toHaveCount(0);

  await waitForGate(harness, 'stream');
  await advance(harness, mockId, 'stream');
  // The rows are persisted under the launch before the agent settles: the
  // opening prompt from the launch input, two text rows and the Read.
  await expect
    .poll(async () => {
      const items = await listItems(harness, threadId);
      const rows = items.filter((i) => i.parentId === 'tu-bg');
      return rows.some((i) => i.summary?.includes('nothing drifted')) ? rows.length : 0;
    })
    .toBe(4);
  await expect(timeline.getByTestId('subagent-group')).toHaveCount(0);

  await waitForGate(harness, 'settle');
  await advance(harness, mockId, 'settle');
  const card = timeline.getByTestId('subagent-group').first();
  await expect(card).toHaveAttribute('data-background', 'true');
  await expect(card.getByTestId('subagent-group-count')).toHaveText('4 entries');
  await expect(card.getByTestId('subagent-group-preview')).toContainText('Shard reviewed: nothing drifted.');
  await expect(card.getByTestId('subagent-group-output-error')).toHaveCount(0);

  // Expanding hydrates the folded rows back from the store.
  await card.getByTestId('subagent-group-toggle').first().click();
  const body = card.getByTestId('subagent-group-body').first();
  await expect(body.getByText('Shard reviewed: nothing drifted.')).toBeVisible();
  await expect(body.getByRole('link', { name: 'Open README.md in editor' })).toBeVisible();
  await expect(body.getByText(/No child entries captured/i)).toHaveCount(0);
});
