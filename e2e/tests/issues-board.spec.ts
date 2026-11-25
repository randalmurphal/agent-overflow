// The Issues pane's Board screen (docs/specs/issues.md, "Pane" and
// "Board") on the desktop project, through the shipped UI, the real
// backend and the fake Jira. Covered: the tools row opens the pane on the
// board the project opens first; the picker switches to another board; a
// card renamed in Jira reaches the open board without a reload, on the
// computer's poll (the `tracker-poll` knob); a column holding more than
// one page of cards reads its next page as the reader scrolls to its end;
// Only mine shows the cards assigned to the verified account and Search
// the cards whose text matches, each from the whole column; a board no
// project lists any more says so and opens its project's Boards page in
// Settings; Refresh reads the board from Jira now, with no poll due; a
// board whose connection is turned off ends, a pane opened then does not
// open it, and the board opens again once the connection is back on. The
// compact strip and menu are compact-issues-board.spec.ts.

import type { Locator, Page } from '@playwright/test';

import { harnessTiming, launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test } from './fixtures.js';
import { BOARD_ID, connectFakeJiraBoard, renameJiraIssue, type ExtraJira } from './tracker-helpers.js';

const APP_BOARD_ID = '2';
// Past the computer's section page of 100 rows, so the column pages in.
const TO_DO_CARDS = 250;

function appIssue(n: number, status: string, extra: { summary?: string; assignee?: string } = {}) {
  return { id: String(20_000 + n), key: `APP-${n}`, project: 'APP', type: '10002', status, summary: extra.summary ?? `App task ${n}`, assignee: extra.assignee };
}

// A kanban board of project APP: To Do holds 250 cards, two of them
// assigned to the verified account (one past the first page) and one named
// for Search past the first page; In Progress holds two.
const APP: ExtraJira = {
  projects: [{ id: '10001', key: 'APP', name: 'App' }],
  boards: [{ id: 2, name: 'APP board', type: 'kanban', project: 'APP', columns: [
    { name: 'To Do', statuses: ['10000'] }, { name: 'In Progress', statuses: ['10001'] }, { name: 'Done', statuses: ['10002'] },
  ] }],
  issues: [
    ...Array.from({ length: TO_DO_CARDS }, (_, i) => {
      const n = i + 1;
      if (n === 7 || n === 240) return appIssue(n, '10000', { assignee: 'acc-user1' });
      if (n === 123) return appIssue(n, '10000', { summary: 'Gamma widget' });
      return appIssue(n, '10000');
    }),
    appIssue(251, '10001'),
    appIssue(252, '10001'),
  ],
};

function card(pane: Locator, issueId: string): Locator {
  return pane.locator(`[data-testid="issues-card"][data-issue="${issueId}"]`);
}

function column(pane: Locator, name: string): Locator {
  return pane.locator(`[data-testid="issues-column"][data-column="${name}"]`);
}

async function openPane(page: Page): Promise<Locator> {
  await page.getByTestId('sidebar-tool-issues').click();
  const pane = page.getByTestId('issues-pane');
  await expect(pane).toBeVisible();
  return pane;
}

async function pickBoard(page: Page, pane: Locator, name: string): Promise<void> {
  await pane.getByTestId('issues-board-picker').click();
  await page.getByTestId('issues-board-choice').filter({ hasText: name }).click();
  await expect(pane.getByTestId('issues-board-picker')).toHaveAccessibleName(`Board: ${name}`);
}

async function withHarness(timing: Record<string, string>, run: (harness: HarnessApp) => Promise<void>): Promise<void> {
  const harness = await launchHarness({ env: harnessTiming(timing) });
  try {
    await run(harness);
  } finally {
    await harness.close();
  }
}

test('the Board screen follows Jira, pages a long column in, filters it, and ends with its board', async ({ page }) => {
  test.setTimeout(120_000);
  // The product polls a Jira board every 10 s; the rename step waits for
  // one poll.
  await withHarness({ 'tracker-poll': '500ms' }, async (harness) => {
    const board = await connectFakeJiraBoard(harness, 'issues-board', APP);
    const web = { connection: board.connectionId, board: BOARD_ID, opens_first: true };
    await harness.rpc('SetProjectTrackerBoards', board.projectId, [web], [
      { ...web, name: 'WEB board', tracker_project: 'WEB' },
      { connection: board.connectionId, board: APP_BOARD_ID, name: 'APP board', tracker_project: 'APP' },
    ]);

    await harness.open(page);
    const pane = await openPane(page);

    // The board the project opens first, with its active sprint's cards.
    await expect(pane.getByTestId('issues-board-picker')).toHaveAccessibleName('Board: WEB board');
    await expect(card(pane, '10101')).toContainText('First story', { timeout: 30_000 });
    await expect(card(pane, '10102')).toContainText('Second task');

    await pickBoard(page, pane, 'APP board');
    const toDo = column(pane, 'To Do');
    await expect(card(toDo, '20001')).toContainText('App task 1', { timeout: 30_000 });
    await expect(column(pane, 'In Progress').getByTestId('issues-card')).toHaveCount(2);

    // A rename made in Jira arrives on the computer's next poll.
    await renameJiraIssue(board, 'APP-1', 'App task 1, renamed in Jira');
    await expect(card(toDo, '20001')).toContainText('App task 1, renamed in Jira', { timeout: 15_000 });

    // The column held its first page; scrolling to its end reads the rest.
    const scroller = toDo.getByTestId('issues-column-cards');
    const last = card(toDo, String(20_000 + TO_DO_CARDS));
    await expect(async () => {
      await scroller.evaluate((el) => { el.scrollTop = el.scrollHeight; });
      await expect(last).toBeVisible({ timeout: 500 });
    }).toPass({ timeout: 30_000 });

    // Only mine: the verified account's two cards, one from past the
    // first page.
    await pane.getByTestId('issues-only-mine').click();
    await expect(toDo.getByTestId('issues-card')).toHaveCount(2);
    await expect(card(toDo, '20007')).toBeVisible();
    await expect(card(toDo, '20240')).toBeVisible();
    await expect(toDo.getByTestId('issues-column-count')).toHaveText('2');
    await expect(column(pane, 'In Progress').getByTestId('issues-column-empty')).toHaveText('No matching cards');
    await pane.getByTestId('issues-only-mine').click();

    // Search filters every column by the cards' text.
    await pane.getByTestId('issues-search').click();
    const bar = pane.getByTestId('pane-find-bar');
    await bar.getByTestId('pane-find-input').fill('gamma');
    await expect(bar.getByTestId('pane-find-count')).toHaveText('1 match');
    await expect(pane.getByTestId('issues-card')).toHaveCount(1);
    await expect(card(toDo, '20123')).toContainText('Gamma widget');
    await bar.getByTestId('pane-find-input').fill('');
    await expect(card(toDo, '20001')).toBeVisible();

    // No project lists the board any more: the pane says so and opens the
    // project's Boards page.
    await harness.rpc('SetProjectTrackerBoards', board.projectId, [web, { connection: board.connectionId, board: APP_BOARD_ID }], [
      { ...web, name: 'WEB board', tracker_project: 'WEB' },
    ]);
    const ended = pane.getByTestId('issues-board-ended');
    await expect(ended).toContainText('This board is no longer followed');
    const toSettings = ended.getByTestId('issues-ended-settings');
    await expect(toSettings).toHaveText("Open the project's boards");
    await toSettings.click();
    await expect(page.getByTestId('settings-tracker-boards-project')).toHaveValue(board.projectId);
    await expect(page.getByTestId('tracker-board-name')).toHaveText(['WEB board']);
  });
});

test('Refresh reads the board from Jira when no poll is due', async ({ page }) => {
  test.setTimeout(90_000);
  // No poll runs within the test, so only Refresh can bring the rename.
  await withHarness({ 'tracker-poll': '10m' }, async (harness) => {
    const board = await connectFakeJiraBoard(harness, 'issues-board-refresh');
    await harness.open(page);
    const pane = await openPane(page);
    await expect(card(pane, '10101')).toContainText('First story', { timeout: 30_000 });

    await renameJiraIssue(board, 'WEB-1', 'First story, renamed in Jira');
    await pane.getByTestId('issues-refresh').click();
    await expect(card(pane, '10101')).toContainText('First story, renamed in Jira', { timeout: 10_000 });
  });
});

test('a board whose connection is turned off ends, and opens again once it is back on', async ({ page }) => {
  test.setTimeout(90_000);
  await withHarness({ 'tracker-poll': '10m' }, async (harness) => {
    const board = await connectFakeJiraBoard(harness, 'issues-board-off');
    await harness.open(page);
    let pane = await openPane(page);
    await expect(card(pane, '10101')).toBeVisible({ timeout: 30_000 });

    await harness.rpc('SetTrackerConnectionEnabled', board.connectionId, false);
    await expect(pane.getByTestId('issues-board-ended')).toBeVisible();

    // A pane opened now does not open a board it cannot.
    await page.locator('section[data-pane-kind="issues"]').getByTestId('pane-close').click();
    await expect(pane).toHaveCount(0);
    pane = await openPane(page);
    await expect(pane.getByTestId('issues-empty-off')).toBeVisible();
    await expect(pane.getByTestId('issues-card')).toHaveCount(0);

    // Turned back on, the computer says so and the pane opens the board.
    await harness.rpc('SetTrackerConnectionEnabled', board.connectionId, true);
    await expect(card(pane, '10101')).toBeVisible({ timeout: 30_000 });

    // Off and on again under an open board: it ends, then follows again.
    await harness.rpc('SetTrackerConnectionEnabled', board.connectionId, false);
    await expect(pane.getByTestId('issues-board-ended')).toBeVisible();
    await harness.rpc('SetTrackerConnectionEnabled', board.connectionId, true);
    await expect(pane.getByTestId('issues-board-ended')).toHaveCount(0);
    await expect(card(pane, '10101')).toBeVisible();
    await renameJiraIssue(board, 'WEB-1', 'First story, after the connection came back');
    await pane.getByTestId('issues-refresh').click();
    await expect(card(pane, '10101')).toContainText('after the connection came back', { timeout: 10_000 });
  });
});
