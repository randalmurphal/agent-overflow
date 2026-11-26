// The Issues pane's Backlog screen (docs/specs/issues.md, "Backlog and
// sprint planning"), shared by the desktop and compact projects, through
// the shipped UI, the real backend and the fake Jira.
//
// Covered on both: the sections (the active sprint, a future sprint, the
// backlog) with their numbers (done of the sprint, the future sprint
// against the last sprint's pace with each person's load and how far it
// is over, the backlog's tickets, points and those with no estimate),
// opening the screen reads nothing from Jira, the active sprint starts
// folded, and a fold outlasts a reload; Search and Only mine filter the
// rows, Display hides a mark and Refresh reads the board again; a
// ticket made at a section's foot is a Story by default in the board's
// location, picked from the projects the board shows, offers what Jira
// fills (a Story's priority) and asks what it requires (a Task's), reads
// the board's projects, the types and each type's fields once for the
// form, and lands in that sprint; one Jira made but did not move into its
// sprint is not made again, says so, and moves on request, the offer
// ending once the board shows it there; a sprint is created (a refusal staying under its form), changed, refused a clearing
// of its dates, completed with its unfinished tickets moved to the
// destination it shows, and started; the row's menu is the card's and
// moves a row to a sprint, offering no Backlog to a row in the backlog
// whose card names a closed sprint, and holds its Move to sprint back
// while the row's move is out. On desktop: a row's marks
// change its type, priority, points and assignee; a carried row ranks in
// its section, moves into another at its place while both headers say
// what their numbers become, and back to the backlog; a row whose move is
// out takes no second carry; a click opens the ticket beside the list,
// and Ctrl or Cmd in its own pane. On compact: a row is two lines with a
// menu button, and the fields change from the menu.

import type { Locator, Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test } from './fixtures.js';
import { longPress } from './touch-helpers.js';
import { FORM_READ_AGE_MS } from '../../frontend/src/lib/pageTiming.ts';
import { methodIdByName } from './offhost-helpers.js';
import { JIRA_SITE, connectFakeJiraBoard, openIssuesPane, press, seedFakeJira, trackerRequests, type ExtraJira, type LoggedRequest } from './tracker-helpers.js';

export interface BacklogSurface {
  compact: boolean;
}

const JIRA = { tracker: 'jira', site: JIRA_SITE };
const EDIT_ROUTE = 'PUT rest/api/3/issue/{issue}';
const RANK_ROUTE = 'PUT rest/agile/1.0/issue/rank';
const TO_SPRINT_ROUTE = 'POST rest/agile/1.0/sprint/{sprint}/issue';
const TO_BACKLOG_ROUTE = 'POST rest/agile/1.0/backlog/{board}/issue';
const CREATE_ROUTE = 'POST rest/api/3/issue';
const NEW_SPRINT_ROUTE = 'POST rest/agile/1.0/sprint';
const SPRINT_ROUTE = 'POST rest/agile/1.0/sprint/{sprint}';
const TYPES_ROUTE = 'GET rest/api/3/issue/createmeta/{project}/issuetypes';
const FIELDS_ROUTE = 'GET rest/api/3/issue/createmeta/{project}/issuetypes/{type}';
const BOARD_PROJECTS_ROUTE = 'GET rest/agile/1.0/board/{board}/project';

const DAY = 24 * 3600_000;
const iso = (offset: number) => new Date(Date.now() + offset).toISOString();
const POINTS = 'customfield_10016';

// Sprint 1 (active) holds the seed's WEB-1 (In Progress) and WEB-2 (To
// Do), neither estimated, and WEB-5 (Done, 2 points). Sprint 2 (future)
// holds WEB-8 (3 points, User 1) and WEB-9 (5 points, nobody's). Sprint 0
// finished with WEB-10 done (4 points): the pace. The backlog holds the
// seed's WEB-3 (no estimate) and WEB-11 (1 point). A Task takes a
// priority to be made; a Story's is required with a default, which Jira
// fills.
const BACKLOG: ExtraJira = {
  projects: [],
  boards: [],
  estimation: POINTS,
  assignable: ['acc-user1'],
  fields: [
    { id: POINTS, name: 'Story Points', schema: { type: 'number', custom: 'com.atlassian.jira.plugin.system.customfieldtypes:float', customId: 10016 } },
  ],
  issueTypes: [
    { id: '10000', name: 'Epic', hierarchyLevel: 1 }, { id: '10001', name: 'Story', priorityDefaulted: true }, { id: '10002', name: 'Task', priorityRequired: true }, { id: '10003', name: 'Bug' },
  ].map(({ priorityRequired, priorityDefaulted, ...t }) => ({
    ...t,
    screen: [
      { field: 'summary', required: true },
      { field: 'priority', ...(priorityRequired ? { required: true } : priorityDefaulted ? { required: true, hasDefault: true } : {}) },
      ...['issuetype', 'description', 'assignee', 'labels', POINTS, 'customfield_10020'].map((field) => ({ field })),
    ],
  })),
  sprints: [
    { id: 2, name: 'Sprint 2', state: 'future', start: iso(14 * DAY), end: iso(28 * DAY) },
    { id: 3, name: 'Sprint 0', state: 'closed', start: iso(-30 * DAY), end: iso(-16 * DAY), complete: iso(-16 * DAY) },
  ],
  issues: [
    { id: '10105', key: 'WEB-5', project: 'WEB', type: '10002', status: '10002', summary: 'Finished this sprint', sprints: [1], values: { [POINTS]: 2 } },
    { id: '10108', key: 'WEB-8', project: 'WEB', type: '10001', status: '10000', summary: 'Planned for User 1', sprints: [2], assignee: 'acc-user1', values: { [POINTS]: 3 } },
    { id: '10109', key: 'WEB-9', project: 'WEB', type: '10002', status: '10000', summary: 'Planned for nobody', sprints: [2], values: { [POINTS]: 5 } },
    { id: '10110', key: 'WEB-10', project: 'WEB', type: '10002', status: '10002', summary: 'Done last sprint', sprints: [3], values: { [POINTS]: 4 } },
    { id: '10111', key: 'WEB-11', project: 'WEB', type: '10002', status: '10000', summary: 'Waiting in the backlog', values: { [POINTS]: 1 } },
  ],
};

function section(pane: Locator, list: string): Locator {
  return pane.locator(`[data-testid="issues-backlog-section"][data-list="${list}"]`);
}

function row(pane: Locator, issueId: string): Locator {
  return pane.locator(`[data-testid="issues-backlog-row"][data-issue="${issueId}"]`);
}

/** The issue IDs of a section's rows, in the order shown. */
async function sectionRows(pane: Locator, list: string): Promise<string[]> {
  return pane.locator(`[data-list="${list}"] [data-testid="issues-backlog-row"]`).evaluateAll((els) => els.map((el) => (el as HTMLElement).dataset.issue ?? ''));
}

/** The sections' lists, in the order shown. */
async function sectionLists(pane: Locator): Promise<string[]> {
  return pane.getByTestId('issues-backlog-section').evaluateAll((els) => els.map((el) => (el as HTMLElement).dataset.list ?? ''));
}

/** The calls this page sends, in the order its sockets send them, from
 * its first socket on, and which of them were answered. */
function sentCalls(page: Page): { readonly sent: () => readonly string[]; readonly answered: (method: string) => number } {
  const byId = new Map<number, string>();
  for (const m of ['RefreshTrackerSubscription', 'TrackerBoardProjects', 'TrackerIssueTypes', 'TrackerCreateFields']) byId.set(methodIdByName(m), m);
  const sent: string[] = [];
  const asked = new Map<string, string>();
  const answered = new Map<string, number>();
  page.on('websocket', (socket) => {
    socket.on('framesent', ({ payload }) => {
      if (typeof payload !== 'string' || !payload.startsWith('{"type":"rpc"')) return;
      const frame = JSON.parse(payload) as { id?: string; methodId?: number };
      const method = byId.get(frame.methodId ?? -1) ?? 'other';
      sent.push(method);
      if (frame.id) asked.set(frame.id, method);
    });
    socket.on('framereceived', ({ payload }) => {
      if (typeof payload !== 'string' || !payload.startsWith('{"type":"rpc"')) return;
      const method = asked.get((JSON.parse(payload) as { id?: string }).id ?? '');
      if (method) answered.set(method, (answered.get(method) ?? 0) + 1);
    });
  });
  return { sent: () => sent, answered: (method) => answered.get(method) ?? 0 };
}

/** Switches the pane to Backlog: its tab, or the folded picker. */
export async function openBacklog(page: Page, pane: Locator, compact: boolean): Promise<Locator> {
  const picker = pane.getByTestId('issues-tabs-picker');
  if (await picker.isVisible()) {
    await press(picker, compact);
    await press(page.getByTestId('issues-tab-option').filter({ hasText: 'Backlog' }), compact);
  } else {
    await press(pane.locator('[data-testid="issues-tab"][data-screen="backlog"]'), compact);
  }
  const screen = pane.getByTestId('issues-backlog');
  await expect(screen).toBeVisible({ timeout: 15_000 });
  return screen;
}

/** Opens the board, waits for its cards, then switches to Backlog. */
async function openBoardBacklog(harness: HarnessApp, page: Page, name: string, compact: boolean, fixture: ExtraJira = BACKLOG): Promise<{ pane: Locator; backlog: Locator }> {
  await connectFakeJiraBoard(harness, name, fixture);
  await harness.open(page);
  const pane = await openIssuesPane(page, compact);
  await expect(pane.locator('[data-testid="issues-card"][data-issue="10101"]')).toBeVisible({ timeout: 30_000 });
  const backlog = await openBacklog(page, pane, compact);
  await expect(section(backlog, 'backlog')).toBeVisible({ timeout: 15_000 });
  return { pane, backlog };
}

/** Opens a row's menu: a right click, or its button on the phone. */
async function rowMenu(page: Page, backlog: Locator, issueId: string, key: string, compact: boolean): Promise<Locator> {
  const menu = page.getByRole('menu', { name: `Card actions for ${key}` });
  if (compact) await press(row(backlog, issueId).getByTestId('issues-backlog-row-menu'), true);
  else await row(backlog, issueId).getByTestId('issues-backlog-row-open').click({ button: 'right' });
  await expect(menu).toBeVisible();
  return menu;
}

/** The labels of the rows a menu shows. */
async function menuRows(menu: Locator): Promise<string[]> {
  return menu.getByRole('menuitem').evaluateAll((els) => els.map((el) => el.querySelector('span')?.textContent?.trim() ?? el.textContent?.trim() ?? ''));
}

/** Presses a row with the mouse and moves it far enough to pick it up. */
async function pickUp(page: Page, from: Locator): Promise<void> {
  // The last carry's copy has landed.
  const copy = page.getByTestId('issues-card-carried');
  await expect(copy).toHaveCount(0);
  const title = from.getByTestId('issues-card-title');
  const box = (await title.boundingBox())!;
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  await page.mouse.down();
  await page.mouse.move(box.x + box.width / 2 + 12, box.y + box.height / 2 + 4, { steps: 3 });
  await expect(copy).toHaveCount(1);
  await expect(copy).toContainText((await title.textContent()) ?? '');
}

/** Carries `from` to the top (`above`) or bottom of `to`, runs `hold`
 * while it is there, and lets go. */
async function carry(page: Page, from: Locator, to: Locator, above: boolean, hold?: () => Promise<void>): Promise<void> {
  await pickUp(page, from);
  const box = (await to.boundingBox())!;
  await page.mouse.move(box.x + box.width / 2, box.y + (above ? box.height * 0.25 : box.height * 0.75), { steps: 12 });
  if (hold) await hold();
  await page.mouse.up();
}

/** The requests to `route` logged after `from` of the log. */
async function requestsSince(harness: HarnessApp, from: number): Promise<LoggedRequest[]> {
  const log = await harness.rpc<{ requests: LoggedRequest[] }>('HarnessTrackerRequests', 0);
  return log.requests.slice(from);
}

async function requestCount(harness: HarnessApp): Promise<number> {
  return (await harness.rpc<{ requests: LoggedRequest[] }>('HarnessTrackerRequests', 0)).requests.length;
}

export function registerBacklogFlow({ compact }: BacklogSurface): void {
  const name = (s: string) => `${compact ? 'compact-' : ''}issues-backlog-${s}`;

  test('Backlog shows the open sprints and the backlog with their numbers, reads nothing on opening, and keeps its folds', async ({ harness, page }) => {
    test.setTimeout(120_000);
    await connectFakeJiraBoard(harness, name('sections'), BACKLOG);
    await harness.open(page);
    let pane = await openIssuesPane(page, compact);
    await expect(pane.locator('[data-testid="issues-card"][data-issue="10101"]')).toBeVisible({ timeout: 30_000 });
    // The board has synced: the screen's sections are its computer's.
    await expect.poll(async () => (await harness.rpc<{ requests: LoggedRequest[] }>('HarnessTrackerRequests', 0)).requests.length).toBeGreaterThan(0);
    const before = await requestCount(harness);
    let backlog = await openBacklog(page, pane, compact);
    await expect(section(backlog, 'backlog')).toBeVisible({ timeout: 15_000 });
    expect(await sectionLists(backlog)).toEqual(['sprint:1', 'sprint:2', 'backlog']);

    // The active sprint starts folded: its numbers, none of its rows.
    const active = section(backlog, 'sprint:1');
    await expect(active).toHaveAttribute('data-kind', 'active');
    await expect(active).not.toHaveAttribute('data-open', '');
    await expect(active.getByTestId('issues-backlog-numbers')).toHaveText('2 of 2 points done');
    await expect(active.getByTestId('issues-backlog-note')).toContainText('days left');
    expect(await sectionRows(backlog, 'sprint:1')).toEqual([]);

    // The future sprint against the last sprint's pace, over it by 4,
    // with User 1's 3 points before the unassigned 5.
    const future = section(backlog, 'sprint:2');
    await expect(future).toHaveAttribute('data-open', '');
    await expect(future.getByTestId('issues-backlog-numbers')).toHaveText('8 of about 4 points, the last sprint · 4 over');
    await expect(future.getByTestId('issues-backlog-warn')).toHaveText('4 over');
    await expect(future.getByTestId('issues-backlog-cap')).toHaveAttribute('data-over', '');
    await expect(future.getByTestId('issues-backlog-load-person')).toHaveCount(2);
    await expect(future.getByTestId('issues-backlog-load-person').first()).toHaveAttribute('title', 'User 1: 3 points in this sprint');
    await expect(future.getByTestId('issues-backlog-load-person').last()).toHaveAttribute('title', 'Unassigned: 5 points in this sprint');
    expect(await sectionRows(backlog, 'sprint:2')).toEqual(['10108', '10109']);

    // The backlog: its tickets, its points and the one with no estimate.
    const rest = section(backlog, 'backlog');
    await expect(rest.getByTestId('issues-backlog-numbers')).toHaveText('2 tickets · 1 point · 1 without an estimate');
    expect((await sectionRows(backlog, 'backlog')).sort()).toEqual(['10103', '10111']);

    // A compact row is two lines with its menu button; a desktop row one.
    const shown = row(backlog, '10111');
    await expect(shown.getByTestId('issues-backlog-row-menu')).toHaveCount(compact ? 1 : 0);
    const lines = await shown.evaluate((el) => el.getBoundingClientRect().height);
    expect(lines).toBeGreaterThan(compact ? 50 : 30);
    expect(lines).toBeLessThan(compact ? 90 : 45);

    // Opening the screen read nothing from Jira.
    expect((await requestsSince(harness, before)).map((r) => r.route)).toEqual([]);

    // Search filters the rows of every section.
    await press(pane.getByTestId('issues-search'), compact);
    const bar = pane.getByTestId('pane-find-bar');
    await bar.getByTestId('pane-find-input').fill('nobody');
    await expect(backlog.getByTestId('issues-backlog-row')).toHaveCount(1);
    await expect(row(backlog, '10109')).toBeVisible();
    await expect(section(backlog, 'backlog').locator('xpath=..').getByTestId('issues-backlog-empty')).toHaveCount(0);
    await bar.getByTestId('pane-find-input').fill('');
    await page.keyboard.press('Escape');
    await expect(row(backlog, '10111')).toBeVisible();

    // Only mine keeps User 1's rows; Display hides the points; Refresh
    // reads the board again and keeps the rows.
    if (compact) {
      await press(pane.getByTestId('issues-display'), true);
      await press(page.getByTestId('issues-only-mine-item'), true);
      await page.keyboard.press('Escape');
    } else {
      await pane.getByTestId('issues-only-mine').click();
    }
    await expect(backlog.getByTestId('issues-backlog-row')).toHaveCount(1);
    await expect(row(backlog, '10108')).toBeVisible();
    if (compact) {
      await press(pane.getByTestId('issues-display'), true);
      await press(page.getByTestId('issues-only-mine-item'), true);
      await page.keyboard.press('Escape');
    } else {
      await pane.getByTestId('issues-only-mine').click();
    }
    await expect(row(backlog, '10111')).toBeVisible();
    await press(pane.getByTestId('issues-display'), compact);
    await press(page.getByTestId('issues-display-estimate'), compact);
    await page.keyboard.press('Escape');
    await expect(row(backlog, '10111').getByTestId('issues-card-estimate')).toHaveCount(0);
    await expect(row(backlog, '10111').getByTestId('issues-card-estimate-edit')).toHaveCount(0);
    await press(pane.getByTestId('issues-display'), compact);
    await press(page.getByTestId('issues-display-estimate'), compact);
    await page.keyboard.press('Escape');
    await expect(row(backlog, '10111').getByTestId('issues-card-estimate')).toHaveText('1');
    const beforeRefresh = await requestCount(harness);
    await press(pane.getByTestId('issues-refresh'), compact);
    await expect.poll(() => requestCount(harness)).toBeGreaterThan(beforeRefresh);
    await expect(row(backlog, '10111')).toBeVisible();

    // Unfold the active sprint and fold the backlog: kept across a reload.
    await press(active.getByTestId('issues-backlog-fold'), compact);
    await expect(active).toHaveAttribute('data-open', '');
    await expect.poll(() => sectionRows(backlog, 'sprint:1').then((r) => r.sort())).toEqual(['10101', '10102', '10105']);
    await press(rest.getByTestId('issues-backlog-fold'), compact);
    await expect(rest).not.toHaveAttribute('data-open', '');
    expect(await sectionRows(backlog, 'backlog')).toEqual([]);

    await page.reload();
    pane = page.getByTestId('issues-pane');
    if (!(await pane.isVisible())) pane = await openIssuesPane(page, compact);
    backlog = pane.getByTestId('issues-backlog');
    // The pane opens on the screen it was on.
    await expect(backlog).toBeVisible({ timeout: 30_000 });
    await expect(section(backlog, 'sprint:1')).toHaveAttribute('data-open', '', { timeout: 30_000 });
    await expect(section(backlog, 'backlog')).not.toHaveAttribute('data-open', '');
    await expect(row(backlog, '10101')).toBeVisible();
    await expect(row(backlog, '10111')).toHaveCount(0);
  });

  test('a ticket made at a section\'s foot is a Story in the board\'s project by default, asks only what Jira requires, and lands in that sprint', async ({ harness, page }) => {
    test.setTimeout(150_000);
    // The board's filter also shows APP's tickets.
    const { backlog } = await openBoardBacklog(harness, page, name('create'), compact, { ...BACKLOG, projects: [{ id: '10001', key: 'APP', name: 'App' }], boardAlso: ['APP'] });
    const foot = backlog.locator('[data-testid="issues-backlog-foot"][data-list="sprint:2"]');
    await press(foot.getByTestId('issues-backlog-new'), compact);
    const form = foot.getByTestId('issues-backlog-create');
    const title = form.getByTestId('issues-backlog-create-title');
    await expect(title).toBeFocused();
    await title.fill('Made in Sprint 2');

    // The board's location, named from the board's projects; the first type
    // at the standard level, not the Epic Jira lists first; a Story's
    // priority, which Jira fills, offered and not asked.
    const items = page.getByTestId('ticket-picker-item');
    const projectField = form.locator('[data-testid="ticket-field"][data-field="project"]');
    await expect(projectField.getByTestId('ticket-field-value')).toHaveText('Web', { timeout: 15_000 });
    const typeField = form.locator('[data-testid="ticket-field"][data-field="type"]');
    await expect(typeField.getByTestId('ticket-field-value')).toHaveText('Story', { timeout: 15_000 });
    const priority = form.locator('[data-testid="ticket-field"][data-field="priority"]');
    await expect(priority.getByTestId('ticket-field-value')).toHaveText('The tracker\'s default', { timeout: 15_000 });
    await expect(priority).not.toContainText('(required)');
    await expect(form.getByTestId('issues-backlog-create-missing')).toHaveCount(0);
    await press(form.getByTestId('issues-backlog-create-submit'), compact);

    // Made: the title clears for the next, and the ticket is in Sprint 2.
    await expect(title).toHaveValue('', { timeout: 15_000 });
    let made = (await trackerRequests(harness, CREATE_ROUTE)).map((r) => r.body as { fields: Record<string, unknown> });
    expect(made).toHaveLength(1);
    expect(made[0].fields).toMatchObject({ project: { id: '10000' }, issuetype: { id: '10001' }, summary: 'Made in Sprint 2' });
    expect(made[0].fields).not.toHaveProperty('priority');
    await expect(backlog.getByTestId('issues-backlog-row').filter({ hasText: 'Made in Sprint 2' })).toBeVisible({ timeout: 15_000 });
    const madeId = await backlog.getByTestId('issues-backlog-row').filter({ hasText: 'Made in Sprint 2' }).getAttribute('data-issue');
    expect(await sectionRows(backlog, 'sprint:2')).toContain(madeId);

    // The project picker offers the projects the board shows.
    await press(projectField.getByTestId('ticket-field-value'), compact);
    await expect(items).toHaveCount(2);
    await expect(items.nth(0)).toContainText('Web (WEB)');
    await expect(items.nth(1)).toContainText('App (APP)');
    await page.keyboard.press('Escape');
    await expect(items).toHaveCount(0);

    // A Task requires its priority.
    await press(typeField.getByTestId('ticket-field-value'), compact);
    await press(items.filter({ hasText: 'Task' }), compact);
    await expect(typeField.getByTestId('ticket-field-value')).toHaveText('Task');
    await expect(priority).toContainText('Priority (required)', { timeout: 15_000 });
    await expect(form.getByTestId('issues-backlog-create-missing')).toHaveText('Needs Priority');
    await expect(form.getByTestId('issues-backlog-create-submit')).toBeDisabled();
    await press(priority.getByTestId('ticket-field-value'), compact);
    await press(items.filter({ hasText: 'High' }).filter({ hasNotText: 'Highest' }), compact);
    await expect(priority.getByTestId('ticket-field-value')).toHaveText('High');
    await title.fill('A task in Sprint 2');
    await press(form.getByTestId('issues-backlog-create-submit'), compact);
    await expect(title).toHaveValue('', { timeout: 15_000 });
    made = (await trackerRequests(harness, CREATE_ROUTE)).map((r) => r.body as { fields: Record<string, unknown> });
    expect(made).toHaveLength(2);
    expect(made[1].fields).toMatchObject({ project: { id: '10000' }, issuetype: { id: '10002' }, summary: 'A task in Sprint 2', priority: { id: '2' } });

    // What the form read from Jira: the board's projects and the types once,
    // and each type's fields once: each Add checks its values against the
    // screen the form read, which the Jira client keeps for a minute. A
    // second open reads nothing.
    expect(await trackerRequests(harness, BOARD_PROJECTS_ROUTE)).toHaveLength(1);
    expect(await trackerRequests(harness, TYPES_ROUTE)).toHaveLength(1);
    const fieldReads = (await trackerRequests(harness, FIELDS_ROUTE)).map((r) => r.path.split('/').pop());
    expect(fieldReads).toEqual(['10001', '10002']);
    await press(form.getByTestId('issues-backlog-create-cancel'), compact);
    await press(foot.getByTestId('issues-backlog-new'), compact);
    await expect(form.getByTestId('issues-backlog-create-title')).toBeVisible();
    // The type picked last on the board is the next one's.
    await expect(typeField.getByTestId('ticket-field-value')).toHaveText('Task');
    await expect(priority).toContainText('Priority (required)');
    expect(await trackerRequests(harness, BOARD_PROJECTS_ROUTE)).toHaveLength(1);
    expect(await trackerRequests(harness, TYPES_ROUTE)).toHaveLength(1);
    expect(await trackerRequests(harness, FIELDS_ROUTE)).toHaveLength(fieldReads.length);
    await form.getByTestId('issues-backlog-create-title').press('Escape');
    await expect(form).toHaveCount(0);
  });

  test('a ticket Jira made but did not move into its sprint is not made again, says so, and moves on request', async ({ harness, page }) => {
    test.setTimeout(120_000);
    const { backlog } = await openBoardBacklog(harness, page, name('unplaced'), compact);
    await harness.rpc('HarnessTrackerFault', {
      origin: JIRA, route: TO_SPRINT_ROUTE, kind: 'refuse', times: 1,
      status: 400, body: { errorMessages: ['Sprint 2 takes no more tickets now.'] },
    });
    const foot = backlog.locator('[data-testid="issues-backlog-foot"][data-list="sprint:2"]');
    await press(foot.getByTestId('issues-backlog-new'), compact);
    const form = foot.getByTestId('issues-backlog-create');
    const title = form.getByTestId('issues-backlog-create-title');
    await title.fill('Made but not moved');
    await expect(form.locator('[data-testid="ticket-field"][data-field="type"]').getByTestId('ticket-field-value')).toHaveText('Story', { timeout: 15_000 });
    await press(form.getByTestId('issues-backlog-create-submit'), compact);

    // Made: the title is done with, the offer names the ticket and what did
    // not happen, and Add cannot send it again.
    await expect(title).toHaveValue('', { timeout: 15_000 });
    const note = form.getByTestId('issues-backlog-create-unplaced-said');
    await expect(note).toContainText(/Created WEB-\d+, but it was not moved into its sprint: .*Sprint 2 takes no more tickets now\./, { timeout: 15_000 });
    await expect(form.getByTestId('issues-backlog-create-submit')).toBeDisabled();
    expect(await trackerRequests(harness, CREATE_ROUTE)).toHaveLength(1);
    const key = /Created (WEB-\d+)/.exec((await note.textContent()) ?? '')![1];
    const madeId = await backlog.getByTestId('issues-backlog-row').filter({ hasText: 'Made but not moved' }).getAttribute('data-issue', { timeout: 15_000 });
    expect(await sectionRows(backlog, 'backlog')).toContain(madeId);

    // The step that did not happen, for that ticket.
    const move = form.getByTestId('issues-backlog-create-move');
    await expect(move).toHaveText(`Move ${key} into Sprint 2`);
    await press(move, compact);
    await expect.poll(() => sectionRows(backlog, 'sprint:2'), { timeout: 15_000 }).toContain(madeId);
    // The board shows it in the sprint: the offer is gone.
    await expect(form.getByTestId('issues-backlog-create-unplaced')).toHaveCount(0);
    expect((await trackerRequests(harness, TO_SPRINT_ROUTE)).filter((r) => r.path.endsWith('/sprint/2/issue')).map((r) => r.body)).toEqual([
      { issues: [madeId] }, { issues: [madeId] },
    ]);
    expect(await trackerRequests(harness, CREATE_ROUTE)).toHaveLength(1);
  });

  test('New ticket offers what Jira offers now: past a minute, or after Refresh, it reads again, and not while no form is open', async ({ harness, page }) => {
    test.setTimeout(120_000);
    // A Story needs a component, which the screen lists from WEB's.
    const withParts = (names: string[]): ExtraJira => ({
      ...BACKLOG,
      components: names.map((n, i) => ({ id: String(30_001 + i), name: n })),
      issueTypes: BACKLOG.issueTypes!.map((t) => (t.id === '10001' ? { ...t, screen: [...t.screen, { field: 'components', required: true }] } : t)),
    });
    // The page clock runs as the real one does; the age is crossed below by moving it on.
    await page.clock.install();
    const calls = sentCalls(page);
    const { pane, backlog } = await openBoardBacklog(harness, page, name('fresh'), compact, withParts(['Web']));
    const formReads = async () => (await Promise.all([BOARD_PROJECTS_ROUTE, TYPES_ROUTE, FIELDS_ROUTE].map((route) => trackerRequests(harness, route)))).reduce((n, r) => n + r.length, 0);
    const foot = backlog.locator('[data-testid="issues-backlog-foot"][data-list="sprint:2"]');
    const form = foot.getByTestId('issues-backlog-create');
    const parts = form.locator('[data-testid="ticket-field"][data-field="components"]');
    const openForm = async () => {
      await press(foot.getByTestId('issues-backlog-new'), compact);
      await expect(form.locator('[data-testid="ticket-field"][data-field="type"]').getByTestId('ticket-field-value')).toHaveText('Story', { timeout: 15_000 });
      await expect(parts).toBeVisible();
    };
    const offered = async (want: string[]) => {
      await press(parts.getByTestId('ticket-field-value'), compact);
      await expect(page.getByTestId('ticket-picker-item')).toHaveText(want, { timeout: 15_000 });
      await page.keyboard.press('Escape');
      await expect(page.getByTestId('ticket-picker-item')).toHaveCount(0);
    };
    const closeForm = async () => {
      await press(form.getByTestId('issues-backlog-create-cancel'), compact);
      await expect(form).toHaveCount(0);
    };

    await openForm();
    await offered(['Web']);
    await expect.poll(formReads).toBe(3);
    await closeForm();
    // Within the minute: nothing read.
    await openForm();
    await closeForm();
    expect(await formReads()).toBe(3);

    // A teammate adds a component in Jira; past the minute the form reads
    // the projects, the types and the fields once each, and offers it.
    await seedFakeJira(harness, withParts(['Web', 'API']));
    await page.clock.fastForward(FORM_READ_AGE_MS);
    await openForm();
    await offered(['Web', 'API']);
    await expect.poll(formReads).toBe(6);

    // Refresh with the form open reads them again; with no form open it reads none.
    await seedFakeJira(harness, withParts(['Web', 'API', 'Docs']));
    await press(pane.getByTestId('issues-refresh'), compact);
    await offered(['Web', 'API', 'Docs']);
    await expect.poll(formReads).toBe(9);
    await closeForm();
    // Refresh with no form open sends no form read. The page sends its
    // calls in order, so a read that Refresh started is sent before the
    // next Refresh's call, which is pressed once the first was answered.
    const mark = calls.sent().length;
    const refreshed = calls.answered('RefreshTrackerSubscription');
    await press(pane.getByTestId('issues-refresh'), compact);
    await expect.poll(() => calls.answered('RefreshTrackerSubscription'), { timeout: 15_000 }).toBe(refreshed + 1);
    await press(pane.getByTestId('issues-refresh'), compact);
    const between = () => {
      const since = calls.sent().slice(mark);
      return since.slice(0, since.indexOf('RefreshTrackerSubscription', since.indexOf('RefreshTrackerSubscription') + 1));
    };
    await expect.poll(() => calls.sent().slice(mark).filter((m) => m === 'RefreshTrackerSubscription').length, { timeout: 15_000 }).toBe(2);
    expect(between().filter((m) => m !== 'RefreshTrackerSubscription' && m !== 'other')).toEqual([]);
    // Past the minute again: the next open reads all three.
    await page.clock.fastForward(FORM_READ_AGE_MS);
    await openForm();
    await expect.poll(formReads).toBe(12);
    // Each read was a person's act: none followed one settling.
    await offered(['Web', 'API', 'Docs']);
    expect(await formReads()).toBe(12);
  });

  test('a sprint is created, changed, refused a clearing of its dates, completed with its unfinished tickets moved, and started', async ({ harness, page }) => {
    test.setTimeout(150_000);
    const { backlog } = await openBoardBacklog(harness, page, name('sprints'), compact);
    const sprintForm = page.getByTestId('sprint-form');

    // A sprint Jira refuses: the reason under the form, what was filled kept.
    await harness.rpc('HarnessTrackerFault', {
      origin: JIRA, route: NEW_SPRINT_ROUTE, kind: 'refuse', times: 1,
      status: 400, body: { errorMessages: ['Sprints cannot be made on this board now.'] },
    });
    await press(section(backlog, 'backlog').getByTestId('issues-backlog-sprint-new'), compact);
    await expect(sprintForm).toHaveAttribute('data-mode', 'create');
    await sprintForm.getByTestId('sprint-form-name').fill('Sprint 3');
    await press(sprintForm.getByTestId('sprint-form-submit'), compact);
    await expect(sprintForm.getByTestId('ticket-write-outcome')).toContainText('Sprints cannot be made on this board now.', { timeout: 15_000 });
    await expect(sprintForm.getByTestId('sprint-form-name')).toHaveValue('Sprint 3');

    // Again: made, the form closes and the sprint is a section.
    await press(sprintForm.getByTestId('sprint-form-submit'), compact);
    await expect(sprintForm).toHaveCount(0, { timeout: 15_000 });
    expect((await trackerRequests(harness, NEW_SPRINT_ROUTE)).map((r) => r.body)).toEqual([
      { originBoardId: 1, name: 'Sprint 3' }, { originBoardId: 1, name: 'Sprint 3' },
    ]);
    await expect.poll(() => sectionLists(backlog), { timeout: 15_000 }).toHaveLength(4);
    const lists = await sectionLists(backlog);
    expect(lists.slice(0, 2)).toEqual(['sprint:1', 'sprint:2']);
    expect(lists[3]).toBe('backlog');
    const made = section(backlog, lists[2]);
    await expect(made).toContainText('Sprint 3');
    await expect(made).toHaveAttribute('data-kind', 'future');

    // Change Sprint 2's goal: only the goal is sent. Clearing its dates is
    // refused in the form, which Jira's sprint update cannot carry.
    await press(section(backlog, 'sprint:2').getByTestId('issues-backlog-sprint-edit'), compact);
    await expect(sprintForm).toHaveAttribute('data-mode', 'edit');
    const startDate = await sprintForm.getByTestId('sprint-form-start').inputValue();
    expect(startDate).not.toBe('');
    await sprintForm.getByTestId('sprint-form-start').fill('');
    await expect(sprintForm.getByTestId('sprint-form-problem')).toHaveText('The app cannot clear a sprint\'s dates: change them, or clear them in the tracker.');
    await expect(sprintForm.getByTestId('sprint-form-submit')).toBeDisabled();
    await sprintForm.getByTestId('sprint-form-start').fill(startDate);
    await expect(sprintForm.getByTestId('sprint-form-problem')).toHaveCount(0);
    await sprintForm.getByTestId('sprint-form-goal').fill('Ship the widget');
    await press(sprintForm.getByTestId('sprint-form-submit'), compact);
    await expect(sprintForm).toHaveCount(0, { timeout: 15_000 });
    const edits = (await trackerRequests(harness, SPRINT_ROUTE)).filter((r) => r.path.endsWith('/sprint/2')).map((r) => r.body);
    expect(edits).toEqual([{ goal: 'Ship the widget' }]);

    // Complete Sprint 1: one ticket of three finished, the two others to
    // Sprint 2, the first future sprint.
    await press(section(backlog, 'sprint:1').getByTestId('issues-backlog-sprint-complete'), compact);
    const complete = page.getByTestId('sprint-complete-form');
    await expect(complete.getByTestId('sprint-complete-summary')).toHaveText('2 of 2 points done. 2 of the tickets this board shows are not in a finished status.');
    await expect(complete.getByTestId('sprint-complete-into')).toHaveValue('2');
    await press(complete.getByTestId('sprint-complete-submit'), compact);
    await expect(complete).toHaveCount(0, { timeout: 15_000 });
    const moved = (await trackerRequests(harness, TO_SPRINT_ROUTE)).filter((r) => r.path.endsWith('/sprint/2/issue')).map((r) => r.body as { issues: string[] });
    expect(moved.flatMap((b) => b.issues).sort()).toEqual(['10101', '10102']);
    const closes = (await trackerRequests(harness, SPRINT_ROUTE)).filter((r) => r.path.endsWith('/sprint/1')).map((r) => r.body);
    expect(closes).toEqual([{ state: 'closed' }]);
    await expect.poll(() => sectionLists(backlog), { timeout: 15_000 }).toEqual(['sprint:2', lists[2], 'backlog']);
    await expect.poll(() => sectionRows(backlog, 'sprint:2').then((r) => r.sort())).toEqual(['10101', '10102', '10108', '10109']);

    // Start Sprint 2: today for the two weeks it was planned.
    await press(section(backlog, 'sprint:2').getByTestId('issues-backlog-sprint-start'), compact);
    await expect(sprintForm).toHaveAttribute('data-mode', 'start');
    await expect(sprintForm.getByTestId('sprint-form-start')).not.toHaveValue('');
    await press(sprintForm.getByTestId('sprint-form-submit'), compact);
    await expect(sprintForm).toHaveCount(0, { timeout: 15_000 });
    const starts = (await trackerRequests(harness, SPRINT_ROUTE)).filter((r) => r.path.endsWith('/sprint/2')).map((r) => r.body as Record<string, unknown>);
    expect(starts).toHaveLength(2);
    expect(starts[1]).toMatchObject({ state: 'active' });
    expect(Object.keys(starts[1]).sort()).toEqual(['endDate', 'startDate', 'state']);
    await expect(section(backlog, 'sprint:2')).toHaveAttribute('data-kind', 'active', { timeout: 15_000 });
  });

  test('a row\'s menu is the card\'s and moves the row to another sprint', async ({ harness, page }) => {
    test.setTimeout(120_000);
    // WEB-12 was left unfinished in Sprint 0: its card still names that
    // closed sprint, and it is in the backlog.
    const carried = { id: '10112', key: 'WEB-12', project: 'WEB', type: '10002', status: '10000', summary: 'Left over from Sprint 0', sprints: [3] };
    const { pane, backlog } = await openBoardBacklog(harness, page, name('menu'), compact, { ...BACKLOG, issues: [...BACKLOG.issues!, carried] });
    let menu = await rowMenu(page, backlog, '10112', 'WEB-12', compact);
    await press(menu.getByTestId('action-menu-more'), compact);
    await press(menu.getByTestId('issues-card-menu-sprint'), compact);
    // A closed sprint is no sprint to leave for the backlog it is in.
    expect(await menu.getByTestId('issues-card-sprint').evaluateAll((els) => els.map((el) => el.textContent?.trim() ?? ''))).toEqual(['Sprint 1 Active', 'Sprint 2 Planned']);
    await page.keyboard.press('Escape');
    await expect(menu).toHaveCount(0);

    menu = await rowMenu(page, backlog, '10111', 'WEB-11', compact);
    expect(await menuRows(menu)).toEqual(['Open', 'Move to…', 'Assignee…', 'Copy key', 'Show more']);
    await press(menu.getByTestId('action-menu-more'), compact);
    await press(menu.getByTestId('issues-card-menu-sprint'), compact);
    const sprints = menu.getByTestId('issues-card-sprint');
    // The open sprints; the backlog it is in is not offered.
    expect(await sprints.evaluateAll((els) => els.map((el) => el.textContent?.trim() ?? ''))).toEqual(['Sprint 1 Active', 'Sprint 2 Planned']);
    await press(sprints.filter({ hasText: 'Sprint 2' }), compact);
    await expect(menu).toHaveCount(0);
    await expect.poll(() => sectionRows(backlog, 'sprint:2'), { timeout: 15_000 }).toContain('10111');
    await expect.poll(async () => (await trackerRequests(harness, TO_SPRINT_ROUTE)).map((r) => r.body)).toEqual([{ issues: ['10111'] }]);
    await expect(section(backlog, 'sprint:2').getByTestId('issues-backlog-numbers')).toHaveText('9 of about 4 points, the last sprint · 5 over', { timeout: 15_000 });

    // Back to the backlog from the menu. While that move is out, the menu
    // holds Move to sprint back, as a carry is.
    await harness.rpc('HarnessTrackerFault', { origin: JIRA, route: TO_BACKLOG_ROUTE, kind: 'hold', times: 1 });
    menu = await rowMenu(page, backlog, '10111', 'WEB-11', compact);
    await press(menu.getByTestId('action-menu-more'), compact);
    await press(menu.getByTestId('issues-card-menu-sprint'), compact);
    await press(menu.getByTestId('issues-card-sprint').filter({ hasText: 'Backlog' }), compact);
    await expect.poll(async () => (await trackerRequests(harness, TO_BACKLOG_ROUTE)).some((r) => r.held)).toBe(true);
    menu = await rowMenu(page, backlog, '10111', 'WEB-11', compact);
    await press(menu.getByTestId('action-menu-more'), compact);
    await expect(menu.getByTestId('issues-card-menu-sprint')).toHaveAttribute('aria-disabled', 'true');
    await page.keyboard.press('Escape');
    await expect(menu).toHaveCount(0);
    await harness.rpc('HarnessTrackerRelease', { origin: JIRA, route: TO_BACKLOG_ROUTE });
    await expect.poll(() => sectionRows(backlog, 'backlog'), { timeout: 15_000 }).toContain('10111');
    await expect.poll(async () => (await trackerRequests(harness, TO_BACKLOG_ROUTE)).map((r) => r.body)).toEqual([{ issues: ['10111'] }]);

    // Open from the menu: the ticket, beside the list on desktop.
    menu = await rowMenu(page, backlog, '10109', 'WEB-9', compact);
    await press(menu.getByTestId('issues-card-menu-open'), compact);
    const ticket = pane.getByTestId('ticket-screen');
    await expect(ticket).toHaveAttribute('data-issue', '10109');
    if (!compact) {
      await expect(pane.getByTestId('issues-ticket')).toHaveAttribute('data-placement', 'beside');
      await expect(backlog).toBeVisible();
      await expect(row(backlog, '10109')).toHaveAttribute('data-open', '');
    }
  });

  if (compact) {
    test('on the phone a row\'s fields change from its menu', async ({ harness, page }) => {
      test.setTimeout(120_000);
      const { backlog } = await openBoardBacklog(harness, page, name('fields'), true);
      const changed = row(backlog, '10111');
      const items = page.getByTestId('ticket-picker-item');
      let menu = await rowMenu(page, backlog, '10111', 'WEB-11', true);
      await press(menu.getByTestId('action-menu-more'), true);
      await press(menu.getByTestId('issues-card-menu-priority'), true);
      await press(items.filter({ hasText: 'High' }).filter({ hasNotText: 'Highest' }), true);
      await expect(changed.getByTestId('issues-card-priority')).toHaveAttribute('title', 'High priority', { timeout: 15_000 });
      menu = await rowMenu(page, backlog, '10111', 'WEB-11', true);
      await press(menu.getByTestId('action-menu-more'), true);
      await press(menu.getByTestId('issues-card-menu-points'), true);
      const typed = page.getByTestId('issues-card-typed').getByTestId('ticket-field-input');
      await typed.fill('2');
      await typed.press('Enter');
      await expect(changed.getByTestId('issues-card-estimate')).toHaveText('2', { timeout: 15_000 });
      await expect(section(backlog, 'backlog').getByTestId('issues-backlog-numbers')).toHaveText('2 tickets · 2 points · 1 without an estimate', { timeout: 15_000 });
      expect((await trackerRequests(harness, EDIT_ROUTE, ['WEB-11', '10111'])).map((r) => r.body)).toEqual([{ fields: { priority: { id: '2' } } }]);
      // The long press opens the same menu.
      menu = page.getByRole('menu', { name: 'Card actions for WEB-9' });
      await longPress(page, row(backlog, '10109').getByTestId('issues-backlog-row-open'), menu);
      await expect(menu).toBeVisible();
    });
  }

  if (!compact) {
    test('a row\'s marks change its fields, and a click opens its ticket beside or in its own pane', async ({ harness, page }) => {
      test.setTimeout(120_000);
      await page.setViewportSize({ width: 1600, height: 900 });
      const { pane, backlog } = await openBoardBacklog(harness, page, name('fields'), false);
      const changed = row(backlog, '10111');
      const items = page.getByTestId('ticket-picker-item');
      await changed.hover();
      await changed.getByTestId('issues-card-priority-edit').click();
      await items.filter({ hasText: 'High' }).filter({ hasNotText: 'Highest' }).click();
      await expect(changed.getByTestId('issues-card-priority-edit')).toHaveAttribute('aria-label', 'Priority: High', { timeout: 15_000 });
      await changed.getByTestId('issues-card-type-edit').click();
      await items.filter({ hasText: 'Bug' }).click();
      await expect(changed.getByTestId('issues-card-type-edit')).toHaveAttribute('aria-label', 'Type: Bug', { timeout: 15_000 });
      await changed.getByTestId('issues-card-estimate-edit').click();
      const typed = page.getByTestId('issues-card-typed').getByTestId('ticket-field-input');
      await typed.fill('3');
      await typed.press('Enter');
      await expect(changed.getByTestId('issues-card-estimate')).toHaveText('3', { timeout: 15_000 });
      await expect(section(backlog, 'backlog').getByTestId('issues-backlog-numbers')).toHaveText('2 tickets · 3 points · 1 without an estimate', { timeout: 15_000 });
      await changed.hover();
      await changed.getByTestId('issues-card-assignee-edit').click();
      await page.getByTestId('ticket-picker-find').pressSequentially('User');
      await items.filter({ hasText: 'User 1' }).click();
      await expect(changed.getByTestId('issues-card-assignee-edit')).toHaveAttribute('aria-label', 'Assignee: User 1', { timeout: 15_000 });
      expect((await trackerRequests(harness, EDIT_ROUTE, ['WEB-11', '10111'])).map((r) => r.body)).toEqual([
        { fields: { priority: { id: '2' } } }, { fields: { issuetype: { id: '10003' } } }, { fields: { assignee: { accountId: 'acc-user1' } } },
      ]);
      expect((await trackerRequests(harness, 'PUT rest/agile/1.0/issue/{issue}/estimation')).map((r) => r.body)).toEqual([{ value: '3' }]);

      // A click opens the ticket beside the list; Ctrl or Cmd in its own pane.
      await changed.getByTestId('issues-backlog-row-open').click();
      await expect(pane.getByTestId('ticket-screen')).toHaveAttribute('data-issue', '10111');
      await expect(pane.getByTestId('issues-ticket')).toHaveAttribute('data-placement', 'beside');
      await expect(backlog).toBeVisible();
      await row(backlog, '10109').getByTestId('issues-backlog-row-open').click({ modifiers: ['ControlOrMeta'] });
      const own = page.getByTestId('ticket-pane');
      await expect(own).toHaveCount(1);
      await expect(own.getByTestId('ticket-screen')).toHaveAttribute('data-issue', '10109');
    });

    test('a carried row ranks in its section, moves into another at its place with both headers previewing, and back', async ({ harness, page }) => {
      test.setTimeout(120_000);
      await page.setViewportSize({ width: 1400, height: 1000 });
      const { backlog } = await openBoardBacklog(harness, page, name('carry'), false);
      const order = await sectionRows(backlog, 'backlog');
      expect(order.sort()).toEqual(['10103', '10111']);
      const [first, second] = await sectionRows(backlog, 'backlog');

      // Rank: the second row to the top of the backlog.
      await carry(page, row(backlog, second), row(backlog, first), true);
      await expect.poll(() => sectionRows(backlog, 'backlog'), { timeout: 15_000 }).toEqual([second, first]);
      await expect.poll(async () => (await trackerRequests(harness, RANK_ROUTE)).map((r) => r.body)).toEqual([
        { issues: [second], rankBeforeIssue: first, rankCustomFieldId: 10019 },
      ]);

      // Carried over Sprint 2, between WEB-8 and WEB-9: a gap opens there,
      // and both headers say what their numbers become.
      const moving = row(backlog, '10111');
      await carry(page, moving, row(backlog, '10109'), true, async () => {
        await expect(section(backlog, 'sprint:2').getByTestId('issues-backlog-to')).toHaveText('→ 9');
        await expect(section(backlog, 'backlog').getByTestId('issues-backlog-to').first()).toHaveText('→ 1 ticket');
        const shift = await row(backlog, '10109').evaluate((el) => (el.closest('[data-list]') as HTMLElement).style.transform);
        expect(shift).toMatch(/^translateY\(\d+(\.\d+)?px\)$/);
        // The carried copy stays level.
        const copy = await page.getByTestId('issues-card-carried').evaluate((el) => getComputedStyle(el).transform);
        expect(copy === 'none' || /^matrix\(1, 0, 0, 1,/.test(copy)).toBe(true);
      });
      await expect.poll(() => sectionRows(backlog, 'sprint:2'), { timeout: 15_000 }).toEqual(['10108', '10111', '10109']);
      expect((await trackerRequests(harness, TO_SPRINT_ROUTE)).map((r) => r.body)).toEqual([{ issues: ['10111'], rankAfterIssue: '10108', rankCustomFieldId: 10019 }]);
      await expect(section(backlog, 'sprint:2').getByTestId('issues-backlog-numbers')).toHaveText('9 of about 4 points, the last sprint · 5 over', { timeout: 15_000 });

      // A row whose move is out takes no second carry.
      await harness.rpc('HarnessTrackerFault', { origin: JIRA, route: TO_BACKLOG_ROUTE, kind: 'hold', times: 1 });
      await carry(page, moving, section(backlog, 'backlog'), false);
      await expect.poll(async () => (await trackerRequests(harness, TO_BACKLOG_ROUTE)).some((r) => r.held)).toBe(true);
      await expect(moving).toHaveAttribute('data-moving', '');
      const box = (await moving.getByTestId('issues-card-title').boundingBox())!;
      await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
      await page.mouse.down();
      await page.mouse.move(box.x + box.width / 2 + 40, box.y + box.height / 2 + 40, { steps: 5 });
      await expect(page.getByTestId('issues-card-carried')).toHaveCount(0);
      await page.mouse.up();
      await harness.rpc('HarnessTrackerRelease', { origin: JIRA, route: TO_BACKLOG_ROUTE });
      await expect.poll(() => sectionRows(backlog, 'backlog'), { timeout: 15_000 }).toContain('10111');
      await expect(moving).not.toHaveAttribute('data-moving', '', { timeout: 15_000 });
      expect(await trackerRequests(harness, TO_BACKLOG_ROUTE)).toHaveLength(1);

      // A refused move: the row is back where Jira holds it, the reason under it.
      await harness.rpc('HarnessTrackerFault', {
        origin: JIRA, route: TO_SPRINT_ROUTE, kind: 'refuse', times: 1,
        status: 400, body: { errorMessages: ['WEB-11 cannot join Sprint 2 now.'] },
      });
      await carry(page, moving, row(backlog, '10108'), true);
      await expect(moving.getByTestId('ticket-write-outcome')).toContainText('WEB-11 cannot join Sprint 2 now.', { timeout: 15_000 });
      expect(await sectionRows(backlog, 'backlog')).toContain('10111');
    });
  }
}
