// A parked background agent the CLI cannot wake (claude-wire.md §E6b,
// dropped wake). The agent reports and parks on a command it started;
// when the command finishes, the CLI fails to resume the agent (its
// worktree was removed, captured 2026-10-14 on 2.1.284) and drops the
// command's notification. The only record is the session file's
// queue-operation removal, which --session-mirror streams.
//
//   dropped - the record ends the agent: the tray empties, the parked
//             card stays as it was, and the agent's last card says Claude
//             could not resume it. The launch settles.
//   stopped - with no record, the agent stays parked. Its Stop, on the
//             row or Stop All, gets the CLI's bare success reply (the
//             CLI has no such task to kill, as captured), and that reply
//             ends it as stopped.
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
  shellBackgroundAckLine,
  startMock,
  taskNotificationLine,
  taskStartedLine,
  taskUpdatedLine,
  textLines,
  toolUseLine,
  wakeDroppedMirrorLine,
  waitForGate,
} from './agent-visibility-helpers.js';
import type { HarnessApp } from '../src/harness.js';
import type { Page } from '@playwright/test';

const REPORT = 'Started the gate; waiting on it before I confirm.';
const SHELL_DONE = 'Background command "sleep 60; echo LONG" completed (exit code 0)';
const AGENT = { task_id: 'task-bg', task_type: 'local_agent', description: 'gate watcher' };
const SHELL = { task_id: 'task-shell', task_type: 'local_bash', description: 'sleep 60; echo LONG' };

// The launch and the park, then the shell's report, with the CLI's drop
// record when `dropped`.
function parkedAgentScenario(name: string, dropped: boolean): unknown {
  const shellDone = [
    taskUpdatedLine('task-shell', { status: 'completed', end_time: 1791995110875 }),
    taskNotificationLine('task-shell', 'tu-shell', SHELL_DONE, { uuid: 'shell-1' }),
    backgroundTasksChangedLine([]),
  ];
  return claudeScenario(name, [
    emit([
      ...textLines('msg-lead', 'Launching the gate watcher.'),
      toolUseLine('msg-launch', 'tu-bg', 'Agent', {
        description: 'gate watcher',
        subagent_type: 'general-purpose',
        prompt: 'Run the gate in the background and report.',
      }),
      taskStartedLine('task-bg', 'tu-bg', 'gate watcher'),
      asyncAgentAckLine('tu-bg', 'task-bg', 'gate watcher'),
      backgroundTasksChangedLine([AGENT]),
      RESULT_LINE,
    ]),
    { waitSignal: { name: 'park' } },
    emit([
      toolUseLine('msg-s1', 'tu-shell', 'Bash', { command: 'sleep 60; echo LONG', run_in_background: true }, 'tu-bg'),
      taskStartedLine('task-shell', 'tu-shell', 'sleep 60; echo LONG', { taskType: 'local_bash', ownedBySubagent: true }),
      shellBackgroundAckLine('tu-shell', 'task-shell', 'tu-bg'),
      backgroundTasksChangedLine([AGENT, SHELL]),
      ...textLines('msg-s2', REPORT, 'tu-bg'),
      taskUpdatedLine('task-bg', { status: 'completed', end_time: 1791995067453 }),
      taskNotificationLine('task-bg', 'tu-bg', REPORT, {
        outputFile: '${CWD}/gate-output.jsonl',
        usage: { total_tokens: 12000, tool_uses: 1, duration_ms: 2100 },
        uuid: 'park-1',
      }),
      backgroundTasksChangedLine([SHELL]),
    ]),
    { waitSignal: { name: 'shell-done' } },
    emit(dropped ? [...shellDone, wakeDroppedMirrorLine('task-shell', 'tu-shell', SHELL_DONE)] : shellDone),
    { waitSignal: { name: 'never' } },
  ]);
}

async function driveToParked(harness: HarnessApp, page: Page, slug: string, title: string) {
  const threadId = await seedAgentThread(harness, slug, title);
  await harness.open(page);
  await page.getByText(title).click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'watch the gate', null);
  await harness.waitForEvent('provider:turn_completed');
  await page.getByTestId('activity-rail-background-toggle').click();
  await waitForGate(harness, 'park');
  await advance(harness, mockId, 'park');
  const agentRow = page.locator('[data-testid="background-task-tray-row"][data-row-id="tu-bg"]');
  await expect(agentRow.getByTestId('background-task-tray-row-status')).toHaveAttribute('data-run-state', 'parked');
  await expect(agentRow.getByTestId('background-task-tray-row-activity')).toHaveText('Waiting on 1 background command');
  await waitForGate(harness, 'shell-done');
  await advance(harness, mockId, 'shell-done');
  return { threadId, agentRow };
}

function mainTimeline(page: Page) {
  return page.locator(
    '[data-testid="message-timeline-scroll"]:not([data-testid="agent-pane-timeline"] [data-testid="message-timeline-scroll"])',
  );
}

async function agentStops(harness: HarnessApp, threadId: string) {
  const items = await listItems(harness, threadId);
  return items
    .filter((i) => i.completionOf === 'tu-bg')
    .map((i) => (i.status === 'parked' ? 'parked' : `${i.status}:${String(itemMeta(i).status_source)}`));
}

test('a dropped wake ends the parked agent: the tray empties and its card says Claude could not resume it', async ({
  harness,
  page,
}) => {
  await harness.rpc('HarnessSetScenario', { scenario: parkedAgentScenario('dropped-wake', true) });
  const { threadId } = await driveToParked(harness, page, 'dropped-wake-app', 'Dropped wake');

  await expect
    .poll(() => agentStops(harness, threadId))
    .toEqual(['parked', 'errored:wake_dropped']);
  await expect(page.getByTestId('background-task-tray-row')).toHaveCount(0);

  const timeline = mainTimeline(page);
  const cards = timeline.getByTestId('subagent-group');
  await expect(cards).toHaveCount(2);
  const parkedCard = cards.nth(0);
  await expect(parkedCard).toHaveAttribute('data-anchor-id', 'complete:tu-bg:parked:park-1');
  await expect(parkedCard.getByTestId('subagent-group-parked-status')).toHaveText('Reported, waiting on 1 background command');
  await expect(parkedCard.getByTestId('subagent-group-preview')).toHaveText(REPORT);
  const endCard = cards.nth(1);
  await expect(endCard).toHaveAttribute('data-anchor-id', 'complete:tu-bg');
  await expect(endCard.getByTestId('subagent-group-error')).toHaveText('Claude could not resume the agent');
  await expect(timeline.locator('[data-item-id="tu-bg"]').getByTestId('agent-row-status')).toHaveAttribute('data-state', 'settled');
});

for (const control of ['row', 'all'] as const) {
  test(`Stop (${control}) ends a parked agent the CLI no longer has, on its bare success reply`, async ({ harness, page }) => {
    await harness.rpc('HarnessSetScenario', { scenario: parkedAgentScenario(`stop-ended-agent-${control}`, false) });
    const { threadId, agentRow } = await driveToParked(
      harness,
      page,
      `stop-ended-agent-${control}-app`,
      `Stop ended agent ${control}`,
    );

    // The shell's report arrived and nothing woke the agent: it stays parked.
    await expect.poll(() => agentStops(harness, threadId)).toEqual(['parked']);
    await expect(agentRow.getByTestId('background-task-tray-row-status')).toHaveAttribute('data-run-state', 'parked');

    if (control === 'row') await agentRow.getByTestId('background-task-tray-row-stop').click();
    else await page.getByTestId('activity-rail-background-stop-all').click();

    await expect.poll(() => agentStops(harness, threadId)).toEqual(['parked', 'killed:stop_acked']);
    await expect(page.getByTestId('background-task-tray-row')).toHaveCount(0);
    const endCard = mainTimeline(page).getByTestId('subagent-group').nth(1);
    await expect(endCard).toHaveAttribute('data-anchor-id', 'complete:tu-bg');
    await expect(endCard.getByTestId('subagent-group-error')).toHaveText('Agent stopped');
  });
}
