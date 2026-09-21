// The background tray digest under load: a running background agent with
// many tool rows expands into a virtualized clip that follows its tail,
// keeps growing live, scrolls on its own without moving the tray list or
// the thread timeline, survives collapse and re-expand, and keeps its rows
// through the two paths that remove rows from pane memory: settled-child
// eviction and the timeline window's prune.
import { test, expect } from './fixtures.js';
import { waitForScrollSettle } from '../src/scroll.js';
import {
  advance, claudeScenario, emit, seedAgentThread, startMock, taskNotificationLine,
  taskUpdatedLine, backgroundTasksChangedLine, waitForGate,
} from './agent-visibility-helpers.js';
import {
  AGENT, FIRST_BATCH, RESULT_LINE, SECOND_BATCH, childResult, childTools, childUse,
  launchLines, mainProseFiller, openTrayDigest, twoTurnScenario,
} from './background-tray-digest-helpers.js';

test('a many-row agent digest virtualizes, follows its tail and scrolls independently', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('tray-digest-load', [
      emit([...launchLines(), ...childTools(0, FIRST_BATCH), RESULT_LINE]),
      { waitSignal: { name: 'more' } },
      emit(childTools(FIRST_BATCH, SECOND_BATCH)),
      { waitSignal: { name: 'settle' } },
      emit([
        taskUpdatedLine('task-agent', { status: 'completed', end_time: 1787415964725 }),
        taskNotificationLine('task-agent', AGENT, 'Read everything.'),
        backgroundTasksChangedLine([]),
      ]),
    ]),
  });
  const threadId = await seedAgentThread(harness, 'tray-digest-app', 'Digest load');
  await harness.open(page);
  await page.getByText('Digest load').click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'read it all', null);
  await harness.waitForEvent('provider:turn_completed');

  const { row, digest, clip, child } = await openTrayDigest(page);

  // Opens at the newest activity, not the top.
  await expect(child(FIRST_BATCH - 1)).toBeVisible();
  await expect(child(0)).toHaveCount(0);
  // Virtualized: only a window of the 120 rows is mounted.
  const mounted = await digest.locator('[data-item-id^="tu-child-"]').count();
  expect(mounted).toBeGreaterThan(0);
  expect(mounted).toBeLessThan(FIRST_BATCH / 2);
  const clipBox = (await clip.boundingBox())!;
  const rowBox = (await row.boundingBox())!;
  expect(clipBox.height).toBeLessThanOrEqual(rowBox.height);

  // Live growth keeps following the tail.
  await waitForGate(harness, 'more');
  await advance(harness, mockId, 'more');
  await expect(child(FIRST_BATCH + SECOND_BATCH - 1)).toBeVisible();

  // Wheel inside the clip scrolls the clip only. The tray list and the
  // thread timeline hold their positions.
  const list = page.getByTestId('activity-rail-background-body').locator('ul');
  const timeline = page.getByTestId('message-timeline-scroll');
  const atBottom = (el: Element) => el.scrollHeight - el.scrollTop - el.clientHeight <= 1;
  // The tray's growth resizes the composer; wait for the timeline to
  // settle back on its tail before measuring.
  await expect.poll(() => timeline.evaluate(atBottom)).toBe(true);
  const before = {
    clip: await clip.evaluate((el) => el.scrollTop),
    list: await list.evaluate((el) => el.scrollTop),
  };
  expect(before.clip).toBeGreaterThan(0);
  await clip.hover();
  await page.mouse.wheel(0, -400);
  await expect.poll(() => clip.evaluate((el) => el.scrollTop)).toBeLessThan(before.clip);
  expect(await list.evaluate((el) => el.scrollTop)).toBe(before.list);
  expect(await timeline.evaluate(atBottom)).toBe(true);
  // Scrolled up, older rows mount on demand.
  await expect(async () => {
    await page.mouse.wheel(0, -100000);
    await expect(child(0)).toBeVisible({ timeout: 500 });
  }).toPass();

  // Collapse drops the body; re-expand reopens at the tail.
  await row.getByTestId('agent-row-toggle').click();
  await expect(digest).toHaveCount(0);
  await row.getByTestId('agent-row-toggle').click();
  await expect(child(FIRST_BATCH + SECOND_BATCH - 1)).toBeVisible();

  // Settle: the tray empties and the card takes over on the timeline.
  await waitForGate(harness, 'settle');
  await advance(harness, mockId, 'settle');
  await expect(row).toHaveCount(0);
  await expect(timeline.getByTestId('subagent-group')).toHaveCount(1);
});

// The digest owns its window even when its launch is outside the main
// window or settled children are evicted from the main pane.
const FILLER = 500;

test('an open digest loads an out-of-window launch and keeps its rows through settled-child eviction', async ({ harness, page }) => {
  const LATE = FIRST_BATCH;
  let holdRunRefresh = false;
  const heldRefreshes: (() => void)[] = [];
  await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
    const server = socket.connectToServer();
    socket.onMessage(message => {
      const frame = JSON.parse(String(message));
      if (holdRunRefresh && frame.type === 'rpc' && frame.methodId === 1602023272 && frame.params[1]?.limit === 0) {
        heldRefreshes.push(() => server.send(message));
      } else server.send(message);
    });
    server.onMessage(message => socket.send(message));
  });
  await harness.rpc('HarnessSetScenario', {
    scenario: twoTurnScenario(
      'tray-digest-retention',
      [{ emit: { lines: [...launchLines(), ...childTools(0, FIRST_BATCH), ...mainProseFiller(0, FILLER), RESULT_LINE], delayBetweenMs: 0 } }],
      [
        { emit: { lines: [childUse(LATE)] } },
        { waitSignal: { name: 'settle-late' } },
        { emit: { lines: [childResult(LATE), RESULT_LINE] } },
      ],
    ),
  });
  const threadId = await seedAgentThread(harness, 'tray-digest-retention-app', 'Digest retention');
  await harness.open(page);
  await page.getByText('Digest retention').click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'read it all', null);
  await harness.waitForEvent('provider:turn_completed');

  // Reopen from persistence: the thread comes back on its tail page and
  // the prose filler keeps the viewport full, so the launch stays out of
  // the loaded window. The tray reads the live task list from the
  // backend, so its row is there regardless.
  await page.reload();
  const timeline = page.getByTestId('message-timeline-scroll');
  await expect(timeline.getByText(`Note ${FILLER - 1}.`)).toBeVisible();
  const atBottom = (el: Element) => el.scrollHeight - el.scrollTop - el.clientHeight <= 1;
  await expect.poll(() => timeline.evaluate(atBottom)).toBe(true);
  await timeline.hover();
  await expect(async () => {
    await page.mouse.wheel(0, -10000);
    await expect(timeline.getByTestId('load-older-messages')).toBeVisible({ timeout: 500 });
  }).toPass();
  await expect(timeline.getByText('Launching the reader.')).toHaveCount(0);
  await expect(timeline.locator(`[data-item-id="${AGENT}"]`)).toHaveCount(0);
  await expect(async () => {
    await page.mouse.wheel(0, 10000);
    await waitForScrollSettle(timeline);
    await expect.poll(() => timeline.evaluate(atBottom), { timeout: 500 }).toBe(true);
  }).toPass();

  // The digest loads its own scope without moving the main reader.
  const { digest, child } = await openTrayDigest(page);
  await expect(child(FIRST_BATCH - 1)).toBeVisible();
  await expect(digest.getByTestId('subagent-group-loading')).toHaveCount(0);
  await expect.poll(() => timeline.evaluate(atBottom)).toBe(true);

  // Turn two. A late child lands running, then settles: the settled row
  // stays in the independent digest window.
  holdRunRefresh = true;
  await harness.rpc('SendMessage', threadId, 'keep going', null);
  await expect(child(LATE)).toBeVisible();
  await expect(child(LATE).getByTestId('tool-call-card-status')).toHaveAttribute('data-state', 'running');
  // The mounted element proves settling did not remount the row.
  await child(LATE).evaluate((el) => { el.setAttribute('data-e2e-mark', 'kept'); });
  await waitForGate(harness, 'settle-late');
  await advance(harness, mockId, 'settle-late');
  await harness.waitForEvent('provider:turn_completed');
  await expect(child(LATE).getByTestId('tool-call-card-status')).toHaveCount(0);
  await expect(child(LATE)).toHaveAttribute('data-e2e-mark', 'kept');
  await expect(digest.getByTestId('subagent-group-loading')).toHaveCount(0);
  await expect.poll(() => heldRefreshes.length).toBeGreaterThan(0);
  await digest.getByTestId('subagent-group-scroll').hover();
  // The reader's boundary request must survive the in-flight count refresh.
  await expect(async () => {
    await page.mouse.wheel(0, -100000);
    await expect(digest.getByRole('button', { name: 'Loading…', exact: true })).toBeVisible({ timeout: 500 });
  }).toPass();
  holdRunRefresh = false;
  for (const release of heldRefreshes.splice(0)) release();
  await expect(async () => {
    await page.mouse.wheel(0, -100000);
    await expect(child(0)).toBeVisible({ timeout: 500 });
  }).toPass();
});
