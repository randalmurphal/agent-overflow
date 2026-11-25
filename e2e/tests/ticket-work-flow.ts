// A ticket's Work, Ask and Start work (docs/specs/issues.md, "Ticket" and
// "Start work applies per-project defaults"), shared by the desktop and
// compact projects, through the shipped UI, the real backend, the fake
// Jira and the fake forge. Covered: a pull request whose title names a
// ticket carries a chip on the Pull requests page, from the list the real
// pump stored, and the chip opens the ticket; the ticket's Work (in Details
// on compact) lists the thread linked to it and that pull request; Ask
// opens a thread linked to the ticket with its file attached to the
// unsent draft and writes nothing to the tracker; Start work shows the
// project's defaults, makes the thread in a worktree on the branch named
// after the ticket, moves and assigns the ticket once each, saves the
// defaults when asked and opens the thread; a move the tracker refuses
// leaves the thread made and unopened with the refusal on screen, and Try
// again sends only the move and opens that same thread.

import type { Locator, Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { publishPullRequest, type ForgeRepo } from './forge-helpers.js';
import { launchPullsHarness, openPage, pickFilter, row as pullRow } from './pulls-page-flow.js';
import { harnessGit } from './worktree-removal-helpers.js';
import { JIRA_SITE, connectFakeJira, connectFakeJiraBoard, openIssuesPane, press, seedFakeJira, trackerRequests } from './tracker-helpers.js';

export interface WorkSurface {
  compact: boolean;
}

// WEB-1 "First story" (in progress) and WEB-2 "Second task" (to do) in
// the seed's sprint.
const WEB1 = '10101';
const WEB2 = '10102';
const MOVE_ROUTE = 'POST rest/api/3/issue/{issue}/transitions';
const EDIT_ROUTE = 'PUT rest/api/3/issue/{issue}';

interface ThreadRow {
  id: string;
  title: string;
  worktreePath?: string;
  ticketLink?: { key: string; origin: string } | null;
}

const threadRows = (harness: HarnessApp) => harness.rpc<ThreadRow[]>('HarnessListThreadRows');

/** Error toasts only: the class is the toast's one type discriminator. */
const errorToasts = (page: Page) => page.locator('[role="alert"].text-error');

function card(pane: Locator, issueId: string): Locator {
  return pane.locator(`[data-testid="issues-card"][data-issue="${issueId}"]`);
}

/** Opens the card's ticket and answers its Work, opening Details on a narrow
 * ticket. On compact the board shows one column, picked from its strip. */
async function openWork(page: Page, issueId: string, compact: boolean, column?: string): Promise<{ ticket: Locator; work: Locator }> {
  const pane = await openIssuesPane(page, compact);
  if (compact && column) {
    const tab = pane.getByTestId('issues-column-tab').filter({ hasText: column });
    await expect(tab).toBeVisible({ timeout: 30_000 });
    await tab.tap();
    await expect(tab).toHaveAttribute('aria-selected', 'true');
  }
  await expect(card(pane, issueId)).toBeVisible({ timeout: 30_000 });
  await press(card(pane, issueId).getByTestId('issues-card-open'), compact);
  const ticket = pane.getByTestId('ticket-screen');
  await expect(ticket).toHaveAttribute('data-issue', issueId);
  return { ticket, work: await workOf(ticket, compact) };
}

/** The Work section on the ticket's rail; the compact ticket is narrow, and
 * opens it from Details once the ticket is read. */
async function workOf(ticket: Locator, compact: boolean): Promise<Locator> {
  const details = ticket.getByTestId('ticket-details');
  if (compact) {
    await expect(details).toBeVisible({ timeout: 30_000 });
    if ((await details.getAttribute('aria-expanded')) !== 'true') await press(details, compact);
  }
  const work = ticket.getByTestId('ticket-rail').getByTestId('ticket-work');
  await expect(work).toBeVisible();
  return work;
}

/** The thread the page shows; on compact the thread screen. */
async function expectThreadShown(page: Page, title: string, compact: boolean): Promise<void> {
  if (compact) await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
  await expect(page.getByTestId('chat-header-title')).toHaveText(title, { timeout: 30_000 });
}

export function registerTicketWorkFlow({ compact }: WorkSurface): void {
  test('a pull request that names a ticket carries its chip, which opens the ticket, whose Work lists it and the linked thread', async ({ page }) => {
    test.setTimeout(150_000);
    const harness = await launchPullsHarness();
    try {
      await seedFakeJira(harness);
      const seed = await harness.rpc<SeedResult>('HarnessSeed', {
        projects: [{
          name: compact ? 'compact-ticket-work' : 'ticket-work',
          repo: { commits: [{ message: 'init', files: { 'README.md': '# Widgets\n' } }] },
          threads: [{ title: 'Linked thread', turns: [{ userText: 'Linked thread', items: [{ kind: 'assistant_text', summary: 'done' }] }] }],
        }],
      });
      const { projectId, path, threadIds: [linked] } = seed.projects[0];
      const board = await connectFakeJira(harness, projectId);
      // Linked first: the link puts WEB-1 in this computer's copy of the
      // tracker, which is what makes WEB a connected project for the chip.
      await harness.rpc('SetThreadTicket', linked, { kind: 'jira', site: board.site, issueId: WEB1 });
      const repo: ForgeRepo = {
        forge: 'github',
        project: `ao-e2e/work-${compact ? 'compact' : 'desktop'}-${Date.now().toString(36)}`,
        pulls: [{ number: 301, title: 'WEB-1 Retry the cursor read', author: 'alice', authorName: 'Alice Ng', updatedAt: '2026-10-19T10:00:00Z' }],
      };
      await publishPullRequest(harness, path, repo, { 'src/cursor.ts': 'export const cursor = 1;\n' });
      await harness.open(page);

      const pulls = await openPage(page);
      await pickFilter(page, pulls, 'all');
      const chip = pullRow(page, 301).getByTestId('pull-ticket');
      await expect(chip).toHaveText('WEB-1', { timeout: 30_000 });
      await expect(chip).toHaveAttribute('title', 'WEB-1 First story');
      await press(chip, compact);

      const issues = page.getByTestId('issues-pane');
      const ticket = issues.getByTestId('ticket-screen');
      await expect(ticket).toHaveAttribute('data-issue', WEB1, { timeout: 30_000 });
      await expect(issues.getByTestId('ticket-key')).toHaveText('WEB-1');
      const work = await workOf(ticket, compact);
      await expect(work.getByTestId('ticket-work-thread')).toHaveCount(1);
      await expect(work.getByTestId('ticket-work-thread')).toHaveAttribute('data-thread', linked);
      await expect(work.getByTestId('ticket-work-thread')).toContainText('Linked thread');
      const pr = work.getByTestId('ticket-work-pull');
      await expect(pr).toHaveCount(1, { timeout: 15_000 });
      await expect(pr).toContainText('#301');
      await expect(pr).toContainText('WEB-1 Retry the cursor read');
      await expect(pr.getByTestId('ticket-work-pull-state')).toHaveText('Open');
      await expect(pr).toContainText('Alice');

      // The thread's row opens it.
      await press(work.getByTestId('ticket-work-thread'), compact);
      await expectThreadShown(page, 'Linked thread', compact);
      await expect(errorToasts(page)).toHaveCount(0);
    } finally {
      await harness.close();
    }
  });

  test('Ask opens a thread linked to the ticket with its file on the unsent draft, and writes nothing to the tracker', async ({ harness, page }) => {
    test.setTimeout(120_000);
    await connectFakeJiraBoard(harness, compact ? 'compact-ticket-ask' : 'ticket-ask');
    await harness.open(page);
    const { work } = await openWork(page, WEB1, compact);
    await expect(work.getByTestId('ticket-work-thread')).toHaveCount(0);
    await press(work.getByTestId('ticket-work-ask'), compact);

    await expectThreadShown(page, 'WEB-1 First story', compact);
    await expect(page.getByTestId('attachment-file-chip')).toContainText('WEB-1.md');
    await expect(page.getByLabel('Message Input')).toHaveValue('');
    const rows = await threadRows(harness);
    expect(rows.map((r) => [r.title, r.ticketLink?.key, r.ticketLink?.origin])).toEqual([['WEB-1 First story', 'WEB-1', 'ask']]);
    expect(await trackerRequests(harness, MOVE_ROUTE)).toEqual([]);
    expect(await trackerRequests(harness, EDIT_ROUTE)).toEqual([]);
    await expect(errorToasts(page)).toHaveCount(0);
  });

  test('Start work moves and assigns the ticket once, makes its thread on the branch named after it, saves the defaults and opens it', async ({ harness, page }) => {
    test.setTimeout(150_000);
    const board = await connectFakeJiraBoard(harness, compact ? 'compact-start-work' : 'start-work');
    await harness.open(page);
    const { ticket, work } = await openWork(page, WEB2, compact, 'To Do');
    await press(work.getByTestId('ticket-work-start'), compact);
    const form = page.getByTestId('start-work-form');
    await expect(form).toBeVisible();
    await expect(form.getByTestId('start-work-branch')).toHaveValue('WEB-2-second-task');
    await expect(form.getByTestId('start-work-status')).toHaveValue('');
    await expect(form.getByTestId('start-work-assign')).toBeChecked();
    await form.getByTestId('start-work-status').selectOption({ label: 'In Progress' });
    await press(form.getByTestId('start-work-save'), compact);
    await press(form.getByTestId('start-work-go'), compact);

    await expectThreadShown(page, 'WEB-2 Second task', compact);
    await expect(page.getByTestId('attachment-file-chip')).toContainText('WEB-2.md');
    await expect(page.getByLabel('Message Input')).toHaveValue('Work on WEB-2: Second task.');
    const [made] = await threadRows(harness);
    expect(made).toMatchObject({ title: 'WEB-2 Second task', ticketLink: { key: 'WEB-2', origin: 'start_work' } });
    expect(harnessGit(harness, made.worktreePath!, 'rev-parse', '--abbrev-ref', 'HEAD')).toBe('WEB-2-second-task');
    const moves = await trackerRequests(harness, MOVE_ROUTE, ['WEB-2', WEB2]);
    expect(moves.map((r) => r.status)).toEqual([204]);
    const edits = await trackerRequests(harness, EDIT_ROUTE, ['WEB-2', WEB2]);
    expect(edits.map((r) => r.body)).toEqual([{ fields: { assignee: { accountId: 'acc-user1' } } }]);
    const setup = await harness.rpc<{ setup: { start_work: unknown[] } }>('ProjectTrackerSetup', board.projectId);
    expect(setup.setup.start_work).toEqual([{ tracker: 'jira', site: board.site, project: 'WEB', status: '10001', assign: true, branch: '{key}-{title}' }]);

    // The ticket shows both changes, and the next Start work starts from
    // the saved defaults and says a thread from Start work exists.
    if (compact) {
      await page.getByTestId('chat-header').getByTestId('compact-back').click();
      await openIssuesPane(page, compact);
    }
    const status = ticket.locator('[data-testid="ticket-narrow-row"], [data-testid="ticket-rail"]').first().locator('[data-testid="ticket-field"][data-field="status"]');
    await expect(status).toContainText('In Progress', { timeout: 15_000 });
    const again = await workOf(ticket, compact);
    await expect(again.getByTestId('ticket-work-thread')).toHaveAttribute('data-thread', made.id);
    await press(again.getByTestId('ticket-work-start'), compact);
    await expect(form.getByTestId('start-work-started')).toContainText('WEB-2 already has a thread from Start work: WEB-2 Second task');
    await expect(form.getByTestId('start-work-status-already')).toHaveText('Already in In Progress.');
    await expect(errorToasts(page)).toHaveCount(0);
  });

  test('a move the tracker refuses leaves the thread made, and Try again sends only the move and opens that thread', async ({ harness, page }) => {
    test.setTimeout(150_000);
    await connectFakeJiraBoard(harness, compact ? 'compact-start-work-refused' : 'start-work-refused');
    await harness.rpc('HarnessTrackerFault', {
      origin: { tracker: 'jira', site: JIRA_SITE }, route: MOVE_ROUTE, kind: 'refuse', times: 1, status: 400,
      body: { errorMessages: ['A resolution is required to move this issue.'], errors: {} },
    });
    await harness.open(page);
    const { work } = await openWork(page, WEB2, compact, 'To Do');
    await press(work.getByTestId('ticket-work-start'), compact);
    const form = page.getByTestId('start-work-form');
    await form.getByTestId('start-work-status').selectOption({ label: 'In Progress' });
    await press(form.getByTestId('start-work-go'), compact);

    const run = work.getByTestId('start-work-run');
    const step = (name: string) => run.locator(`[data-testid="start-work-step"][data-step="${name}"]`);
    await expect(step('move')).toHaveAttribute('data-outcome', 'refused', { timeout: 30_000 });
    await expect(step('move')).toContainText('A resolution is required to move this issue.');
    await expect(step('thread')).toHaveAttribute('data-outcome', 'applied');
    await expect(step('assign')).toHaveAttribute('data-outcome', 'applied');
    await expect(run).not.toHaveAttribute('data-running', '');
    const [made] = await threadRows(harness);
    expect(made).toMatchObject({ title: 'WEB-2 Second task', ticketLink: { key: 'WEB-2', origin: 'start_work' } });
    // The thread is made and listed, and not opened over the refusal.
    await expect(page.getByTestId('issues-pane').getByTestId('ticket-screen')).toBeVisible();

    await press(run.getByTestId('start-work-retry'), compact);
    await expectThreadShown(page, 'WEB-2 Second task', compact);
    expect((await threadRows(harness)).map((r) => r.id)).toEqual([made.id]);
    const moves = await trackerRequests(harness, MOVE_ROUTE, ['WEB-2', WEB2]);
    expect(moves.map((r) => r.status)).toEqual([400, 204]);
    expect(await trackerRequests(harness, EDIT_ROUTE, ['WEB-2', WEB2])).toHaveLength(1);
  });
}
