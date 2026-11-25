// The Issues pane's Time screen (docs/specs/issues.md, "Time"), shared by
// the desktop and compact projects, through the shipped UI, the real
// backend and the fake Jira. Covered: the pane switches to Time and shows
// the person's week as Jira holds it, By day and By ticket; time logged
// from an empty cell of By ticket, changed from an entry's hours and
// deleted from the entry's menu (a long press on compact) after the
// form asks, each reaching Jira and showing in the week; a change of an
// entry that someone changed in Jira since the week was read is refused
// as stale, sends nothing, and the week shows what Jira holds; a week
// whose read Jira refuses says so and shows its days as not read yet
// until Refresh reads them; Time opens again on the next pane. A Linear
// board says Linear has no time tracking. A week shown from a paired
// computer that goes off says once that the computer is not connected,
// and shows again once it is back.

import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import type { Locator, Page } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test } from './fixtures.js';
import { pairOwnDevice } from './headless-pairing-helpers.js';
import { startTogether } from './launch-helpers.js';
import { longPress } from './touch-helpers.js';
import {
  JIRA_SITE,
  connectFakeJiraBoard,
  connectFakeLinearBoard,
  editJiraWorklog,
  logJiraWork,
  openIssuesPane,
  press,
  seedFakeJira,
  trackerRequests,
} from './tracker-helpers.js';

export interface TimeSurface {
  compact: boolean;
}

const WORKLOG_ADD = 'POST rest/api/3/issue/{issue}/worklog';
const WORKLOG_EDIT = 'PUT rest/api/3/issue/{issue}/worklog/{id}';
const WORKLOG_DELETE = 'DELETE rest/api/3/issue/{issue}/worklog/{id}';
const STALE = 'This entry changed on the tracker since it was read. The week was read again; look at it before you change it.';

/** A day as YYYY-MM-DD in this machine's zone, which the page shares. */
function localDay(d: Date): string {
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

/** The Monday of this week and the day `n` days after it. */
function weekDay(n: number): string {
  const now = new Date();
  const monday = new Date(now.getFullYear(), now.getMonth(), now.getDate() - ((now.getDay() + 6) % 7));
  return localDay(new Date(monday.getFullYear(), monday.getMonth(), monday.getDate() + n));
}

/** Switches the pane to Time: its tab, or the folded picker in a narrow pane. */
async function switchToTime(page: Page, pane: Locator, compact: boolean): Promise<Locator> {
  const picker = pane.getByTestId('issues-tabs-picker');
  if (await picker.isVisible()) {
    await press(picker, compact);
    await press(page.getByTestId('issues-tab-option').filter({ hasText: 'Time' }), compact);
  } else {
    await press(pane.locator('[data-testid="issues-tab"][data-screen="time"]'), compact);
  }
  const screen = pane.getByTestId('time-screen');
  await expect(screen).toBeVisible();
  return screen;
}

/** The day `day` of By day; on compact, picked first from the day strip. */
async function byDay(time: Locator, day: string, compact: boolean): Promise<Locator> {
  if (compact) await press(time.locator(`[data-testid="time-day-tab"][data-day="${day}"]`), compact);
  const shown = time.locator(`[data-testid="time-day"][data-day="${day}"]`);
  await expect(shown).toBeVisible();
  return shown;
}

/** Replaces the text of an input as a person does: select it all and type. */
async function retype(input: Locator, text: string): Promise<void> {
  await input.press('ControlOrMeta+a');
  await input.pressSequentially(text);
}

export function registerIssuesTimeFlow({ compact }: TimeSurface): void {
  test('Time shows the week Jira holds, and time logged from a cell, changed and deleted reaches Jira', async ({ harness, page }) => {
    test.setTimeout(120_000);
    const board = await connectFakeJiraBoard(harness, compact ? 'compact-issues-time' : 'issues-time');
    const monday = weekDay(0);
    const tuesday = weekDay(1);
    // Two hours on WEB-2 on Monday, logged in Jira itself.
    const seeded = await logJiraWork(board.jiraURL, 'WEB-2', monday, 7200);
    const added = (await trackerRequests(harness, WORKLOG_ADD)).length;
    await harness.open(page);
    let pane = await openIssuesPane(page, compact);
    await expect(pane.getByTestId('issues-card').first()).toBeVisible({ timeout: 30_000 });

    const time = await switchToTime(page, pane, compact);
    // The board is not mounted on Time; Only mine and Search are the board's.
    await expect(pane.getByTestId('issues-card')).toHaveCount(0);
    await expect(pane.getByTestId('issues-search')).toHaveCount(0);
    const mondayShown = await byDay(time, monday, compact);
    const entry = mondayShown.locator(`[data-testid="time-entry"][data-worklog="${seeded}"]`);
    await expect(entry.getByTestId('time-entry-key')).toHaveText('WEB-2', { timeout: 30_000 });
    await expect(entry.getByTestId('time-entry-hours')).toHaveText('2h');
    await expect(entry).toContainText('Second task');
    await expect(time.getByTestId('time-week-total')).toContainText('2h');

    // By ticket: WEB-2's empty Tuesday cell logs on WEB-2 and Tuesday.
    await press(time.getByRole('radio', { name: 'By ticket' }), compact);
    const row = time.locator('[data-testid="time-ticket-row"][data-issue="10102"]');
    const cell = row.locator(`[data-testid="time-cell"][data-day="${tuesday}"]`);
    await expect(cell).toHaveAttribute('data-count', '0');
    await press(cell, compact);
    const form = page.getByTestId('worklog-form');
    await expect(form.getByTestId('worklog-form-ticket')).toContainText('WEB-2');
    await expect(form.getByTestId('worklog-form-day')).toHaveValue(tuesday);
    await form.getByTestId('worklog-form-time').fill('1h 30m');
    await press(form.getByTestId('worklog-form-save'), compact);
    await expect(form).toHaveCount(0, { timeout: 15_000 });
    await expect(cell).toHaveText('1h 30m');
    await expect(row.getByTestId('time-ticket-total')).toHaveText('3h 30m');
    const logged = (await trackerRequests(harness, WORKLOG_ADD)).slice(added);
    expect(logged.map((r) => [r.path, r.status])).toEqual([['/rest/api/3/issue/10102/worklog', 201]]);

    // By day: Tuesday's entry changed from its hours.
    await press(time.getByRole('radio', { name: 'By day' }), compact);
    const tuesdayShown = await byDay(time, tuesday, compact);
    const fresh = tuesdayShown.getByTestId('time-entry');
    await expect(fresh.getByTestId('time-entry-hours')).toHaveText('1h 30m');
    await press(fresh.getByTestId('time-entry-hours'), compact);
    await expect(form.getByTestId('worklog-form-time')).toHaveValue('1h 30m');
    await retype(form.getByTestId('worklog-form-time'), '45m');
    await press(form.getByTestId('worklog-form-save'), compact);
    await expect(form).toHaveCount(0, { timeout: 15_000 });
    await expect(fresh.getByTestId('time-entry-hours')).toHaveText('45m');
    expect((await trackerRequests(harness, WORKLOG_EDIT)).map((r) => [r.body, r.status])).toEqual([[expect.objectContaining({ timeSpentSeconds: 2700 }), 200]]);

    // Monday's entry deleted from its menu: the form asks first, and Keep
    // sends nothing.
    await byDay(time, monday, compact);
    const menu = page.getByRole('menu', { name: 'Entry actions' });
    const openMenu = async () => {
      if (compact) await longPress(page, entry.getByTestId('time-entry-key'), menu);
      else await entry.click({ button: 'right' });
      await expect(menu).toBeVisible();
    };
    await openMenu();
    await press(menu.getByTestId('time-menu-delete'), compact);
    await expect(form.getByTestId('worklog-form-confirm-delete')).toContainText('Delete this entry from the tracker?');
    await press(form.getByTestId('worklog-form-delete-keep'), compact);
    await expect(form.getByTestId('worklog-form-confirm-delete')).toHaveCount(0);
    await press(form.getByTestId('worklog-form-cancel'), compact);
    expect(await trackerRequests(harness, WORKLOG_DELETE)).toEqual([]);
    await openMenu();
    await press(menu.getByTestId('time-menu-delete'), compact);
    await press(form.getByTestId('worklog-form-delete-confirm'), compact);
    await expect(form).toHaveCount(0, { timeout: 15_000 });
    await expect(entry).toHaveCount(0);
    await expect(mondayShown.getByTestId('time-day-empty')).toHaveText('Nothing logged');
    expect((await trackerRequests(harness, WORKLOG_DELETE)).map((r) => [r.path, r.status])).toEqual([[`/rest/api/3/issue/10102/worklog/${seeded}`, 204]]);
    await expect(time.getByTestId('time-week-total')).toContainText('45m');

    // Changed in Jira since the week was read: the change is refused as
    // stale with nothing sent, and the week shows what Jira holds.
    await byDay(time, tuesday, compact);
    const tuesdayId = (await fresh.getAttribute('data-worklog'))!;
    await editJiraWorklog(board.jiraURL, 'WEB-2', tuesdayId, 3600);
    const edits = (await trackerRequests(harness, WORKLOG_EDIT)).length;
    await press(fresh.getByTestId('time-entry-hours'), compact);
    await retype(form.getByTestId('worklog-form-time'), '30m');
    await press(form.getByTestId('worklog-form-save'), compact);
    const refused = form.getByTestId('worklog-form-write-outcome');
    await expect(refused).toHaveAttribute('data-kind', 'refused', { timeout: 15_000 });
    await expect(refused).toContainText(STALE);
    expect((await trackerRequests(harness, WORKLOG_EDIT)).length, 'the stale change sent an edit').toBe(edits);
    await press(form.getByTestId('worklog-form-cancel'), compact);
    await expect(fresh.getByTestId('time-entry-hours')).toHaveText('1h', { timeout: 15_000 });

    // A week Jira refuses to read: the refusal shows over its days, which
    // are not read yet, until Refresh reads them.
    const lastMonday = weekDay(-7);
    await harness.rpc('HarnessTrackerFault', {
      origin: { tracker: 'jira', site: JIRA_SITE }, route: 'POST rest/api/3/search/jql', kind: 'refuse', times: 1,
      status: 400, body: { errorMessages: ['The search could not run.'] },
    });
    await press(time.getByTestId('time-previous'), compact);
    await expect(time.getByTestId('time-refresh-failure')).toContainText('The week is shown as this computer last read it.', { timeout: 30_000 });
    const refusedSearch = (await trackerRequests(harness, 'POST rest/api/3/search/jql')).filter((r) => r.status === 400);
    expect(refusedSearch.map((r) => (r.body as { jql: string }).jql)).toEqual([expect.stringContaining('worklogAuthor')]);
    const unread = await byDay(time, lastMonday, compact);
    await expect(unread.getByTestId('time-day-empty')).toHaveText('Not read yet');
    await expect(unread.getByTestId('time-day-uncounted')).toHaveAttribute('data-reason', 'unread');
    await press(pane.getByTestId('issues-refresh'), compact);
    await expect(unread.getByTestId('time-day-empty')).toHaveText('Nothing logged', { timeout: 30_000 });
    await expect(time.getByTestId('time-refresh-failure')).toHaveCount(0);
    await press(time.getByTestId('time-this-week'), compact);
    await expect((await byDay(time, tuesday, compact)).getByTestId('time-entry-hours')).toHaveText('1h');

    // The next pane opens on Time.
    await page.reload();
    pane = await openIssuesPane(page, compact);
    await expect(pane.getByTestId('time-screen')).toBeVisible({ timeout: 30_000 });
    await expect(pane.getByTestId('issues-card')).toHaveCount(0);
  });

  test('a Linear board says Linear has no time tracking', async ({ harness, page }) => {
    test.setTimeout(90_000);
    await connectFakeLinearBoard(harness, compact ? 'compact-issues-time-linear' : 'issues-time-linear');
    await harness.open(page);
    const pane = await openIssuesPane(page, compact);
    await expect(pane.getByTestId('issues-board-picker')).toHaveAccessibleName('Board: Web', { timeout: 30_000 });
    const time = await switchToTime(page, pane, compact);
    await expect(time.getByTestId('time-linear')).toHaveText("Linear has no time tracking, so this board's connection has no logged time.");
    await expect(time.getByTestId('time-toolbar')).toHaveCount(0);
  });

  test('a week shown from a paired computer that goes off says so once, and shows again once it is back', async ({ page }) => {
    test.setTimeout(240_000);
    const root = await mkdtemp(join(tmpdir(), 'ao-issues-time-'));
    const dataDir = join(root, 'state');
    let home: HarnessApp | undefined;
    let remote: HarnessApp | undefined;
    try {
      [home, remote] = await startTogether(launchHarness(), launchHarness({ dataDir }));
      const laptop = await pairOwnDevice(home, remote, 'Laptop');
      const board = await connectFakeJiraBoard(remote, 'issues-time-remote');
      const monday = weekDay(0);
      await logJiraWork(board.jiraURL, 'WEB-2', monday, 7200);
      await home.open(page);
      const pane = await openIssuesPane(page, compact);
      await expect(pane.getByTestId('issues-card').first()).toBeVisible({ timeout: 60_000 });
      const time = await switchToTime(page, pane, compact);
      const day = await byDay(time, monday, compact);
      await expect(day.getByTestId('time-entry-hours')).toHaveText('2h', { timeout: 30_000 });

      // Off: one sentence where the week was, and none from the board.
      expect(await remote.stop()).toBe(true);
      await expect(time.getByTestId('time-offline')).toHaveText('Laptop is not connected. Your time shows when it connects.', { timeout: 30_000 });
      await expect(time.getByTestId('time-entry')).toHaveCount(0);
      await expect(pane.getByText('Laptop is not connected')).toHaveCount(1);

      // Back on, at its new address, with its Jira site and the worklog
      // served again: the week reads again. The page stays off the network
      // until the fake Jira is seeded, which a relaunched harness serves
      // only once the spec seeds it; a real computer serves its trackers
      // from boot.
      await page.context().setOffline(true);
      remote = await launchHarness({ dataDir });
      const jiraURL = await seedFakeJira(remote);
      await logJiraWork(jiraURL, 'WEB-2', monday, 5400);
      await home.rpc('RepairBackendAddress', laptop, `https://127.0.0.1:${remote.bootstrap.port}`);
      await page.context().setOffline(false);
      await expect(time.getByTestId('time-offline')).toHaveCount(0, { timeout: 60_000 });
      await expect((await byDay(time, monday, compact)).getByTestId('time-entry-hours')).toHaveText('1h 30m', { timeout: 30_000 });
    } finally {
      await remote?.close();
      await home?.close();
      await rm(root, { recursive: true, force: true });
    }
  });
}
