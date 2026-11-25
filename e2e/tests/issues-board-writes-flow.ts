// Changing tickets from the Issues pane's Board screen (docs/specs/issues.md,
// "Board" and "Compact layout"), shared by the desktop and compact
// projects, through the shipped UI, the real backend and the fake Jira.
//
// Covered on both: the card's menu shows Open, Move to…, Assignee… and
// Copy key at once and the rest behind Show more, and a changed shown set
// outlasts a reload; Move to… lists the columns the ticket's moves reach
// and the others disabled, and moves the card; a refused move leaves the
// card where Jira holds it with the reason under it. On desktop: the
// type, priority, points and assignee change from the card's marks, each
// reaching Jira, while the same ticket open beside the board shows the
// same write pending and then its value; a carried card moves to another
// column by the one move that reaches it, a column no move reaches says so
// while the card is over it and refuses the drop, a column two moves reach
// asks which, and a move that takes fields opens the ticket with its form;
// a drop within the card's own column ranks it in Jira. On compact: the
// fields change from the menu, and the status mark opens Move to….

import type { Locator, Page } from '@playwright/test';

import { expect, test } from './fixtures.js';
import { longPress } from './touch-helpers.js';
import { JIRA_SITE, connectFakeJiraBoard, openIssuesPane, press, trackerRequests, type ExtraJira } from './tracker-helpers.js';

export interface BoardWritesSurface {
  compact: boolean;
}

const JIRA = { tracker: 'jira', site: JIRA_SITE };
const MOVE_ROUTE = 'POST rest/api/3/issue/{issue}/transitions';
const EDIT_ROUTE = 'PUT rest/api/3/issue/{issue}';
const RANK_ROUTE = 'PUT rest/agile/1.0/issue/rank';

// The board's sprint holds the seed's WEB-1 (In Progress) and WEB-2 (To
// Do), and WEB-5 (To Do, 3 points), WEB-6 and WEB-7 (In Progress). From
// To Do no move reaches Done; from In Progress two do, Done and Resolve,
// whose screen takes the resolution.
const BOARD: ExtraJira = {
  projects: [],
  boards: [],
  estimation: 'customfield_10016',
  assignable: ['acc-user1'],
  fields: [
    { id: 'customfield_10016', name: 'Story Points', schema: { type: 'number', custom: 'com.atlassian.jira.plugin.system.customfieldtypes:float', customId: 10016 } },
  ],
  issueTypes: [
    { id: '10000', name: 'Epic', hierarchyLevel: 1 }, { id: '10001', name: 'Story' }, { id: '10002', name: 'Task' }, { id: '10003', name: 'Bug' },
  ].map((t) => ({
    ...t,
    screen: [{ field: 'summary', required: true }, ...['issuetype', 'description', 'assignee', 'priority', 'labels', 'customfield_10016', 'customfield_10020'].map((field) => ({ field }))],
  })),
  resolutions: [{ id: '10000', name: 'Done' }, { id: '10001', name: 'Fixed' }],
  transitions: [
    { id: '11', name: 'To Do', to: '10000' },
    { id: '21', name: 'In Progress', to: '10001' },
    { id: '31', name: 'Done', to: '10002', from: ['10001'] },
    { id: '41', name: 'Resolve', to: '10002', from: ['10001'], screen: [{ field: 'resolution', required: true }] },
  ],
  issues: [
    { id: '10105', key: 'WEB-5', project: 'WEB', type: '10002', status: '10000', summary: 'Changed from its card', sprints: [1], values: { customfield_10016: 3 } },
    { id: '10106', key: 'WEB-6', project: 'WEB', type: '10002', status: '10001', summary: 'Resolved from the board', sprints: [1] },
    { id: '10107', key: 'WEB-7', project: 'WEB', type: '10002', status: '10001', summary: 'Refused a move', sprints: [1] },
  ],
};

function card(pane: Locator, issueId: string): Locator {
  return pane.locator(`[data-testid="issues-card"][data-issue="${issueId}"]`);
}

/** A column's card IDs, in the order shown. */
async function columnCards(column: Locator): Promise<string[]> {
  return column.getByTestId('issues-card').evaluateAll((els) => els.map((el) => (el as HTMLElement).dataset.issue ?? ''));
}

/** Opens a card's menu: a right click, a long press on the phone. */
async function cardMenu(page: Page, pane: Locator, issueId: string, key: string, compact: boolean): Promise<Locator> {
  const menu = page.getByRole('menu', { name: `Card actions for ${key}` });
  const target = card(pane, issueId).getByTestId('issues-card-open');
  if (compact) await longPress(page, target, menu);
  else await target.click({ button: 'right' });
  await expect(menu).toBeVisible();
  return menu;
}

/** The labels of the rows a menu shows. */
async function rows(menu: Locator): Promise<string[]> {
  return menu.getByRole('menuitem').evaluateAll((els) => els.map((el) => el.querySelector('span')?.textContent?.trim() ?? el.textContent?.trim() ?? ''));
}

/** Shows column `name` on compact (the strip picks it); a no-op on desktop. */
async function showColumn(pane: Locator, name: string, compact: boolean): Promise<Locator> {
  if (compact) {
    await press(pane.getByTestId('issues-column-tab').filter({ hasText: name }), compact);
    return pane.getByTestId('issues-column');
  }
  return pane.locator(`[data-testid="issues-column"][data-column="${name}"]`);
}

/**
 * Carries `from` with the mouse to (x, y), pausing over each point of
 * `via` for `hold` to hold while the card is there, and lets go.
 */
async function carry(page: Page, from: Locator, to: { x: number; y: number }, hold?: () => Promise<void>): Promise<void> {
  const box = (await from.getByTestId('issues-card-title').boundingBox())!;
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  await page.mouse.down();
  await page.mouse.move(box.x + box.width / 2 + 12, box.y + box.height / 2 + 4, { steps: 3 });
  await expect(page.getByTestId('issues-card-carried')).toHaveCount(1);
  await page.mouse.move(to.x, to.y, { steps: 12 });
  if (hold) await hold();
  await page.mouse.up();
}

async function center(target: Locator): Promise<{ x: number; y: number }> {
  const box = (await target.boundingBox())!;
  return { x: box.x + box.width / 2, y: box.y + box.height / 2 };
}

export function registerBoardWritesFlow({ compact }: BoardWritesSurface): void {
  test('the card menu shows a chosen set at once, the rest behind Show more, and moves a card by Move to…', async ({ harness, page }) => {
    test.setTimeout(120_000);
    await connectFakeJiraBoard(harness, compact ? 'compact-issues-board-menu' : 'issues-board-menu', BOARD);
    await harness.open(page);
    let pane = await openIssuesPane(page, compact);
    let todo = await showColumn(pane, 'To Do', compact);
    await expect(card(todo, '10102')).toBeVisible({ timeout: 30_000 });

    // At once: the four defaults, then Show more.
    let menu = await cardMenu(page, pane, '10102', 'WEB-2', compact);
    expect(await rows(menu)).toEqual(['Open', 'Move to…', 'Assignee…', 'Copy key', 'Show more']);
    await press(menu.getByTestId('action-menu-more'), compact);
    expect(await rows(menu)).toEqual([
      'Open', 'Move to…', 'Assignee…', 'Copy key',
      'Open in the tracker', 'Move to sprint…', 'Priority…', 'Points…', 'Type…', 'Copy link',
      'Choose what shows first…',
    ]);
    // Copy link joins the set shown at once; Assignee… leaves it.
    await press(menu.getByTestId('action-menu-choose'), compact);
    await press(menu.getByTestId('action-menu-choose-item').filter({ hasText: 'Copy link' }), compact);
    await press(menu.getByTestId('action-menu-choose-item').filter({ hasText: 'Assignee…' }), compact);
    await press(menu.getByTestId('action-menu-choose-done'), compact);
    await page.keyboard.press('Escape');
    await expect(menu).toHaveCount(0);

    // Kept across a reload of the window.
    await page.reload();
    pane = page.getByTestId('issues-pane');
    if (!(await pane.isVisible())) pane = await openIssuesPane(page, compact);
    todo = await showColumn(pane, 'To Do', compact);
    await expect(card(todo, '10102')).toBeVisible({ timeout: 30_000 });
    menu = await cardMenu(page, pane, '10102', 'WEB-2', compact);
    expect(await rows(menu)).toEqual(['Open', 'Move to…', 'Copy key', 'Copy link', 'Show more']);

    // Move to… reads the moves on opening its page: from To Do, In
    // Progress is reached and Done is not.
    const movesBefore = (await trackerRequests(harness, 'GET rest/api/3/issue/{issue}/transitions')).length;
    await press(menu.getByTestId('issues-card-menu-move'), compact);
    const targets = menu.getByTestId('issues-card-move-column');
    await expect(targets).toHaveCount(2);
    await expect(targets.filter({ hasText: 'Done' })).toHaveAttribute('aria-disabled', 'true');
    await expect(targets.filter({ hasText: 'Done' })).toContainText('No move reaches it');
    expect((await trackerRequests(harness, 'GET rest/api/3/issue/{issue}/transitions')).length - movesBefore).toBe(1);
    await press(targets.filter({ hasText: 'In Progress' }), compact);
    await expect(menu).toHaveCount(0);
    const progress = await showColumn(pane, 'In Progress', compact);
    await expect(card(progress, '10102')).toBeVisible({ timeout: 15_000 });
    await expect(card(progress, '10102').getByTestId('ticket-write-pending')).toHaveCount(0, { timeout: 15_000 });
    const moves = await trackerRequests(harness, MOVE_ROUTE, ['WEB-2', '10102']);
    expect(moves.map((r) => r.body)).toEqual([{ transition: { id: '21' } }]);

    // A move Jira refuses: the card shows where Jira holds it, the reason
    // under it.
    await harness.rpc('HarnessTrackerFault', {
      origin: JIRA, route: MOVE_ROUTE, kind: 'refuse', times: 1,
      status: 400, body: { errorMessages: ['WEB-7 cannot leave In Progress now.'] },
    });
    menu = await cardMenu(page, pane, '10107', 'WEB-7', compact);
    await press(menu.getByTestId('issues-card-menu-move'), compact);
    await press(menu.getByTestId('issues-card-move-column').filter({ hasText: 'To Do' }), compact);
    const note = card(progress, '10107').getByTestId('ticket-write-outcome');
    await expect(note).toContainText('WEB-7 cannot leave In Progress now.', { timeout: 15_000 });
    await expect(note).toHaveAttribute('data-kind', 'refused');
    expect(await columnCards(progress)).toContain('10107');
  });

  if (compact) {
    test('on the phone a card\'s fields change from its menu and its status mark opens Move to…', async ({ harness, page }) => {
      test.setTimeout(120_000);
      await connectFakeJiraBoard(harness, 'compact-issues-board-fields', BOARD);
      await harness.open(page);
      const pane = await openIssuesPane(page, true);
      const todo = await showColumn(pane, 'To Do', true);
      const changed = card(todo, '10105');
      await expect(changed).toBeVisible({ timeout: 30_000 });
      const items = page.getByTestId('ticket-picker-item');
      const fromMenu = async (row: string) => {
        const menu = await cardMenu(page, pane, '10105', 'WEB-5', true);
        await press(menu.getByTestId('action-menu-more'), true);
        await press(menu.getByTestId(row), true);
        await expect(menu).toHaveCount(0);
      };

      await fromMenu('issues-card-menu-priority');
      await press(items.filter({ hasText: 'High' }).filter({ hasNotText: 'Highest' }), true);
      await expect(changed.getByTestId('issues-card-priority')).toHaveAttribute('title', 'High priority', { timeout: 15_000 });
      await fromMenu('issues-card-menu-type');
      await press(items.filter({ hasText: 'Bug' }), true);
      await expect(changed.getByTestId('issues-card-type')).toHaveAttribute('title', 'Bug', { timeout: 15_000 });
      await fromMenu('issues-card-menu-assignee');
      await page.getByTestId('ticket-picker-find').pressSequentially('User');
      await press(items.filter({ hasText: 'User 1' }), true);
      await expect(changed.getByTestId('issues-card-assignee')).toBeVisible({ timeout: 15_000 });
      await fromMenu('issues-card-menu-points');
      const typed = page.getByTestId('issues-card-typed').getByTestId('ticket-field-input');
      await typed.fill('8');
      await typed.press('Enter');
      await expect(changed.getByTestId('issues-card-estimate')).toHaveText('8', { timeout: 15_000 });
      const edits = (await trackerRequests(harness, EDIT_ROUTE, ['WEB-5', '10105'])).map((r) => r.body);
      expect(edits).toEqual(expect.arrayContaining([
        { fields: { priority: { id: '2' } } }, { fields: { issuetype: { id: '10003' } } }, { fields: { assignee: { accountId: 'acc-user1' } } },
      ]));

      // The status mark opens Move to… at once.
      await press(changed.getByTestId('issues-card-status'), true);
      const menu = page.getByRole('menu', { name: 'Card actions for WEB-5' });
      await expect(menu.getByTestId('action-menu')).toHaveAttribute('data-mode', 'page');
      await press(menu.getByTestId('issues-card-move-column').filter({ hasText: 'In Progress' }), true);
      const progress = await showColumn(pane, 'In Progress', true);
      await expect(card(progress, '10105')).toBeVisible({ timeout: 15_000 });
    });
  }

  if (!compact) {
    test('a card\'s marks change its fields, the ticket open beside showing the same write', async ({ harness, page }) => {
      test.setTimeout(120_000);
      await page.setViewportSize({ width: 1600, height: 900 });
      await connectFakeJiraBoard(harness, 'issues-board-fields', BOARD);
      await harness.open(page);
      const pane = await openIssuesPane(page, false);
      const changed = card(pane, '10105');
      await expect(changed).toBeVisible({ timeout: 30_000 });
      await changed.getByTestId('issues-card-open').click();
      const ticket = pane.getByTestId('ticket-screen');
      await expect(ticket).toHaveAttribute('data-issue', '10105');
      await ticket.getByRole('radio', { name: 'Beside' }).click();
      await expect(pane.getByTestId('issues-ticket')).toHaveAttribute('data-placement', 'beside');
      // The narrow row's field, else the rail's.
      const priorityField = ticket.locator('[data-testid="ticket-narrow-row"], [data-testid="ticket-rail"]').first().locator('[data-testid="ticket-field"][data-field="priority"]');
      await expect(priorityField.getByTestId('ticket-field-value')).not.toContainText('High');

      // The priority, held by Jira: the card's mark and the ticket's field
      // both wait on the one write, then both show it.
      const items = page.getByTestId('ticket-picker-item');
      const mark = changed.getByTestId('issues-card-priority-edit');
      await changed.hover();
      await mark.click();
      await harness.rpc('HarnessTrackerFault', { origin: JIRA, route: EDIT_ROUTE, kind: 'hold', times: 1 });
      await items.filter({ hasText: 'High' }).filter({ hasNotText: 'Highest' }).click();
      await expect.poll(async () => (await trackerRequests(harness, EDIT_ROUTE)).some((r) => r.held)).toBe(true);
      await expect(mark).toBeDisabled();
      await expect(changed.getByTestId('ticket-write-pending')).toBeVisible();
      const ticketPending = ticket.locator('[data-testid="ticket-write-pending"][data-target="field:priority"]');
      await expect(ticketPending).toBeVisible();
      await harness.rpc('HarnessTrackerRelease', { origin: JIRA, route: EDIT_ROUTE });
      await expect(mark).toHaveAttribute('aria-label', 'Priority: High', { timeout: 15_000 });
      await expect(priorityField.getByTestId('ticket-field-value')).toContainText('High');
      await expect(changed.getByTestId('ticket-write-pending')).toHaveCount(0);
      await expect(ticketPending).toHaveCount(0);

      // The type, the points and the assignee.
      await changed.getByTestId('issues-card-type-edit').click();
      await items.filter({ hasText: 'Bug' }).click();
      await expect(changed.getByTestId('issues-card-type-edit')).toHaveAttribute('aria-label', 'Type: Bug', { timeout: 15_000 });
      await changed.getByTestId('issues-card-estimate-edit').click();
      const typed = page.getByTestId('issues-card-typed').getByTestId('ticket-field-input');
      await typed.fill('5');
      await typed.press('Enter');
      await expect(changed.getByTestId('issues-card-estimate')).toHaveText('5', { timeout: 15_000 });
      await changed.hover();
      await changed.getByTestId('issues-card-assignee-edit').click();
      // Jira lists the people who may be assigned once a name is typed.
      await page.getByTestId('ticket-picker-find').pressSequentially('User');
      await items.filter({ hasText: 'User 1' }).click();
      await expect(changed.getByTestId('issues-card-assignee-edit')).toHaveAttribute('aria-label', 'Assignee: User 1', { timeout: 15_000 });
      const edits = (await trackerRequests(harness, EDIT_ROUTE, ['WEB-5', '10105'])).map((r) => r.body);
      expect(edits).toEqual([
        { fields: { priority: { id: '2' } } }, { fields: { issuetype: { id: '10003' } } }, { fields: { assignee: { accountId: 'acc-user1' } } },
      ]);
      expect((await trackerRequests(harness, 'PUT rest/agile/1.0/issue/{issue}/estimation')).map((r) => r.body)).toEqual([{ value: '5' }]);
    });

    test('a carried card moves, asks which move, opens a move\'s form, refuses an unreachable column, and ranks in its own', async ({ harness, page }) => {
      test.setTimeout(120_000);
      await page.setViewportSize({ width: 1600, height: 900 });
      await connectFakeJiraBoard(harness, 'issues-board-carry', BOARD);
      await harness.open(page);
      const pane = await openIssuesPane(page, false);
      const column = (name: string) => pane.locator(`[data-testid="issues-column"][data-column="${name}"]`);
      const todo = column('To Do');
      const progress = column('In Progress');
      const done = column('Done');
      await expect(card(todo, '10102')).toBeVisible({ timeout: 30_000 });
      const doneCards = done.getByTestId('issues-column-cards');

      // Over Done no move of WEB-2 reaches: said while it is there, and the
      // drop sends nothing.
      await carry(page, card(todo, '10102'), await center(doneCards), async () => {
        await expect(done.getByTestId('issues-column-drop-note')).toHaveText('No move of WEB-2 reaches Done', { timeout: 15_000 });
      });
      await expect(page.getByTestId('issues-card-carried')).toHaveCount(0);
      await expect(card(todo, '10102')).toBeVisible();
      expect(await trackerRequests(harness, MOVE_ROUTE)).toEqual([]);

      // To In Progress, by its one move; the header counts it there while
      // it is carried.
      await expect(progress.getByTestId('issues-column-count')).toHaveText('3 · 0 pts');
      await carry(page, card(todo, '10102'), await center(progress.getByTestId('issues-column-cards')), async () => {
        await expect(progress.getByTestId('issues-column-count')).toHaveText('4 · 0 pts');
      });
      await expect(card(progress, '10102')).toBeVisible({ timeout: 15_000 });
      await expect(card(progress, '10102').getByTestId('ticket-write-pending')).toHaveCount(0, { timeout: 15_000 });
      expect((await trackerRequests(harness, MOVE_ROUTE, ['WEB-2', '10102'])).map((r) => r.body)).toEqual([{ transition: { id: '21' } }]);

      // A rank: WEB-7, last of In Progress, dropped above its first card.
      const before = await columnCards(progress);
      expect(before.at(-1)).toBe('10107');
      const first = (await card(progress, before[0]).boundingBox())!;
      await carry(page, card(progress, '10107'), { x: first.x + first.width / 2, y: first.y + 6 }, async () => {
        await expect(progress.getByTestId('issues-column-slot')).toBeVisible();
      });
      await expect.poll(() => columnCards(progress), { timeout: 15_000 }).toEqual(['10107', ...before.slice(0, -1)]);
      const ranks = await trackerRequests(harness, RANK_ROUTE);
      expect(ranks.map((r) => r.body)).toEqual([expect.objectContaining({ issues: ['10107'], rankBeforeIssue: before[0] })]);

      // To Done, which two moves reach: which, at the drop; Resolve takes
      // fields, so the ticket opens with its form.
      await carry(page, card(progress, '10106'), await center(doneCards));
      const choice = page.getByRole('menu', { name: 'Move WEB-6 by' });
      await expect(choice.getByTestId('issues-card-drop-move')).toHaveText([/^\s*Done\s*$/, /^\s*Resolve\s+Asks for fields\s*$/]);
      await choice.getByTestId('issues-card-drop-move').filter({ hasText: 'Resolve' }).click();
      const ticket = pane.getByTestId('ticket-screen');
      await expect(ticket).toHaveAttribute('data-issue', '10106');
      await expect(ticket.getByTestId('ticket-move-form')).toHaveAttribute('data-move', '41');
      expect(await trackerRequests(harness, MOVE_ROUTE, ['WEB-6', '10106'])).toEqual([]);
    });
  }
}
