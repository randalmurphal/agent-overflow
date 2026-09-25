// Agent tray headers at phone widths: metrics and activity fit below the
// name, tapping the name expands the digest and the open button, present
// at every width, opens the live agent pane. A background launch row in the
// timeline keeps its geometry when the launch settles: the indicator's box
// stays with its dots hidden, so the open-in-pane door does not move.
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE, advance, asyncAgentAckLine, backgroundTasksChangedLine,
  claudeScenario, emit, seedAgentThread, startMock, taskNotificationLine,
  taskProgressLine, taskStartedLine, taskUpdatedLine, toolUseLine, waitForGate,
} from './agent-visibility-helpers.js';

const activity = 'Searching for Reuters March coverage and checking the original sources';

test('agent metrics fit the phone, the name expands the digest and the open button opens its pane', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('compact-agent-summary', [
      emit([
        toolUseLine('msg-research', 'tu-research', 'Agent', {
          description: activity, subagent_type: 'research-original-financial-sources',
        }),
        taskStartedLine('task-research', 'tu-research', activity),
        asyncAgentAckLine('tu-research', 'task-research', activity),
        backgroundTasksChangedLine([{ task_id: 'task-research', task_type: 'local_agent', description: activity }]),
        RESULT_LINE,
      ]),
      { waitSignal: { name: 'tick' } },
      emit([taskProgressLine('task-research', 'tu-research', activity,
        { total_tokens: 117500, tool_uses: 66, duration_ms: 320000 }, 'Grep')]),
      { waitSignal: { name: 'settle' } },
      emit([
        taskUpdatedLine('task-research', { status: 'completed', end_time: 1787415964725 }),
        taskNotificationLine('task-research', 'tu-research', 'Research complete.'),
        backgroundTasksChangedLine([]),
      ]),
    ]),
  });
  const threadId = await seedAgentThread(harness, 'compact-agent-app', 'Agent layout');
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'Agent layout' }).click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'research the sources', null);
  await harness.waitForEvent('provider:turn_completed');
  await page.getByTestId('activity-rail-background-toggle').click();
  await waitForGate(harness, 'tick');
  await advance(harness, mockId, 'tick');
  const row = page.getByTestId('background-task-tray-row');
  await expect(row.getByTestId('background-task-tray-row-tokens')).toHaveText('117.5k tokens');
  await expect(row.getByTestId('background-task-tray-row-activity')).toHaveText(activity);
  for (const width of [412, 360, 320]) {
    await page.setViewportSize({ width, height: 915 });
    await expect.poll(() => row.evaluate((element) => {
      const box = (id: string) => element.querySelector(`[data-testid="${id}"]`)!.getBoundingClientRect();
      const outer = element.getBoundingClientRect();
      const name = box('agent-row-toggle-body-slot');
      const open = box('background-task-tray-row-open');
      const stop = box('background-task-tray-row-stop');
      const tools = box('background-task-tray-row-tools');
      const tokens = box('background-task-tray-row-tokens');
      const activity = box('background-task-tray-row-activity');
      return [name, open, stop, tools, tokens, activity].every(rect => rect.width > 0 && rect.left >= outer.left && rect.right <= outer.right + 1)
        && name.right <= open.left && open.right <= stop.left && tools.top >= name.bottom
        && activity.top >= tokens.bottom && Math.abs(activity.left - name.left) < 1;
    })).toBe(true);
  }
  await row.getByTestId('agent-row-toggle').click();
  await expect(row.getByTestId('background-task-tray-row-digest')).toBeVisible();
  await expect(page.getByTestId('companion-pane-agent-body')).toHaveCount(0);
  await row.getByTestId('background-task-tray-row-open').click();
  const pane = page.getByTestId('companion-pane-agent-body');
  await expect(pane.getByTestId('agent-pane-breadcrumb-current')).toContainText('Research Original Financial Sources');
  await expect(pane.getByTestId('agent-pane-working')).toBeVisible();
  await waitForGate(harness, 'settle');
  await advance(harness, mockId, 'settle');
  await expect(pane.getByTestId('agent-pane-working')).toHaveCount(0);
});

test('a background launch row keeps its open-in-pane door in place when the launch settles', async ({ harness, page }) => {
  const description = 'Check the release notes against the tags';
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('compact-launch-settle', [
      emit([
        toolUseLine('msg-notes', 'tu-notes', 'Agent', { description, subagent_type: 'general-purpose' }),
        taskStartedLine('task-notes', 'tu-notes', description),
        asyncAgentAckLine('tu-notes', 'task-notes', description),
        backgroundTasksChangedLine([{ task_id: 'task-notes', task_type: 'local_agent', description }]),
        RESULT_LINE,
      ]),
      { waitSignal: { name: 'settle' } },
      emit([
        taskUpdatedLine('task-notes', { status: 'completed', end_time: 1787415964725 }),
        taskNotificationLine('task-notes', 'tu-notes', 'Release notes match the tags.'),
        backgroundTasksChangedLine([]),
      ]),
    ]),
  });
  const threadId = await seedAgentThread(harness, 'compact-launch-settle-app', 'Launch settle');
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'Launch settle' }).click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'check the notes', null);
  await harness.waitForEvent('provider:turn_completed');

  const timeline = page.getByTestId('message-timeline-scroll');
  const launchRow = timeline.locator('[data-item-id="tu-notes"]');
  const status = launchRow.getByTestId('agent-row-status');
  const door = launchRow.getByTestId('agent-row-open-pane');
  const slot = launchRow.getByTestId('agent-row-status-slot');
  await expect(status).toHaveAttribute('data-state', 'backgrounded');
  await expect(door).toBeVisible();
  const doorBefore = (await door.boundingBox())!;
  const slotBefore = (await slot.boundingBox())!;

  await waitForGate(harness, 'settle');
  await advance(harness, mockId, 'settle');
  await expect(timeline.getByTestId('subagent-group')).toHaveCount(1);
  // The settled launch is pushed as stored: the dots hide in their box.
  await expect(status).toHaveAttribute('data-state', 'settled');
  await expect(door).toBeVisible();
  expect((await door.boundingBox())!.x).toBe(doorBefore.x);
  expect((await slot.boundingBox())!.width).toBe(slotBefore.width);
});
