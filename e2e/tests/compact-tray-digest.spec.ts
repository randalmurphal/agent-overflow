// The background tray digest at phone width: the expanded clip fits the
// viewport with no horizontal overflow, tapping the row header toggles it,
// its rows scroll inside the clip, and the open button opens the pane.
import { test, expect } from './fixtures.js';
import {
  advance, claudeScenario, emit, seedAgentThread, startMock, taskNotificationLine,
  taskUpdatedLine, backgroundTasksChangedLine, waitForGate,
} from './agent-visibility-helpers.js';
import {
  AGENT, FIRST_BATCH, RESULT_LINE, childTools, launchLines, openTrayDigest, swipe,
} from './background-tray-digest-helpers.js';

test('the tray digest fits a phone and stays touch-scrollable', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('compact-tray-digest', [
      emit([...launchLines(), ...childTools(0, FIRST_BATCH), RESULT_LINE]),
      { waitSignal: { name: 'settle' } },
      emit([
        taskUpdatedLine('task-agent', { status: 'completed', end_time: 1787415964725 }),
        taskNotificationLine('task-agent', AGENT, 'Read everything.'),
        backgroundTasksChangedLine([]),
      ]),
    ]),
  });
  const threadId = await seedAgentThread(harness, 'compact-tray-digest-app', 'Compact digest');
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'Compact digest' }).click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'read it all', null);
  await harness.waitForEvent('provider:turn_completed');

  for (const width of [412, 360, 320]) {
    await page.setViewportSize({ width, height: 915 });
    const { row, digest, clip, child } = await openTrayDigest(page);
    await expect(child(FIRST_BATCH - 1)).toBeVisible();
    const fits = await page.evaluate(() => {
      const rect = (id: string) => document.querySelector(`[data-testid="${id}"]`)!.getBoundingClientRect();
      const row = rect('background-task-tray-row');
      const clip = rect('subagent-group-scroll');
      const open = rect('background-task-tray-row-open');
      return document.documentElement.scrollWidth <= window.innerWidth
        && clip.left >= row.left && clip.right <= row.right + 1
        && clip.bottom <= window.innerHeight
        && open.width > 0 && open.right <= row.right + 1;
    });
    expect(fits, `width ${width}`).toBe(true);
    // A touch drag inside the clip scrolls the clip, not the page or the
    // tray list, and releases its tail follow so older rows mount.
    const box = (await clip.boundingBox())!;
    const list = page.getByTestId('activity-rail-background-body').locator('ul');
    const listBefore = await list.evaluate((el) => el.scrollTop);
    const clipBefore = await clip.evaluate((el) => el.scrollTop);
    expect(clipBefore).toBeGreaterThan(0);
    await swipe(page, box.x + box.width / 2, box.y + 20, box.y + box.height - 20);
    await expect.poll(() => clip.evaluate((el) => el.scrollTop)).toBeLessThan(clipBefore);
    expect(await list.evaluate((el) => el.scrollTop)).toBe(listBefore);
    expect(await page.evaluate(() => window.scrollY)).toBe(0);
    // The touch escape released the tail follow, so a jump to the top
    // holds and mounts the oldest rows on demand.
    await clip.evaluate((el) => el.scrollTo({ top: 0 }));
    await expect(child(0)).toBeVisible();
    // Tap the header again to collapse.
    await row.getByTestId('agent-row-toggle').click();
    await expect(digest).toHaveCount(0);
    await page.getByTestId('activity-rail-background-toggle').click();
  }

  // The open button opens the pane at phone width.
  await page.getByTestId('activity-rail-background-toggle').click();
  const row = page.getByTestId('background-task-tray-row');
  await row.getByTestId('background-task-tray-row-open').click();
  const pane = page.getByTestId('companion-pane-agent-body');
  await expect(pane.getByTestId('agent-pane-breadcrumb-current')).toContainText('Reader');
  await waitForGate(harness, 'settle');
  await advance(harness, mockId, 'settle');
  await expect(row).toHaveCount(0);
});
