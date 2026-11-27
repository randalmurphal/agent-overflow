// A project's status groups (Settings > Issues > Status groups) and its
// ticket pins (on the ticket) (docs/specs/issues.md, "Setup" and
// "Ticket"), shared by the desktop and compact projects, through the
// shipped UI, the real backend, its tracker.json and the fake trackers.
//
// Covered on both: each status of the project's board listed with Jira's
// group, a pick saved at once to the project's tracker.json, the Backlog
// and the Board following it without a reload (a status counted done
// leaves the backlog, one left out of counts leaves its column's count
// and keeps its card, and the status marks of the sprint's rows, the
// column, the card and the ticket draw the project's group, a left-out
// status Jira's), and the tracker's group picked to put it back;
// Complete sprint counting finished the tickets of the board's last
// column that has statuses, the ones its completion keeps; a
// save that lost to a hand edit of the file said in words, with what is
// stored shown and the hand edit kept; a file that cannot be read said
// with its path, and one a newer build saved said so, with no select; a
// board not read yet named, and its statuses listed when the page is
// shown again once it has been; a field pinned from its row and a
// section from its header, and the description unpinned, laying out
// every ticket of the project, kept in the file and after a reload; and a
// Linear ticket's sections pinned while its fields take no pin. On
// desktop: a connected browser's pick, followed by this window's page,
// and a paired computer's project, whose file is that computer's.

import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';

import type { Locator, Page } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import { openBacklog } from './issues-backlog-flow.js';
import { expect, test } from './fixtures.js';
import { pairOwnDevice } from './headless-pairing-helpers.js';
import { startTogether } from './launch-helpers.js';
import { confirmOnHost, mintInvite, nonLoopbackIPv4, redeemOnScreen } from './offhost-helpers.js';
import {
  LINEAR_TEAM,
  connectFakeJiraBoard,
  connectFakeLinearBoard,
  openIssuesPane,
  press,
  trackerRequests,
  type ConnectedBoard,
  type ExtraJira,
  type TrackerConnection,
} from './tracker-helpers.js';

export interface SetupSurface {
  compact: boolean;
}

export const EFFORT = 'customfield_10050';
export const ROOT_CAUSE = 'customfield_10095';
export const PINNED_ID = '10104';
export const OTHER_ID = '10105';
const DONE_ID = '10106';

function doc(text: string) {
  return { type: 'doc', version: 1, content: [{ type: 'paragraph', content: [{ type: 'text', text }] }] };
}

// The seed's WEB-1 (In Progress) and WEB-2 (To Do) in Sprint 1, WEB-3
// (To Do) in the backlog; WEB-4 and WEB-5 in Sprint 1 with an Effort and
// a Root cause, the fields a ticket pins, and WEB-6 done in Sprint 1.
export const SETUP: ExtraJira = {
  projects: [],
  boards: [],
  fields: [
    { id: EFFORT, name: 'Effort', schema: { type: 'number', custom: 'com.atlassian.jira.plugin.system.customfieldtypes:float', customId: 10050 } },
    { id: ROOT_CAUSE, name: 'Root cause', schema: { type: 'string', custom: 'com.atlassian.jira.plugin.system.customfieldtypes:textarea', customId: 10095 } },
  ],
  issueTypes: [{ id: '10001', name: 'Story' }, { id: '10002', name: 'Task' }].map((t) => ({
    ...t,
    screen: [{ field: 'summary', required: true }, ...['issuetype', 'description', 'assignee', 'priority', EFFORT, ROOT_CAUSE].map((field) => ({ field }))],
  })),
  issues: [
    { id: PINNED_ID, key: 'WEB-4', project: 'WEB', type: '10002', status: '10001', summary: 'Ticket to pin from', sprints: [1], description: doc('What to do'), values: { [EFFORT]: 3, [ROOT_CAUSE]: doc('A stale cache key') } },
    { id: OTHER_ID, key: 'WEB-5', project: 'WEB', type: '10002', status: '10001', summary: 'Another ticket', sprints: [1], description: doc('More to do'), values: { [EFFORT]: 5, [ROOT_CAUSE]: doc('A missing index') } },
    { id: DONE_ID, key: 'WEB-6', project: 'WEB', type: '10002', status: '10002', summary: 'Finished ticket', sprints: [1] },
  ],
};

/** Where the computer that holds `projectId` keeps its tracker setup:
 * tracker.json in the project's app directory, beside the key files'. */
export async function setupFile(harness: HarnessApp, projectId: string): Promise<string> {
  const [connection] = await harness.rpc<TrackerConnection[]>('TrackerConnections');
  const rows = await harness.rpc<Array<{ project: { id: string; slug: string } }>>('ListProjects');
  const slug = rows.find((row) => row.project.id === projectId)?.project.slug;
  if (!connection || !slug) throw new Error(`no connection or no project ${projectId} to place tracker.json by`);
  return join(dirname(dirname(connection.keyFilePath)), 'projects', slug, 'tracker.json');
}

export async function writeSetup(path: string, text: string): Promise<void> {
  await mkdir(dirname(path), { recursive: true });
  await writeFile(path, text, { mode: 0o600 });
}

export async function storedSetup(path: string): Promise<Record<string, unknown>> {
  return JSON.parse(await readFile(path, 'utf8')) as Record<string, unknown>;
}

function groupsFile(site: string, statuses: Record<string, string>): string {
  return JSON.stringify({ status_groups: { version: 1, sites: [{ tracker: 'jira', site, statuses }] } });
}

/** Opens the Settings > Issues page `tab`, whose screen has test ID
 * `screenId`; on compact from the list screen, and back to the rail from a
 * page first. */
export async function openIssuesSetting(page: Page, compact: boolean, tab: string, screenId: string): Promise<Locator> {
  const tabs = page.getByRole('tablist', { name: 'Settings Sections' });
  if (compact && (await page.getByTestId('settings-page-back').isVisible())) {
    await press(page.getByTestId('settings-page-back'), true);
  } else if (!(await tabs.isVisible())) {
    if (compact && (await page.locator('html').getAttribute('data-compact-screen')) === 'thread') {
      await press(page.getByTestId('compact-back'), true);
    }
    await press(page.getByTestId('sidebar-settings-button'), compact);
  }
  await press(tabs.getByRole('tab', { name: tab, exact: true }), compact);
  const screen = page.getByTestId(screenId);
  await expect(screen).toBeVisible();
  return screen;
}

/** Opens Settings > Issues > Status groups (openIssuesSetting). */
function openStatusGroups(page: Page, compact: boolean): Promise<Locator> {
  return openIssuesSetting(page, compact, 'Status groups', 'settings-tracker-statuses');
}

export async function closeSettings(page: Page, compact: boolean): Promise<void> {
  await press(page.getByRole('button', { name: 'Close Settings', exact: true }), compact);
  await expect(page.getByTestId('settings-overlay')).toHaveCount(0);
}

function statusRow(screen: Locator, status: string): Locator {
  return screen.locator(`[data-testid="tracker-status-row"][data-status="${status}"]`);
}

function groupOf(screen: Locator, status: string): Locator {
  return statusRow(screen, status).getByTestId('tracker-status-group');
}

function backlogRow(pane: Locator, issueId: string): Locator {
  return pane.locator(`[data-list="backlog"] [data-testid="issues-backlog-row"][data-issue="${issueId}"]`);
}

/** The active sprint's header on the Backlog screen. */
function activeSprint(backlog: Locator): Locator {
  return backlog.locator('[data-testid="issues-backlog-section"][data-list="sprint:1"]');
}

/** The active sprint's numbers on the Backlog screen. */
function sprintNumbers(backlog: Locator): Locator {
  return activeSprint(backlog).getByTestId('issues-backlog-numbers');
}

/** A row of the active sprint on the Backlog screen. */
function sprintRow(backlog: Locator, issueId: string): Locator {
  return backlog.locator(`[data-list="sprint:1"] [data-testid="issues-backlog-row"][data-issue="${issueId}"]`);
}

/** The status mark drawn in `within`. */
function markOf(within: Locator): Locator {
  return within.locator('[data-status-group]').first();
}

/** A Board column's mark: its header's, or its tab's on compact. */
async function expectColumnMark(pane: Locator, name: string, group: string, compact: boolean): Promise<void> {
  const head = compact ? pane.getByTestId('issues-column-tab').filter({ hasText: name }) : pane.locator(`[data-testid="issues-column"][data-column="${name}"] header`);
  await expect(markOf(head)).toHaveAttribute('data-status-group', group, { timeout: 15_000 });
}

/** A Board column's count: its header's, or its tab's on compact. */
async function expectColumnCount(pane: Locator, name: string, count: string, compact: boolean): Promise<void> {
  if (compact) {
    await expect(pane.getByTestId('issues-column-tab').filter({ hasText: name })).toHaveText(new RegExp(`${name}\\s*${count}$`), { timeout: 15_000 });
  } else {
    await expect(pane.locator(`[data-testid="issues-column"][data-column="${name}"]`).getByTestId('issues-column-count')).toHaveText(count, { timeout: 15_000 });
  }
}

export function card(pane: Locator, issueId: string): Locator {
  return pane.locator(`[data-testid="issues-card"][data-issue="${issueId}"]`);
}

async function openBoardScreen(page: Page, pane: Locator, compact: boolean): Promise<void> {
  const picker = pane.getByTestId('issues-tabs-picker');
  if (await picker.isVisible()) {
    await press(picker, compact);
    await press(page.getByTestId('issues-tab-option').filter({ hasText: /^\s*Board\s*$/ }), compact);
  } else {
    await press(pane.locator('[data-testid="issues-tab"][data-screen="board"]'), compact);
  }
}

export async function openTicket(pane: Locator, issueId: string, compact: boolean): Promise<Locator> {
  await expect(card(pane, issueId)).toBeVisible({ timeout: 30_000 });
  await press(card(pane, issueId).getByTestId('issues-card-open'), compact);
  const ticket = pane.getByTestId('ticket-screen');
  await expect(ticket).toHaveAttribute('data-issue', issueId);
  return ticket;
}

export async function closeTicket(pane: Locator, compact: boolean): Promise<void> {
  await press(pane.getByTestId(compact ? 'issues-back' : 'ticket-close'), compact);
  await expect(pane.getByTestId('ticket-screen')).toHaveCount(0);
}

/** The rail of the ticket's fields; a narrow ticket opens it from Details,
 * which a compact ticket shows once its detail is read. */
export async function openRail(ticket: Locator, compact: boolean): Promise<Locator> {
  const details = ticket.getByTestId('ticket-details');
  if (compact) await expect(details).toBeVisible({ timeout: 15_000 });
  if (await details.isVisible() && (await details.getAttribute('aria-expanded')) !== 'true') await press(details, compact);
  const rail = ticket.getByTestId('ticket-rail');
  await expect(rail).toBeVisible();
  return rail;
}

/** Closes the Details rail a narrow ticket opened over its content. */
export async function closeRail(ticket: Locator, compact: boolean): Promise<void> {
  const close = ticket.getByTestId('ticket-details-close');
  if (await close.isVisible()) await press(close, compact);
}

/** The rail's rows above its "more fields" control: the card's and the
 * pinned ones. Opens a narrow ticket's rail, and leaves it open. */
export async function railFields(ticket: Locator, compact: boolean): Promise<string[]> {
  await openRail(ticket, compact);
  return ticket.getByTestId('ticket-fields').evaluate((rail) => {
    const out: string[] = [];
    for (const el of rail.children) {
      const testId = (el as HTMLElement).dataset.testid;
      if (testId === 'ticket-more-fields') break;
      if (testId === 'ticket-field') out.push((el as HTMLElement).dataset.field ?? '');
    }
    return out;
  });
}

/** The sections the ticket shows by its pins, in order: those above the
 * line that names the folded ones, which may be open below it. */
async function shownSections(ticket: Locator): Promise<string[]> {
  return ticket.getByTestId('ticket-content').evaluate((content) => {
    const fold = content.querySelector('[data-testid="ticket-more-sections"]');
    return [...content.querySelectorAll<HTMLElement>('[data-testid="ticket-section"], [data-testid="ticket-links"], [data-testid="ticket-attachments"]')]
      .filter((el) => !fold || (el.compareDocumentPosition(fold) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0)
      .map((el) => el.dataset.field ?? el.dataset.testid ?? '');
  });
}

export function sectionPin(ticket: Locator, field: string): Locator {
  return ticket.locator(`[data-testid="ticket-section"][data-field="${field}"] [data-testid="ticket-section-pin"]`);
}

export async function openBoard(harness: HarnessApp, page: Page, name: string, compact: boolean, setup: ExtraJira = SETUP): Promise<{ board: ConnectedBoard; pane: Locator; file: string }> {
  const board = await connectFakeJiraBoard(harness, name, setup);
  const file = await setupFile(harness, board.projectId);
  await harness.open(page);
  const pane = await openIssuesPane(page, compact);
  await expect(card(pane, '10101')).toBeVisible({ timeout: 30_000 });
  return { board, pane, file };
}

export function registerIssuesSetupFlow({ compact }: SetupSurface): void {
  test('a status put in another group saves at once, and the Backlog, its sprint\'s done count and the Board follow it without a reload until it is put back', async ({ harness, page }) => {
    const { board, pane, file } = await openBoard(harness, page, 'setup-groups', compact);
    const backlog = await openBacklog(page, pane, compact);
    await expect(backlogRow(backlog, '10103')).toBeVisible({ timeout: 15_000 });
    await expect(sprintNumbers(backlog), 'WEB-6 is done, by Jira\'s group').toHaveText('1 of 5 tickets done');

    const screen = await openStatusGroups(page, compact);
    await expect(screen.getByTestId('tracker-status-name')).toHaveText(['To Do', 'In Progress', 'Done'], { timeout: 15_000 });
    await expect(statusRow(screen, '10001').getByTestId('tracker-status-own')).toHaveText('Jira: In progress');
    await groupOf(screen, '10000').selectOption('done');
    await expect(statusRow(screen, '10000').getByTestId('tracker-status-changed')).toBeVisible();
    await groupOf(screen, '10001').selectOption('left_out');
    await expect(statusRow(screen, '10001').getByTestId('tracker-status-changed')).toBeVisible();
    await expect(groupOf(screen, '10001')).toHaveValue('left_out');
    expect(await storedSetup(file)).toEqual({ status_groups: { version: 1, sites: [{ tracker: 'jira', site: board.site, statuses: { 10000: 'done', 10001: 'left_out' } }] } });

    // To Do counts done, so WEB-3 is in no backlog and WEB-2 is done in
    // the sprint, whose In Progress tickets count for nothing.
    await closeSettings(page, compact);
    await openIssuesPane(page, compact);
    await expect(backlogRow(backlog, '10103')).toHaveCount(0, { timeout: 15_000 });
    await expect(sprintNumbers(backlog)).toHaveText('2 of 2 tickets done', { timeout: 15_000 });
    if (!compact) {
      // The marks draw the project's groups: WEB-2's To Do as Done, and
      // WEB-1's In Progress, left out of counts, as Jira's. A Backlog
      // row's mark is on the desktop alone.
      const fold = activeSprint(backlog).getByTestId('issues-backlog-fold');
      if ((await fold.getAttribute('aria-expanded')) !== 'true') await press(fold, compact);
      await expect(markOf(sprintRow(backlog, '10102'))).toHaveAttribute('data-status-group', 'done');
      await expect(markOf(sprintRow(backlog, '10101'))).toHaveAttribute('data-status-group', 'in_progress');
    }
    // The Complete sprint form counts by Jira's groups, not the project's.
    await press(activeSprint(backlog).getByTestId('issues-backlog-sprint-complete'), compact);
    const complete = page.getByTestId('sprint-complete-form');
    await expect(complete.getByTestId('sprint-complete-summary')).toHaveText('1 of 5 tickets done. 4 of the tickets this board shows are not in a finished status.');
    await press(complete.getByTestId('sprint-complete-cancel'), compact);
    await expect(complete).toHaveCount(0);
    // In Progress is left out of counts: its cards show, and its count is none of them.
    await openBoardScreen(page, pane, compact);
    await expect(card(pane, PINNED_ID)).toBeVisible({ timeout: 15_000 });
    await expectColumnCount(pane, 'In Progress', '0', compact);
    await expectColumnMark(pane, 'To Do', 'done', compact);
    await expectColumnMark(pane, 'In Progress', 'in_progress', compact);
    if (compact) {
      // A card's mark is on compact alone.
      await expect(markOf(card(pane, PINNED_ID))).toHaveAttribute('data-status-group', 'in_progress');
      await press(pane.getByTestId('issues-column-tab').filter({ hasText: 'To Do' }), true);
      await expect(markOf(card(pane, '10102'))).toHaveAttribute('data-status-group', 'done');
    }
    const ticket = await openTicket(pane, '10102', compact);
    await expect(markOf(ticket.locator('[data-testid="ticket-field"][data-field="status"]'))).toHaveAttribute('data-status-group', 'done');
    await closeTicket(pane, compact);

    // Put back: the tracker's group again, and the file holds nothing.
    const again = await openStatusGroups(page, compact);
    await groupOf(again, '10000').selectOption('');
    await expect(statusRow(again, '10000').getByTestId('tracker-status-changed')).toHaveCount(0);
    await groupOf(again, '10001').selectOption('');
    await expect(statusRow(again, '10001').getByTestId('tracker-status-changed')).toHaveCount(0);
    await expect.poll(async () => readFile(file, 'utf8').then(() => 'present', () => 'removed')).toBe('removed');
    await closeSettings(page, compact);
    await openIssuesPane(page, compact);
    await expectColumnCount(pane, 'In Progress', '3', compact);
    await expectColumnMark(pane, 'To Do', 'todo', compact);
    const backlogAgain = await openBacklog(page, pane, compact);
    await expect(backlogRow(backlogAgain, '10103')).toBeVisible({ timeout: 15_000 });
    await expect(sprintNumbers(backlogAgain)).toHaveText('1 of 5 tickets done', { timeout: 15_000 });

    // Done counted as In progress: the sprint has nothing done.
    const third = await openStatusGroups(page, compact);
    await groupOf(third, '10002').selectOption('in_progress');
    await expect(statusRow(third, '10002').getByTestId('tracker-status-changed')).toBeVisible();
    await closeSettings(page, compact);
    await openIssuesPane(page, compact);
    await expect(sprintNumbers(backlogAgain)).toHaveText('0 of 5 tickets done', { timeout: 15_000 });
  });

  test('Complete sprint counts finished the tickets of the board\'s last column that has statuses, and the completion moves the rest', async ({ harness, page }) => {
    // Done sits in the first column beside To Do, so only In Progress,
    // the last column, is finished: WEB-1 and WEB-7 stay, WEB-2 and WEB-6
    // (Done by Jira's group) move.
    await connectFakeJiraBoard(harness, 'setup-complete', {
      projects: [], boards: [],
      columns: [{ name: 'To Do', statuses: ['10000', '10002'] }, { name: 'In Progress', statuses: ['10001'] }],
      issues: [
        { id: DONE_ID, key: 'WEB-6', project: 'WEB', type: '10002', status: '10002', summary: 'Finished ticket', sprints: [1] },
        { id: '10107', key: 'WEB-7', project: 'WEB', type: '10002', status: '10001', summary: 'Ticket in the last column', sprints: [1] },
      ],
    });
    await harness.open(page);
    const pane = await openIssuesPane(page, compact);
    const backlog = await openBacklog(page, pane, compact);
    await expect(sprintNumbers(backlog), 'the header counts WEB-6 done by its group').toHaveText('1 of 4 tickets done', { timeout: 30_000 });
    await press(activeSprint(backlog).getByTestId('issues-backlog-sprint-complete'), compact);
    const complete = page.getByTestId('sprint-complete-form');
    await expect(complete.getByTestId('sprint-complete-summary')).toHaveText('2 of 4 tickets done. 2 of the tickets this board shows are not in a finished status.');
    await expect(complete.getByTestId('sprint-complete-into')).toHaveValue('');
    await press(complete.getByTestId('sprint-complete-submit'), compact);
    await expect(complete).toHaveCount(0, { timeout: 15_000 });
    const moved = (await trackerRequests(harness, 'POST rest/agile/1.0/backlog/issue')).map((r) => r.body as { issues: string[] });
    expect(moved.flatMap((b) => b.issues).sort(), 'Jira moved the two the form counted unfinished').toEqual(['10102', DONE_ID]);
    await expect(backlogRow(backlog, '10102')).toBeVisible({ timeout: 15_000 });
  });

  test('a save that lost to a hand edit of the file says so in words, shows what is stored, and keeps the hand edit', async ({ harness, page }) => {
    const { board, file } = await openBoard(harness, page, 'setup-refused', compact);
    const screen = await openStatusGroups(page, compact);
    await expect(groupOf(screen, '10002')).toHaveValue('', { timeout: 15_000 });

    await writeSetup(file, groupsFile(board.site, { 10002: 'canceled' }));
    await groupOf(screen, '10000').selectOption('backlog');
    await expect(screen.getByTestId('settings-tracker-statuses-save-note')).toHaveText(
      "This change was not saved: the project's tracker setup changed after it was read; it is shown as it is now. Make the change again.",
    );
    await expect(groupOf(screen, '10000')).toHaveValue('');
    await expect(groupOf(screen, '10002')).toHaveValue('canceled');
    expect(await storedSetup(file)).toEqual(JSON.parse(groupsFile(board.site, { 10002: 'canceled' })));

    // Made again over what is stored: both are kept.
    await groupOf(screen, '10000').selectOption('backlog');
    await expect(screen.getByTestId('settings-tracker-statuses-save-note')).toHaveCount(0);
    await expect(statusRow(screen, '10000').getByTestId('tracker-status-changed')).toBeVisible();
    expect(await storedSetup(file)).toEqual({ status_groups: { version: 1, sites: [{ tracker: 'jira', site: board.site, statuses: { 10000: 'backlog', 10002: 'canceled' } }] } });
  });

  test('a setup file that cannot be read is said with its path, and one a newer build saved is said so, with no select', async ({ harness, page }) => {
    const board = await connectFakeJiraBoard(harness, 'setup-unreadable', SETUP);
    const file = await setupFile(harness, board.projectId);
    await writeSetup(file, '{"status_groups": ');
    await harness.open(page);
    const screen = await openStatusGroups(page, compact);
    const error = screen.getByTestId('settings-tracker-statuses-error');
    await expect(error).toContainText(file, { timeout: 15_000 });
    await expect(screen.getByTestId('tracker-status-group')).toHaveCount(0);

    await writeSetup(file, JSON.stringify({ status_groups: { version: 2, rules: ['any'] } }));
    await press(screen.getByTestId('settings-tracker-statuses-retry'), compact);
    await expect(screen.getByTestId('settings-tracker-statuses-newer')).toContainText('A newer version of Agent Overflow saved this project\'s status groups.');
    await expect(error).toHaveCount(0);
    await expect(screen.getByTestId('tracker-status-group')).toHaveCount(0);
  });

  test('a board not read on this computer yet is named, and its statuses are listed once it has been, when the page shows again', async ({ harness, page }) => {
    await connectFakeJiraBoard(harness, 'setup-unread', SETUP);
    await harness.open(page);
    const screen = await openStatusGroups(page, compact);
    await expect(screen.getByTestId('tracker-status-unread')).toContainText('WEB board has not been read on this computer yet. Open it once in the Issues pane to list its statuses.', { timeout: 15_000 });
    await expect(screen.getByTestId('tracker-status-row')).toHaveCount(0);
    await closeSettings(page, compact);

    const pane = await openIssuesPane(page, compact);
    await expect(card(pane, '10101')).toBeVisible({ timeout: 30_000 });
    const again = await openStatusGroups(page, compact);
    await expect(again.getByTestId('tracker-status-name')).toHaveText(['To Do', 'In Progress', 'Done'], { timeout: 15_000 });
    await expect(again.getByTestId('tracker-status-unread')).toHaveCount(0);
  });

  test('a field pinned from its row and a section from its header lay out every ticket of the project, kept in the file and after a reload', async ({ harness, page }) => {
    const { board, pane, file } = await openBoard(harness, page, 'setup-pins', compact);
    let ticket = await openTicket(pane, PINNED_ID, compact);
    await expect(sectionPin(ticket, 'description')).toBeAttached({ timeout: 15_000 });
    expect(await shownSections(ticket)).toEqual(['description', 'ticket-links', 'ticket-attachments']);
    await expect(ticket.getByTestId('ticket-more-sections')).toHaveText('1 more section: Root cause');

    // Effort, pinned from its row behind "more fields".
    const rail = await openRail(ticket, compact);
    await press(rail.getByTestId('ticket-more-fields'), compact);
    const effort = rail.locator(`[data-testid="ticket-field"][data-field="${EFFORT}"]`);
    if (!compact) await effort.hover();
    await press(effort.getByTestId('ticket-field-pin'), compact);
    await expect.poll(() => railFields(ticket, compact)).toContain(EFFORT);
    await closeRail(ticket, compact);

    // Root cause, pinned from its header; the description unpinned from its own.
    await press(ticket.getByTestId('ticket-more-sections'), compact);
    if (!compact) await ticket.locator(`[data-testid="ticket-section"][data-field="${ROOT_CAUSE}"] header`).hover();
    await press(sectionPin(ticket, ROOT_CAUSE), compact);
    await expect(ticket.getByTestId('ticket-more-sections')).toHaveCount(0);
    await press(sectionPin(ticket, 'description'), compact);
    await expect(ticket.getByTestId('ticket-more-sections')).toHaveText('1 more section: Description');
    expect(await shownSections(ticket)).toEqual(['ticket-links', 'ticket-attachments', ROOT_CAUSE]);
    expect(await storedSetup(file)).toEqual({
      ticket_pins: { version: 1, sites: [{ tracker: 'jira', site: board.site, fields: [EFFORT], sections: ['links', 'attachments', ROOT_CAUSE] }] },
    });

    // Another ticket of the project lays out by the same pins.
    await closeTicket(pane, compact);
    ticket = await openTicket(pane, OTHER_ID, compact);
    await expect.poll(() => railFields(ticket, compact), { timeout: 15_000 }).toContain(EFFORT);
    await closeRail(ticket, compact);
    await expect(ticket.getByTestId('ticket-more-sections')).toHaveText('1 more section: Description');
    expect(await shownSections(ticket)).toEqual(['ticket-links', 'ticket-attachments', ROOT_CAUSE]);

    // And after a reload, as the file holds them.
    await page.reload();
    const reloaded = await openTicket(await openIssuesPane(page, compact), OTHER_ID, compact);
    await expect(reloaded.getByTestId('ticket-more-sections')).toHaveText('1 more section: Description', { timeout: 15_000 });
    expect(await shownSections(reloaded)).toEqual(['ticket-links', 'ticket-attachments', ROOT_CAUSE]);
    await expect.poll(() => railFields(reloaded, compact)).toContain(EFFORT);
  });

  test('a Linear ticket\'s sections take a pin and its fields none, and unpinning them all stores none', async ({ harness, page }) => {
    const issue = '00000000-0000-4000-8000-000000000101';
    const linear = await connectFakeLinearBoard(harness, 'setup-linear', [
      { id: issue, team: LINEAR_TEAM, number: 1, title: 'Linear ticket', description: 'Some text', state: '00000000-0000-4000-8000-000000000021', dueDate: '2026-12-01' },
    ]);
    await harness.open(page);
    const pane = await openIssuesPane(page, compact);
    const ticket = await openTicket(pane, issue, compact);
    await expect(sectionPin(ticket, 'description')).toBeAttached({ timeout: 15_000 });
    const rail = await openRail(ticket, compact);
    const more = rail.getByTestId('ticket-more-fields');
    if (await more.isVisible()) await press(more, compact);
    await expect(rail.getByTestId('ticket-field').filter({ hasText: 'Due date' })).toBeVisible();
    await expect(ticket.getByTestId('ticket-field-pin')).toHaveCount(0);
    await closeRail(ticket, compact);

    // Unpinned, Description and Linked tickets fold, and the file holds
    // no section this ticket has no place for.
    const file = await setupFile(harness, linear.projectId);
    await press(sectionPin(ticket, 'description'), compact);
    await expect(ticket.getByTestId('ticket-more-sections')).toHaveText('1 more section: Description');
    await press(ticket.getByTestId('ticket-links-pin'), compact);
    await expect(ticket.getByTestId('ticket-more-sections')).toHaveText('2 more sections: Description, Linked tickets');
    expect(await shownSections(ticket)).toEqual([]);
    expect(await storedSetup(file)).toEqual({ ticket_pins: { version: 1, sites: [{ tracker: 'linear', site: 'example', sections: [] }] } });
  });

  if (!compact) {
    test('a connected browser\'s pick is saved, and this window\'s page follows it', async ({ browser, page }) => {
      test.skip(nonLoopbackIPv4() === null, 'requires a LAN interface for a connected browser');
      test.setTimeout(180_000);
      const harness = await launchHarness();
      const context = await browser.newContext();
      try {
        await harness.rpc('SetNetworkSettings', { bindAll: true });
        const board = await connectFakeJiraBoard(harness, 'setup-connected', SETUP);
        const file = await setupFile(harness, board.projectId);
        await harness.open(page);
        const pane = await openIssuesPane(page, false);
        await expect(card(pane, '10101')).toBeVisible({ timeout: 30_000 });
        const here = await openStatusGroups(page, false);
        await expect(groupOf(here, '10000')).toHaveValue('', { timeout: 15_000 });

        const device = await context.newPage();
        const code = await redeemOnScreen(device, await mintInvite(harness, 'full'), 'Setup browser');
        await confirmOnHost(harness, code);
        const there = await openStatusGroups(device, false);
        await groupOf(there, '10000').selectOption('canceled');
        await expect(statusRow(there, '10000').getByTestId('tracker-status-changed')).toBeVisible({ timeout: 15_000 });
        expect(await storedSetup(file)).toEqual(JSON.parse(groupsFile(board.site, { 10000: 'canceled' })));
        await expect(groupOf(here, '10000')).toHaveValue('canceled', { timeout: 15_000 });
      } finally {
        await context.close();
        await harness.close();
      }
    });

    test('a paired computer\'s project is set on that computer, and its Backlog follows', async ({ page }) => {
      test.setTimeout(240_000);
      const root = await mkdtemp(join(tmpdir(), 'ao-issues-setup-'));
      let home: HarnessApp | undefined;
      let remote: HarnessApp | undefined;
      try {
        [home, remote] = await startTogether(launchHarness(), launchHarness({ dataDir: join(root, 'state') }));
        await pairOwnDevice(home, remote, 'Laptop');
        const board = await connectFakeJiraBoard(remote, 'setup-remote', SETUP);
        const file = await setupFile(remote, board.projectId);
        await home.open(page);
        const pane = await openIssuesPane(page, false);
        await expect(card(pane, '10101')).toBeVisible({ timeout: 60_000 });
        const backlog = await openBacklog(page, pane, false);
        await expect(backlogRow(backlog, '10103')).toBeVisible({ timeout: 15_000 });

        await page.getByTestId('sidebar-settings-button').click();
        await page.getByRole('tablist', { name: 'Settings Sections' }).getByRole('tab', { name: 'Status groups', exact: true }).click();
        await page.getByRole('combobox', { name: 'Computer', exact: true }).selectOption({ label: 'Laptop' });
        const screen = page.getByTestId('settings-tracker-statuses');
        await groupOf(screen, '10000').selectOption('done');
        await expect(statusRow(screen, '10000').getByTestId('tracker-status-changed')).toBeVisible({ timeout: 15_000 });
        expect(await storedSetup(file)).toEqual(JSON.parse(groupsFile(board.site, { 10000: 'done' })));
        await closeSettings(page, false);
        await expect(backlogRow(backlog, '10103')).toHaveCount(0, { timeout: 15_000 });
      } finally {
        await remote?.close();
        await home?.close();
        await rm(root, { recursive: true, force: true });
      }
    });
  }
}
