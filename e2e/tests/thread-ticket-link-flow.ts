// A thread's ticket link on screen (docs/specs/issues.md, "Thread link"),
// shared by the desktop and compact projects, through the shipped UI, the
// real backend and the fake Jira. Covered: Link Ticket… from the sidebar
// row's menu finds a ticket by key and links it; the row's line carries the
// ticket's mark and key, and a linked thread with no worktree gets the
// line; sidebar search finds the thread by the ticket's key and by its
// title; the thread header's ticket (on compact the Thread actions menu's
// row above the pull request) opens the ticket in the Issues pane; a
// connection turned off dims the mark on both surfaces, and the ticket
// says why it cannot open; Change ticket… from the Thread actions menu
// finds another by words of its title; removing the link from the header
// (desktop) and from the sidebar row's menu (both) takes the line and the
// button away and clears the link on the computer, and Link ticket… from
// the Thread actions menu links it again (desktop).

import type { Locator, Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test } from './fixtures.js';
import { connectFakeJiraBoard } from './tracker-helpers.js';

export interface LinkSurface {
  compact: boolean;
}

// WEB-1 "First story", in progress in the seed's sprint.
const ISSUE_ID = '10101';

interface LinkedRow { ticketLink?: { key: string; state: string } | null }

async function linkOf(harness: HarnessApp, threadId: string): Promise<LinkedRow['ticketLink']> {
  return (await harness.rpc<LinkedRow>('GetThread', threadId)).ticketLink ?? null;
}

function row(page: Page, threadId: string) {
  return page.locator(`[data-sidebar-thread-id="${threadId}"]`);
}

/** The row with its second line, which sits under the row in its shell. */
function shell(page: Page, threadId: string) {
  return page.getByTestId('thread-row-shell').filter({ has: row(page, threadId) });
}

/** Opens the row's menu: a right click, or the row's menu button on the phone. */
async function rowMenu(page: Page, threadId: string, compact: boolean) {
  if (compact) await row(page, threadId).getByTestId('thread-row-menu').tap();
  else await row(page, threadId).click({ button: 'right' });
  const menu = page.getByRole('menu', { name: 'Thread Actions' });
  await expect(menu).toBeVisible();
  return menu;
}

/** Opens the thread; on compact the thread screen shows it. */
async function openThread(page: Page, threadId: string, title: string, compact: boolean): Promise<void> {
  await row(page, threadId).click();
  if (compact) await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
  await expect(page.getByTestId('chat-header-title')).toHaveText(title);
}

/** Opens the ticket from the header: its button, or the menu's row on the phone. */
async function openFromHeader(page: Page, compact: boolean): Promise<void> {
  if (compact) {
    await page.getByTestId('chat-header-more').tap();
    await page.getByTestId('chat-header-menu-ticket').tap();
  } else {
    await page.getByTestId('chat-header-ticket').click();
  }
}

async function back(page: Page, compact: boolean): Promise<void> {
  if (!compact) return;
  // The thread's own Back: the Issues pane keeps one of its own.
  await page.getByTestId('chat-header').getByTestId('compact-back').click();
  await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'list');
}

async function press(target: Locator, compact: boolean): Promise<void> {
  await (compact ? target.tap() : target.click());
}

/** Picks a ticket in the open finder: types `text`, then picks the row naming `key`. */
async function pickTicket(page: Page, text: string, key: string, compact: boolean): Promise<void> {
  const find = page.getByTestId('ticket-picker-find');
  await expect(find).toBeVisible();
  await find.fill(text);
  const item = page.getByTestId('ticket-picker-item').filter({ hasText: key });
  await expect(item).toHaveCount(1, { timeout: 15_000 });
  await press(item, compact);
  await expect(find).toHaveCount(0);
}

export function registerThreadTicketLinkFlow({ compact }: LinkSurface): void {
  test('a thread linked through the finder shows its ticket in the sidebar and the header, is found by it, opens it, changes and drops it', async ({ harness, page }) => {
    test.setTimeout(120_000);
    const board = await connectFakeJiraBoard(harness, compact ? 'compact-thread-link' : 'thread-link', undefined, ['Linked thread', 'Unlinked thread'].map((title) => ({
      title, turns: [{ userText: title, items: [{ kind: 'assistant_text', summary: 'done' }] }],
    })));
    const [linked, other] = board.threadIds;
    await harness.open(page);

    // Link Ticket… from the sidebar row's menu, by key.
    const ticket = shell(page, linked).getByTestId('thread-row-ticket');
    await expect(row(page, linked)).toBeVisible();
    await expect(ticket).toHaveCount(0);
    let menu = await rowMenu(page, linked, compact);
    await expect(menu.getByTestId('thread-menu-ticket-find')).toHaveText(/Link Ticket…/);
    await press(menu.getByTestId('thread-menu-ticket-find'), compact);
    await pickTicket(page, 'web-1', 'WEB-1', compact);
    await expect(ticket).toHaveText('WEB-1');
    expect(await linkOf(harness, linked)).toMatchObject({ key: 'WEB-1', state: 'current' });

    // The row's line: the mark and key, on a thread with no worktree.
    await expect(ticket).toHaveAttribute('data-link-state', 'current');
    await expect(ticket.locator('[data-status-group]')).toHaveAttribute('data-status-group', 'in_progress');
    await expect(shell(page, other).getByTestId('thread-row-worktree')).toHaveCount(0);

    // Search by the key and by the ticket's title.
    const search = page.getByTestId('sidebar-thread-search');
    for (const query of ['web-1', 'first story']) {
      await search.fill(query);
      await expect(row(page, linked)).toBeVisible();
      await expect(row(page, other)).toHaveCount(0);
    }
    await search.fill('');
    await expect(row(page, other)).toBeVisible();

    // The header opens the ticket in the Issues pane.
    await openThread(page, linked, 'Linked thread', compact);
    if (compact) await expect(page.getByTestId('chat-header-ticket')).toHaveCount(0);
    else await expect(page.getByTestId('chat-header-ticket')).toHaveText('WEB-1');
    await openFromHeader(page, compact);
    const pane = page.getByTestId('issues-pane');
    await expect(pane.getByTestId('ticket-screen')).toHaveAttribute('data-issue', ISSUE_ID, { timeout: 30_000 });
    await expect(pane.getByTestId('ticket-key')).toHaveText('WEB-1');
    await expect(pane.getByTestId('ticket-title')).toHaveText('First story');
    // On the desktop it opens beside the thread, which stays.
    if (!compact) await expect(page.getByTestId('chat-header-title')).toHaveText('Linked thread');

    // A connection turned off: the mark dims on both surfaces, and the
    // ticket says why it cannot open instead of opening.
    await harness.rpc('SetTrackerConnectionEnabled', board.connectionId, false);
    if (compact) {
      // Back from the ticket to its board, then to the list.
      await pane.getByTestId('issues-back').click();
      await expect(pane.getByTestId('ticket-screen')).toHaveCount(0);
      await pane.getByTestId('compact-back').click();
      await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'list');
    }
    await expect(ticket).toHaveAttribute('data-link-state', 'turned_off');
    await openThread(page, linked, 'Linked thread', compact);
    if (!compact) await expect(page.getByTestId('chat-header-ticket')).toHaveAttribute('data-link-state', 'turned_off');
    await openFromHeader(page, compact);
    await expect(page.getByTestId('toast').filter({ hasText: 'has its Jira connection to example.atlassian.net turned off, so WEB-1 cannot open.' })).toHaveCount(1);
    await harness.rpc('SetTrackerConnectionEnabled', board.connectionId, true);
    await expect(ticket).toHaveAttribute('data-link-state', /current|not_checked/);

    // Change ticket… from the Thread actions menu, by words of its title.
    await press(page.getByTestId('chat-header-more'), compact);
    await expect(page.getByTestId('chat-header-menu-ticket-find')).toHaveText(/Change ticket…/);
    await press(page.getByTestId('chat-header-menu-ticket-find'), compact);
    await pickTicket(page, 'second', 'WEB-2', compact);
    await expect(ticket).toHaveText('WEB-2');
    await expect(ticket.locator('[data-status-group]')).toHaveAttribute('data-status-group', 'todo');
    expect(await linkOf(harness, linked)).toMatchObject({ key: 'WEB-2' });

    if (!compact) {
      // Remove from the header's menu: the button and the row's mark go.
      await expect(page.getByTestId('chat-header-ticket')).toHaveText('WEB-2');
      await page.getByTestId('chat-header-ticket').click({ button: 'right' });
      await page.getByTestId('chat-header-ticket-remove').click();
      await expect(page.getByTestId('chat-header-ticket')).toHaveCount(0);
      await expect(ticket).toHaveCount(0);
      expect(await linkOf(harness, linked)).toBeNull();
      // Linked again from the Thread actions menu, for the sidebar's Remove next.
      await page.getByTestId('chat-header-more').click();
      await expect(page.getByTestId('chat-header-menu-ticket-find')).toHaveText(/Link ticket…/);
      await page.getByTestId('chat-header-menu-ticket-find').click();
      await pickTicket(page, 'WEB-2', 'WEB-2', compact);
      await expect(ticket).toHaveText('WEB-2');
    }
    await back(page, compact);

    // Remove from the sidebar row's menu.
    menu = await rowMenu(page, linked, compact);
    await expect(menu.getByTestId('thread-menu-ticket-open')).toHaveText(/Open Ticket WEB-2/);
    await press(menu.getByTestId('thread-menu-ticket-remove'), compact);
    await expect(ticket).toHaveCount(0);
    await expect(shell(page, linked).getByTestId('thread-row-worktree')).toHaveCount(0);
    expect(await linkOf(harness, linked)).toBeNull();
  });
}

