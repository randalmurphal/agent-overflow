// The background tray's wake rows under the compact layout
// (docs/specs/standing-agents.md, "Activity rail"): a loop, an interval
// watch, a long-running watch and a reminder each fit a phone-width row
// with touch-sized Wake now and Cancel, the label whole on its first line
// and when it fires and the actions on its second; a watch expands in place to its
// command, directory and last stderr; and a tap on Wake now and Cancel acts
// as a click does on desktop.
import type { Locator } from '@playwright/test';

import { test, expect } from './fixtures.js';
import { awaitTurnCompleted, type ProviderName } from './thread-tools-helpers.js';
import { capture, dbWake, openTray, registerThrough, run, seedWakeAgent, trayRow, wakeInput } from './wake-rows-helpers.js';

/** The row stays inside the window and its actions are touch-sized. */
async function expectFits(row: Locator, viewportWidth: number): Promise<void> {
  const box = (await row.boundingBox())!;
  expect(box.x).toBeGreaterThanOrEqual(0);
  expect(box.x + box.width).toBeLessThanOrEqual(viewportWidth);
  expect(await row.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
  for (const id of ['wake-tray-row-wake-now', 'wake-tray-row-cancel']) {
    const button = (await row.getByTestId(id).boundingBox())!;
    expect(button.height, id).toBeGreaterThanOrEqual(36);
    expect(button.x + button.width, id).toBeLessThanOrEqual(viewportWidth);
  }
}

/** The label reads whole across the row, above when it fires and the actions. */
async function expectWholeLabel(row: Locator, label: string): Promise<void> {
  const text = row.getByTestId('wake-tray-row-label');
  await expect(text).toHaveText(label);
  expect(await text.evaluate((el) => el.scrollHeight - el.clientHeight), 'label cut below').toBeLessThanOrEqual(1);
  expect(await text.evaluate((el) => el.scrollWidth - el.clientWidth), 'label cut at the side').toBeLessThanOrEqual(1);
  const labelBox = (await text.boundingBox())!;
  const controls = (await row.getByTestId('wake-tray-row-controls').boundingBox())!;
  expect(controls.y, 'controls below the label').toBeGreaterThanOrEqual(labelBox.y + labelBox.height - 1);
  const when = row.getByTestId('wake-tray-row-when');
  const cancel = (await row.getByTestId('wake-tray-row-cancel').boundingBox())!;
  if (await when.count()) {
    const whenBox = (await when.boundingBox())!;
    expect(whenBox.x + whenBox.width, 'when left of the actions').toBeLessThan(cancel.x);
  }
  // The actions end at the row's right edge, as the label does.
  expect(controls.x + controls.width - (cancel.x + cancel.width)).toBeLessThanOrEqual(8);
}

// Forty characters: past what fits beside a fire time and two buttons.
const REMINDER = 'review the deploy notes before releasing';

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: wake rows fit a phone, expand in place and answer taps`, async ({ harness, page }) => {
    test.setTimeout(180_000);
    const agent = await seedWakeAgent(harness, provider, 'compact-wake-tray', 'Phone agent');
    await registerThrough(harness, provider, agent, `compact-wake-tray-${provider}`, [
      { tool: 'thread_loop', args: { label: 'standup', note: 'post the standup summary', every_seconds: 3600 } },
      { tool: 'thread_watch', args: { label: 'ci', command: run('ci.sh'), every_seconds: 3600 } },
      { tool: 'thread_watch', args: { label: 'events', command: 'exec sleep 3600', long_running: true } },
      { tool: 'thread_remind', args: { after_seconds: 3600, note: REMINDER } },
    ]);
    await harness.open(page);
    await openTray(page, 'Phone agent');
    const width = page.viewportSize()!.width;
    const standup = trayRow(page, 'loop', 'standup');
    const ci = trayRow(page, 'watch', 'ci');
    const events = trayRow(page, 'watch', 'events');
    const review = trayRow(page, 'reminder', REMINDER);
    for (const row of [standup, ci, events, review]) {
      await expect(row).toBeVisible();
      await expectFits(row, width);
    }
    await expectWholeLabel(review, REMINDER);
    await expectWholeLabel(standup, 'standup');
    await review.scrollIntoViewIfNeeded();
    await capture(page, `${provider}-compact-tray-reminder.png`, review);
    await expect(events.getByTestId('wake-tray-row-when')).toHaveText('running');
    const body = page.getByTestId('activity-rail-background-body').filter({ visible: true });
    await capture(page, `${provider}-compact-tray-collapsed.png`, body);

    // A tap on Wake now runs the watch; its row expands in place.
    const ciWake = wakeInput(harness, agent.cwd, 'Watch "ci" reported.');
    await ci.getByTestId('wake-tray-row-wake-now').tap();
    await ciWake;
    await awaitTurnCompleted(harness, agent.thread);
    await expect.poll(() => dbWake(agent.dataDir, agent.thread, 'ci')?.last_stderr).toContain('ci: checked pipeline 12');
    await ci.getByTestId('wake-tray-row-toggle').tap();
    await expect(ci.getByTestId('wake-tray-row-stderr')).toHaveText('ci: checked pipeline 12 at the default branch');
    await expect(ci.getByTestId('wake-tray-row-command')).toHaveText(run('ci.sh'));
    await expect(ci.getByTestId('wake-tray-row-cwd')).toHaveText(agent.cwd);
    await expectFits(ci, width);
    const detail = (await ci.getByTestId('wake-tray-row-detail').boundingBox())!;
    expect(detail.x + detail.width).toBeLessThanOrEqual(width);
    await capture(page, `${provider}-compact-tray-watch-expanded.png`, ci);

    // A tap on Cancel removes the row.
    await standup.getByTestId('wake-tray-row-cancel').tap();
    await expect(standup).toHaveCount(0);
    expect(dbWake(agent.dataDir, agent.thread, 'standup')).toBeUndefined();
  });
}
