// A number widget's limit and pin, and the board's summary row
// (docs/specs/issues.md, "Board" and "Dashboards"), shared by the desktop
// and compact projects, through the shipped UI, the real backend and the
// fake Jira of issues-dashboards-flow.ts.
//
// Covered on both: the bell sets the stuck count's limit (on compact once
// the dashboard is edited) and the widget turns amber past it; the pin
// puts it on the board's row, which shows it as a chip from the board's
// computer's count, amber too; the chip filters the board to its ticket,
// says so, and keeps the filter on the Backlog, where the row does not
// show; a move made in Jira reaches the filter with no Refresh (it
// follows its tile's count); Clear shows every card; the row expands into
// tiles and is edited in place: a tile added from a dashboard, both taken
// off, and the row goes with its last tile.

import type { Locator, Page } from '@playwright/test';

import { expect, test } from './fixtures.js';
import { openBacklog } from './issues-backlog-flow.js';
import { openBoardDashboards, widget } from './issues-dashboards-flow.js';
import { moveJiraIssue, press } from './tracker-helpers.js';

export interface BoardRowSurface {
  compact: boolean;
}

// The fake's workflow: the transition into Done.
const DONE = '31';

/** Switches the pane to the Board: its tab, or the folded picker. */
async function openBoard(page: Page, pane: Locator, compact: boolean): Promise<void> {
  const picker = pane.getByTestId('issues-tabs-picker');
  if (await picker.isVisible()) {
    await press(picker, compact);
    await press(page.getByTestId('issues-tab-option').filter({ hasText: 'Board' }), compact);
  } else {
    await press(pane.locator('[data-testid="issues-tab"][data-screen="board"]'), compact);
  }
  await expect(pane.getByTestId('issues-board-region')).toHaveAttribute('data-screen', 'board');
}

export function registerBoardRowFlow({ compact }: BoardRowSurface): void {
  const name = (s: string) => `${compact ? 'compact-' : ''}issues-board-row-${s}`;

  test("a number's limit and pin reach the board's row, whose tile filters the board, follows its count and is edited in place", async ({ harness, page }) => {
    test.setTimeout(240_000);
    const { board, pane, screen } = await openBoardDashboards(harness, page, name('row'), compact);

    // The limit: on compact the bell shows once the dashboard is edited.
    const stuck = widget(screen, 'stuck');
    if (compact) {
      await expect(stuck.getByTestId('dashboard-limit-toggle')).toHaveCount(0);
      await press(screen.getByTestId('dashboard-edit'), compact);
    }
    await press(stuck.getByTestId('dashboard-limit-toggle'), compact);
    const limit = page.getByTestId('dashboard-limit');
    await expect(limit).toContainText('Warn me when this is above');
    await expect(limit.getByTestId('dashboard-limit-value')).toHaveText('1');
    await expect(limit.getByTestId('dashboard-limit-note')).toHaveText('No limit is set.');
    await press(limit.getByTestId('dashboard-limit-switch'), compact);
    await expect(stuck.getByTestId('dashboard-limit-toggle')).toHaveAttribute('data-on', 'true', { timeout: 15_000 });
    await expect(stuck).toHaveAttribute('data-over', 'false');
    await press(limit.getByTestId('dashboard-limit-lower'), compact);
    await expect(stuck).toHaveAttribute('data-over', 'true', { timeout: 15_000 });
    await expect(stuck.getByTestId('dashboard-over')).toHaveText('over your limit of 0 tickets');
    await page.keyboard.press('Escape');
    await expect(limit).toHaveCount(0);

    // The pin.
    await press(stuck.getByTestId('dashboard-pin'), compact);
    await expect(stuck.getByTestId('dashboard-pin')).toHaveAttribute('aria-pressed', 'true', { timeout: 15_000 });

    // The row on the Board, from the board's computer's count.
    await openBoard(page, pane, compact);
    const chip = pane.getByTestId('board-row-chip');
    await expect(chip).toHaveText('1 ticket · Stuck right now', { timeout: 30_000 });
    await expect(chip).toHaveAttribute('data-over', 'true');
    const cards = pane.getByTestId('issues-card');
    await expect(cards).not.toHaveCount(1);

    // The chip filters the board to its ticket, on the Backlog too.
    await press(chip, compact);
    const filter = pane.getByTestId('issues-ticket-set-text');
    await expect(filter).toHaveText('Showing only Stuck right now · 1 ticket', { timeout: 15_000 });
    await expect(cards).toHaveCount(1);
    await expect(cards).toHaveAttribute('data-issue', '10120');
    await expect(chip).toHaveAttribute('aria-pressed', 'true');
    const backlog = await openBacklog(page, pane, compact);
    await expect(pane.getByTestId('board-row')).toHaveCount(0);
    await expect(filter).toHaveText('Showing only Stuck right now · 1 ticket');
    await expect(backlog.getByTestId('issues-backlog-row')).toHaveCount(1);
    await expect(backlog.getByTestId('issues-backlog-row')).toHaveAttribute('data-issue', '10120');
    await openBoard(page, pane, compact);

    // A move in Jira reaches the filter and the chip with no Refresh.
    await moveJiraIssue(board, 'WEB-20', DONE);
    await expect(filter).toHaveText('Showing only Stuck right now · 0 tickets', { timeout: 60_000 });
    await expect(cards).toHaveCount(0);
    await expect(chip).toHaveText('0 tickets · Stuck right now', { timeout: 30_000 });
    await expect(chip).toHaveAttribute('data-over', 'false');
    await press(pane.getByTestId('issues-ticket-set-clear'), compact);
    await expect(pane.getByTestId('issues-ticket-set')).toHaveCount(0);
    await expect(cards).not.toHaveCount(0);

    // Tiles, edited in place.
    await press(pane.getByTestId('board-row-toggle'), compact);
    await expect(pane.getByTestId('board-row-tile')).toHaveCount(1);
    await press(pane.getByTestId('board-row-edit'), compact);
    await press(pane.getByTestId('board-row-add'), compact);
    await expect(page.getByTestId('board-row-offer-stuck_list')).toContainText('Stuck tickets');
    await press(page.getByTestId('board-row-offer-stuck_list'), compact);
    await expect(pane.getByTestId('board-row-tile')).toHaveCount(2, { timeout: 15_000 });
    await expect(pane.getByTestId('board-row-tile').nth(1)).toHaveAttribute('data-widget', 'stuck_list');
    await press(pane.getByTestId('board-row-remove').first(), compact);
    await expect(pane.getByTestId('board-row-tile')).toHaveCount(1, { timeout: 15_000 });
    await press(pane.getByTestId('board-row-remove'), compact);
    await expect(pane.getByTestId('board-row')).toHaveCount(0, { timeout: 15_000 });
  });
}
