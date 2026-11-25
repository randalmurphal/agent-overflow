// The Issues pane's ticket screen (docs/specs/issues.md, "Ticket"), shared
// by the desktop and compact projects, through the shipped UI, the real
// backend and the fake Jira. Covered: a card opens its ticket over the
// board (full width with Back on compact); a ticket with 30 comments
// mounts the newest five and the reveal control mounts 20 more; a status
// picked from the ticket's moves reaches Jira and the ticket shows it; a
// comment sent from the box reaches Jira and shows; a description edit
// that would lose a column layout is refused with what it would lose, and
// Save anyway sends it; closing the ticket shows the board, which reads at
// once and holds the moved card; Escape (the phone's Back on compact)
// closes a ticket too. A paired computer going off takes its open
// ticket's content with it, saying once where it was that the computer is
// not connected, and the ticket reads again when the computer is back.

import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import type { Locator, Page } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test } from './fixtures.js';
import { pairOwnDevice } from './headless-pairing-helpers.js';
import { startTogether } from './launch-helpers.js';
import { connectFakeJiraBoard, seedFakeJira, type ExtraJira } from './tracker-helpers.js';

export interface TicketSurface {
  compact: boolean;
}

const ISSUE_ID = '10104';
const COMMENTS = 30;

function paragraph(text: string) {
  return { type: 'paragraph', content: [{ type: 'text', text }] };
}

function doc(...content: unknown[]) {
  return { type: 'doc', version: 1, content };
}

function column(text: string) {
  return { type: 'layoutColumn', attrs: { width: 50 }, content: [paragraph(text)] };
}

// WEB-4, in progress in the active sprint: a description with a column
// layout Markdown writes between markers, and 30 comments a minute apart.
const TICKET: ExtraJira = {
  projects: [],
  boards: [],
  issues: [{
    id: ISSUE_ID, key: 'WEB-4', project: 'WEB', type: '10002', status: '10001', summary: 'Ticket with comments', sprints: [1],
    description: doc(paragraph('Intro'), { type: 'layoutSection', content: [column('Left side'), column('Right side')] }),
    comments: Array.from({ length: COMMENTS }, (_, i) => ({
      id: String(30_001 + i),
      author: 'acc-user1',
      body: doc(paragraph(`Seeded comment ${i + 1}`)),
      created: new Date(Date.now() - (COMMENTS - i) * 60_000 - 3600_000).toISOString(),
    })),
  }],
};

function card(pane: Locator, issueId: string): Locator {
  return pane.locator(`[data-testid="issues-card"][data-issue="${issueId}"]`);
}

/** Opens the pane from the tools row; on compact the thread screen holds it. */
async function openPane(page: Page, compact: boolean): Promise<Locator> {
  await page.getByTestId('sidebar-tool-issues').click();
  if (compact) await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
  const pane = page.getByTestId('issues-pane');
  await expect(pane).toBeVisible();
  return pane;
}

async function press(target: Locator, compact: boolean): Promise<void> {
  await (compact ? target.tap() : target.click());
}

/** Opens the card's ticket and waits for its content. */
async function openTicket(pane: Locator, issueId: string, compact: boolean): Promise<Locator> {
  await press(card(pane, issueId).getByTestId('issues-card-open'), compact);
  const ticket = pane.getByTestId('ticket-screen');
  await expect(ticket).toHaveAttribute('data-issue', issueId);
  await expect(pane.getByTestId('issues-ticket')).toHaveAttribute('data-placement', 'over');
  return ticket;
}

function field(ticket: Locator, id: string): Locator {
  return ticket.locator(`[data-testid="ticket-field"][data-field="${id}"]`);
}

interface LoggedRequest { route?: string; status: number; body?: unknown }

async function trackerRequests(harness: HarnessApp, route: string): Promise<LoggedRequest[]> {
  const log = await harness.rpc<{ requests: LoggedRequest[] }>('HarnessTrackerRequests', 0);
  return log.requests.filter((r) => r.route === route);
}

export function registerIssuesTicketFlow({ compact }: TicketSurface): void {
  test('a ticket opened from a card shows its comments a few at a time and writes its status, a comment and its description', async ({ harness, page }) => {
    test.setTimeout(120_000);
    await connectFakeJiraBoard(harness, compact ? 'compact-issues-ticket' : 'issues-ticket', TICKET);
    await harness.open(page);
    const pane = await openPane(page, compact);
    await expect(card(pane, ISSUE_ID)).toContainText('Ticket with comments', { timeout: 30_000 });

    const ticket = await openTicket(pane, ISSUE_ID, compact);
    await expect(ticket.getByTestId('ticket-title')).toHaveText('Ticket with comments');
    await expect(ticket.getByTestId('ticket-key')).toHaveText('WEB-4');
    // The board under the ticket takes no input while covered.
    await expect(pane.getByTestId('issues-board-region')).toHaveAttribute('inert', '');
    if (compact) {
      await expect(ticket.getByTestId('ticket-close')).toHaveCount(0);
      await expect(pane.getByTestId('issues-back')).toBeVisible();
    }

    // The newest five of 30 comments are mounted; one press mounts 20 more.
    const comments = ticket.getByTestId('ticket-comment');
    await expect(comments).toHaveCount(5, { timeout: 30_000 });
    await expect(comments.last()).toContainText(`Seeded comment ${COMMENTS}`);
    const earlier = ticket.getByTestId('ticket-comments-earlier');
    await expect(earlier).toHaveText('Show 20 earlier comments');
    await press(earlier, compact);
    await expect(comments).toHaveCount(25);
    await expect(comments.first()).toContainText('Seeded comment 6');
    await expect(earlier).toHaveText('Show 5 earlier comments');

    // The status, from the moves the ticket holds.
    const status = field(ticket, 'status');
    await expect(status.getByTestId('ticket-field-value')).toContainText('In Progress');
    await press(status.getByTestId('ticket-field-value'), compact);
    await press(page.getByTestId('ticket-picker-item').filter({ hasText: 'Done' }), compact);
    await expect(status.getByTestId('ticket-field-value')).toContainText('Done', { timeout: 15_000 });
    expect((await trackerRequests(harness, 'POST rest/api/3/issue/{issue}/transitions')).map((r) => r.status)).toEqual([204]);

    // A comment from the box.
    const box = ticket.getByTestId('ticket-comment-input');
    await box.fill('A comment from the ticket screen');
    await press(ticket.getByTestId('ticket-comment-send'), compact);
    await expect(comments.filter({ hasText: 'A comment from the ticket screen' })).toHaveCount(1, { timeout: 15_000 });
    await expect(box).toHaveValue('');
    expect((await trackerRequests(harness, 'POST rest/api/3/issue/{issue}/comment')).map((r) => r.status)).toEqual([201]);

    // A description edit that drops the layout's markers would lose the
    // layout: refused, nothing sent, until Save anyway.
    const description = ticket.locator('[data-testid="ticket-section"][data-field="description"]');
    await expect(description.getByTestId('ticket-section-text')).toContainText('Left side');
    await press(description.getByTestId('ticket-section-edit'), compact);
    const editor = description.getByTestId('ticket-section-editor');
    const markdown = await editor.inputValue();
    expect(markdown).toContain('<!-- layout -->');
    await editor.fill(markdown.split('\n').filter((line) => !line.startsWith('<!--')).join('\n').replace('Intro', 'Intro, edited'));
    await press(description.getByTestId('ticket-section-save'), compact);
    const outcome = description.getByTestId('ticket-write-outcome');
    await expect(outcome).toHaveAttribute('data-kind', 'refused', { timeout: 15_000 });
    await expect(outcome).toContainText('It would lose');
    expect(await trackerRequests(harness, 'PUT rest/api/3/issue/{issue}')).toEqual([]);
    await press(description.getByTestId('ticket-write-save-anyway'), compact);
    await expect(description.getByTestId('ticket-section-text')).toContainText('Intro, edited', { timeout: 15_000 });
    await expect(outcome).toHaveCount(0);
    expect((await trackerRequests(harness, 'PUT rest/api/3/issue/{issue}')).map((r) => r.status)).toEqual([200]);

    // Closing the ticket shows the board, which reads the move at once.
    await press(pane.getByTestId(compact ? 'issues-back' : 'ticket-close'), compact);
    await expect(ticket).toHaveCount(0);
    await expect(pane.getByTestId('issues-board-region')).not.toHaveAttribute('inert');
    if (compact) {
      await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
      await expect(pane.getByTestId('issues-column-tab').filter({ hasText: 'Done' })).toContainText('1', { timeout: 15_000 });
    } else {
      const done = pane.locator('[data-testid="issues-column"][data-column="Done"]');
      await expect(card(done, ISSUE_ID)).toBeVisible({ timeout: 15_000 });
    }

    // Escape, and the phone's Back, close the ticket too. The compact
    // board shows the moved card under Done.
    if (compact) await press(pane.getByTestId('issues-column-tab').filter({ hasText: 'Done' }), compact);
    const reopened = await openTicket(pane, ISSUE_ID, compact);
    await expect(reopened.getByTestId('ticket-title')).toHaveText('Ticket with comments');
    if (compact) await page.goBack();
    else await page.keyboard.press('Escape');
    await expect(reopened).toHaveCount(0);
    await expect(card(pane, ISSUE_ID)).toBeVisible();
    if (compact) await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
  });

  test('a ticket open on a paired computer that goes off says so where its content was, and shows again once it is back', async ({ page }) => {
    test.setTimeout(240_000);
    const root = await mkdtemp(join(tmpdir(), 'ao-issues-ticket-'));
    const dataDir = join(root, 'state');
    let home: HarnessApp | undefined;
    let remote: HarnessApp | undefined;
    try {
      [home, remote] = await startTogether(launchHarness(), launchHarness({ dataDir }));
      const laptop = await pairOwnDevice(home, remote, 'Laptop');
      await connectFakeJiraBoard(remote, 'issues-ticket-remote', TICKET);
      await home.open(page);
      const pane = await openPane(page, compact);
      await expect(card(pane, ISSUE_ID)).toBeVisible({ timeout: 60_000 });
      const ticket = await openTicket(pane, ISSUE_ID, compact);
      const comments = ticket.getByTestId('ticket-comment');
      await expect(comments).toHaveCount(5, { timeout: 30_000 });
      await expect(ticket.getByTestId('ticket-offline')).toHaveCount(0);

      // Off: one sentence where the content was, under the card's title.
      expect(await remote.stop()).toBe(true);
      await expect(ticket.getByTestId('ticket-offline')).toHaveText('Laptop is not connected. The ticket shows when it connects.', { timeout: 30_000 });
      await expect(ticket.getByTestId('ticket-title')).toHaveText('Ticket with comments');
      await expect(comments).toHaveCount(0);
      await expect(ticket.getByTestId('ticket-opening')).toHaveCount(0);
      await expect(ticket.getByText('Laptop is not connected')).toHaveCount(1);

      // Back on, at its new address, with its Jira site served again: the
      // open ticket reads again.
      remote = await launchHarness({ dataDir });
      await seedFakeJira(remote, TICKET);
      await home.rpc('RepairBackendAddress', laptop, `https://127.0.0.1:${remote.bootstrap.port}`);
      await expect(comments).toHaveCount(5, { timeout: 60_000 });
      await expect(ticket.getByTestId('ticket-offline')).toHaveCount(0);
    } finally {
      await remote?.close();
      await home?.close();
      await rm(root, { recursive: true, force: true });
    }
  });
}
