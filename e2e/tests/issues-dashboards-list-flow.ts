// A dashboard's ticket list with rows selected (docs/specs/issues.md,
// "Dashboards"), shared by the desktop and compact projects, through the
// shipped UI, the real backend and the fake Jira of
// issues-dashboards-flow.ts.
//
// Covered on both: a row's box shows on hover on desktop and always on
// compact, and selecting shows the bar; Move to lists the statuses the
// selected tickets can reach, off with the reason for the one they are
// all in, and moves each ticket with at most 4 of its moves out at
// once; a move Jira refuses says so under its row and the note counts it
// out; Assign assigns each ticket to the person picked; Start a thread
// makes one thread, linked to no ticket, with each ticket's file and the
// text on its unsent draft, and opens it.

import type { Locator, Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test } from './fixtures.js';
import { openBoardDashboards, SPRINT_E, widget } from './issues-dashboards-flow.js';
import { JIRA_SITE, press, trackerRequests } from './tracker-helpers.js';

export interface DashboardListSurface {
  compact: boolean;
}

const JIRA = { tracker: 'jira', site: JIRA_SITE };
const MOVE_ROUTE = 'POST rest/api/3/issue/{issue}/transitions';
const EDIT_ROUTE = 'PUT rest/api/3/issue/{issue}';
// The fake's workflow: a transition into each status, named for it.
const TO_DO = '11';
const IN_PROGRESS = '21';
const keys = (from: number, n: number) => Array.from({ length: n }, (_, i) => `WEB-${from + i}`);
const errorToasts = (page: Page) => page.locator('[role="alert"].text-error');

/** Opens Sprint E's finished tickets, 105 Done, in the drill. */
async function openSprintE(page: Page, screen: Locator, compact: boolean): Promise<Locator> {
  await press(widget(screen, 'finished').locator(`[data-testid="dashboard-bar"][data-bucket="sprint:${SPRINT_E}"]`), compact);
  const drill = page.getByTestId('dashboard-drill');
  await expect(drill.getByTestId('dashboard-drill-row')).toHaveCount(100, { timeout: 15_000 });
  return drill;
}

async function select(list: Locator, picked: readonly string[], compact: boolean): Promise<void> {
  for (const key of picked) await press(list.getByRole('checkbox', { name: `Select ${key}` }), compact);
  await expect(list.getByTestId('ticket-list-count')).toHaveText(`${picked.length} selected`);
}

async function moveTo(page: Page, list: Locator, status: string, compact: boolean): Promise<void> {
  await press(list.getByTestId('ticket-list-move'), compact);
  await press(page.getByTestId('ticket-list-move-status').filter({ hasText: status }), compact);
}

async function movesOf(harness: HarnessApp, picked: readonly string[]): Promise<unknown[]> {
  const all = await trackerRequests(harness, MOVE_ROUTE);
  return all.filter((r) => picked.some((key) => r.path.includes(`/${key}/`) || r.path.includes(`/${Number(key.slice(4)) + 10000}/`))).map((r) => r.body);
}

export function registerDashboardListFlow({ compact }: DashboardListSurface): void {
  const name = (s: string) => `${compact ? 'compact-' : ''}issues-dashboards-list-${s}`;

  test('a ticket list\'s rows are selected and moved together, at most 4 moves out at once, a refusal under its row', async ({ harness, page }) => {
    test.setTimeout(180_000);
    const { screen } = await openBoardDashboards(harness, page, name('move'), compact);

    // A row's box: on hover on desktop, always on compact.
    const stuck = widget(screen, 'stuck_list');
    const box = stuck.getByRole('checkbox', { name: 'Select WEB-20' });
    if (compact) {
      await expect(box).toHaveCSS('opacity', '1');
    } else {
      await expect(box).toHaveCSS('opacity', '0');
      await stuck.getByTestId('dashboard-list-row').hover();
      await expect(box).toHaveCSS('opacity', '1');
    }
    await press(box, compact);
    await expect(stuck.getByTestId('ticket-list-count')).toHaveText('1 selected');
    await expect(stuck.getByTestId('dashboard-list')).toHaveAttribute('data-selecting', 'true');
    await press(stuck.getByTestId('ticket-list-clear'), compact);
    await expect(stuck.getByTestId('ticket-list-bar')).toHaveCount(0);

    // Six Done tickets to In Progress, four moves out at a time.
    const drill = await openSprintE(page, screen, compact);
    const six = keys(200, 6);
    await select(drill, six, compact);
    await press(drill.getByTestId('ticket-list-move'), compact);
    await expect(page.getByTestId('ticket-list-move-title')).toHaveText('Move 6 tickets to');
    const statuses = page.getByTestId('ticket-list-move-status');
    await expect(statuses).toHaveCount(3, { timeout: 15_000 });
    await expect(statuses.filter({ hasText: 'Done' })).toContainText('They are all in Done already');
    await expect(statuses.filter({ hasText: 'Done' })).toBeDisabled();
    await harness.rpc('HarnessTrackerFault', { origin: JIRA, route: MOVE_ROUTE, kind: 'hold', times: 4 });
    await press(statuses.filter({ hasText: 'In Progress' }), compact);
    await expect(drill.getByTestId('ticket-list-note')).toHaveText('Moving 6 tickets to In Progress…');
    await expect.poll(async () => (await trackerRequests(harness, MOVE_ROUTE)).filter((r) => r.held).length, { timeout: 15_000 }).toBe(4);
    expect(await trackerRequests(harness, MOVE_ROUTE)).toHaveLength(4);
    await expect(drill.getByTestId('ticket-write-pending')).toHaveCount(6);
    await harness.rpc('HarnessTrackerRelease', { origin: JIRA, route: MOVE_ROUTE });
    await expect(drill.getByTestId('ticket-list-note')).toHaveText('6 tickets moved to In Progress', { timeout: 15_000 });
    expect(await movesOf(harness, six)).toEqual(six.map(() => ({ transition: { id: IN_PROGRESS } })));
    await expect(drill.getByTestId('ticket-write-pending')).toHaveCount(0);

    // A move Jira refuses says why under its row; the other is made.
    await harness.rpc('HarnessTrackerFault', {
      origin: JIRA, route: MOVE_ROUTE, kind: 'refuse', times: 1,
      status: 400, body: { errorMessages: ['This ticket cannot leave Done now.'] },
    });
    await select(drill, keys(206, 2), compact);
    await moveTo(page, drill, 'To Do', compact);
    await expect(drill.getByTestId('ticket-list-note')).toHaveText('1 of 2 tickets moved to To Do', { timeout: 15_000 });
    await expect(drill.getByTestId('ticket-write-outcome')).toHaveCount(1);
    await expect(drill.getByTestId('ticket-write-outcome')).toContainText('This ticket cannot leave Done now.');
    await expect(page.getByTestId('toast')).toHaveCount(0);
    expect((await movesOf(harness, keys(206, 2))).filter((b) => JSON.stringify(b) === JSON.stringify({ transition: { id: TO_DO } }))).toHaveLength(2);
  });

  test('a ticket list\'s rows are assigned together, and given to one new thread with each ticket\'s file', async ({ harness, page }) => {
    test.setTimeout(180_000);
    const { screen } = await openBoardDashboards(harness, page, name('assign'), compact);
    const drill = await openSprintE(page, screen, compact);

    await select(drill, keys(200, 2), compact);
    await press(drill.getByTestId('ticket-list-assign'), compact);
    // Jira lists the people who match what is typed.
    await page.getByTestId('ticket-picker-find').pressSequentially('User');
    await press(page.getByTestId('ticket-picker-item').filter({ hasText: 'User 2' }), compact);
    await expect(drill.getByTestId('ticket-list-note')).toHaveText('2 tickets assigned to User 2', { timeout: 15_000 });
    const edits = await trackerRequests(harness, EDIT_ROUTE);
    expect(edits.map((r) => [r.path.split('/').at(-1), r.body]).sort()).toEqual([
      ['10200', { fields: { assignee: { accountId: 'acc-user2' } } }],
      ['10201', { fields: { assignee: { accountId: 'acc-user2' } } }],
    ]);

    await select(drill, keys(202, 2), compact);
    await press(drill.getByTestId('ticket-list-thread'), compact);
    await page.getByTestId('ticket-list-thread-text').fill('find out why these took so long');
    await press(page.getByTestId('ticket-list-thread-start'), compact);
    if (compact) await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
    await expect(page.getByTestId('chat-header-title')).toHaveText('WEB-202 and WEB-203', { timeout: 30_000 });
    await expect(page.getByTestId('attachment-file-chip')).toHaveCount(2);
    await expect(page.getByTestId('attachment-file-chip').nth(0)).toContainText('WEB-202.md');
    await expect(page.getByTestId('attachment-file-chip').nth(1)).toContainText('WEB-203.md');
    await expect(page.getByLabel('Message Input')).toHaveValue('find out why these took so long');
    const rows = await harness.rpc<Array<{ title: string; ticketLink?: unknown }>>('HarnessListThreadRows');
    expect(rows.map((r) => [r.title, r.ticketLink ?? null])).toEqual([['WEB-202 and WEB-203', null]]);
    expect(await trackerRequests(harness, MOVE_ROUTE)).toEqual([]);
    expect(await trackerRequests(harness, EDIT_ROUTE)).toHaveLength(2);
    await expect(errorToasts(page)).toHaveCount(0);
  });
}
