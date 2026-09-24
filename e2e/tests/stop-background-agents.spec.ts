// Stop with a live background agent (claude-wire.md §Background task
// ownership): a Claude interrupt kills every async agent the session
// holds, so the backend refuses a person's Stop until they confirm
// (internal/app/app_background_kill.go) and the app asks with the agents
// named (components/composer/BackgroundKillConfirmationHost.svelte).
//
//   "Keep them running"  - nothing stops: the turn keeps going, the Stop
//                          button stays, the agent stays in the tray.
//   "Stop both"          - the confirmed interrupt goes out and the turn
//                          ends. The sent message stays: the un-send is
//                          not offered while background work runs.
//
// The mock provider acknowledges the interrupt and ends the turn; it does
// not emit the CLI's killed frames for the agent, so the agent's own fate
// is the wire doc's claim, not this spec's.
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  asyncAgentAckLine,
  backgroundTasksChangedLine,
  claudeScenario,
  emit,
  listItems,
  seedAgentThread,
  startMock,
  taskStartedLine,
  textLines,
  toolUseLine,
  waitForGate,
} from './agent-visibility-helpers.js';

test('Stop asks before killing a background agent; keep leaves the turn running, confirm stops it', async ({
  harness,
  page,
}) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('stop-with-agent', [
      emit([
        ...textLines('msg-lead', 'Launching the gate watcher, then thinking.'),
        toolUseLine('msg-launch', 'tu-bg', 'Agent', {
          description: 'gate watcher',
          subagent_type: 'gate-watcher',
          prompt: 'Watch the gate.',
        }),
        taskStartedLine('task-bg', 'tu-bg', 'gate watcher'),
        asyncAgentAckLine('tu-bg', 'task-bg', 'gate watcher'),
        backgroundTasksChangedLine([
          { task_id: 'task-bg', task_type: 'local_agent', description: 'gate watcher' },
        ]),
      ]),
      // The turn stays open here; only the confirmed interrupt ends it.
      { waitSignal: { name: 'answer' } },
      emit([...textLines('msg-answer', 'Never reached.'), RESULT_LINE]),
    ]),
  });

  const threadId = await seedAgentThread(harness, 'stop-app', 'Stop with agent');
  await harness.open(page);
  await page.getByText('Stop with agent').click();
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'watch the gate and wait', null);
  await waitForGate(harness, 'answer');

  const stop = page.getByRole('button', { name: 'Interrupt current turn', exact: true });
  await expect(stop).toBeVisible();
  await expect(page.getByTestId('activity-rail-background-count')).toHaveText('1');
  const dialog = page.getByTestId('background-kill-dialog');

  // --- Keep them running ------------------------------------------
  await stop.click();
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText('stops the background agent still working');
  const rows = page.getByTestId('background-kill-agent');
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText('gate watcher');
  await expect(rows.first()).toHaveAttribute('data-run-state', 'running');
  await expect(page.getByTestId('background-kill-confirm')).toHaveText('Stop both');
  // The refusal had no effect: the turn is still running.
  await expect(stop).toBeVisible();
  await page.getByTestId('background-kill-cancel').click();
  await expect(dialog).toHaveCount(0);
  await expect(stop).toBeVisible();
  await expect(page.getByTestId('activity-rail-background-count')).toHaveText('1');
  await expect(page.getByText('watch the gate and wait', { exact: true })).toBeVisible();

  // --- Stop both ---------------------------------------------------
  const completed = harness.waitForEvent('provider:turn_completed');
  await stop.click();
  await expect(dialog).toBeVisible();
  await page.getByTestId('background-kill-confirm').click();
  await completed;
  await expect(dialog).toHaveCount(0);
  await expect(stop).toHaveCount(0);
  // The message stays: a Stop that kills agents never un-sends.
  await expect(page.getByText('watch the gate and wait', { exact: true })).toBeVisible();
  await expect
    .poll(async () => {
      const items = await listItems(harness, threadId);
      return items.filter((i) => i.kind === 'user_text' && i.summary === 'watch the gate and wait').length;
    })
    .toBe(1);
});
