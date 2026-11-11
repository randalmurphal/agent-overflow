// mod+f on the Pull requests page filters the list's rows through the
// pane's find bar, shared by the desktop and compact projects
// (pulls-page-find.spec.ts, compact-pulls-page-find.spec.ts), through the
// shipped UI, the real backend and the fake forge. Covered: mod+f from the
// list opens the bar at the top of the list column, above the toolbar and
// the rows, with its input focused and no step buttons; typing narrows the
// rows live, and the bar's count, the group heading, the Needs you count on
// the filter button and the empty state follow it; the filter composes with
// the toolbar's filter; Enter focuses the first row; Escape closes the bar
// and restores the rows, leaving focus on a row the reader moved to and
// otherwise returning it to where the bar opened from; a tab switch applies
// the query to the Pipelines rows and Escape restores them. Desktop: a
// picked row the filter hides stays open in the review beside the list, and
// Escape closes the find before it closes the review. Compact: an opened
// row replaces the list, mod+f there opens no bar, and Back returns to the
// list still filtered. Region and filter rules: paneFind.svelte.test.ts,
// pullsPageRows.test.ts, PullsPaneFind.test.ts, PaneFindBar.test.ts.

import type { Locator, Page } from '@playwright/test';

import { expect, test } from './fixtures.js';
import { launchPullsHarness, openPage, pickFilter, row, seedProjects } from './pulls-page-flow.js';

function listedNumbers(pane: Locator): Promise<number[]> {
  return pane.getByTestId('pull-row').evaluateAll((rows) =>
    rows.map((el) => Number((el as HTMLElement).dataset.prKey!.split(':').pop())));
}

/** The list-row key of the focused element's row, or null. */
function focusedRowKey(page: Page): Promise<string | null> {
  return page.evaluate(() => document.activeElement?.closest<HTMLElement>('[data-list-row-key]')?.dataset.listRowKey ?? null);
}

export function registerPullsPageFindFlow({ compact }: { compact: boolean }): void {
  test('mod+f filters the list rows, every count follows, and Escape restores them', async ({ page }) => {
    test.setTimeout(120_000);
    const harness = await launchPullsHarness();
    try {
      await seedProjects(harness);
      await harness.open(page);
      const pane = await openPage(page);
      await expect(pane.getByTestId('pull-row')).toHaveCount(4);
      await expect(pane.getByTestId('pulls-filter-needs-you-count')).toHaveText('4');
      const column = pane.getByTestId('pulls-list-column');

      // From the toolbar's filter button, mod+f opens the list's bar.
      const filterButton = pane.getByTestId('pulls-filter');
      await filterButton.focus();
      await page.keyboard.press('ControlOrMeta+f');
      const bar = column.getByTestId('pane-find-bar');
      await expect(bar).toBeVisible();
      await expect(bar).toHaveAttribute('data-find-mode', 'filter');
      await expect(bar.getByTestId('pane-find-previous')).toHaveCount(0);
      await expect(bar.getByTestId('pane-find-next')).toHaveCount(0);
      const input = bar.getByTestId('pane-find-input');
      await expect(input).toBeFocused();
      await expect(input).toHaveAttribute('placeholder', 'Filter merge requests');
      const barBox = (await bar.boundingBox())!;
      const columnBox = (await column.boundingBox())!;
      const toolbarBox = (await filterButton.boundingBox())!;
      const firstRowBox = (await pane.getByTestId('pull-row').first().boundingBox())!;
      expect(barBox.x).toBeGreaterThanOrEqual(columnBox.x - 1);
      expect(barBox.x + barBox.width).toBeLessThanOrEqual(columnBox.x + columnBox.width + 1);
      expect(barBox.y + barBox.height).toBeLessThanOrEqual(toolbarBox.y + 1);
      expect(barBox.y + barBox.height).toBeLessThanOrEqual(firstRowBox.y + 1);

      // Typing narrows the rows and every count, live.
      await page.keyboard.type('retry');
      await expect.poll(() => listedNumbers(pane)).toEqual([101]);
      await expect(bar.getByTestId('pane-find-count')).toHaveText('1 match');
      await expect(pane.getByTestId('pulls-group-needs_you')).toContainText('1');
      await expect(pane.getByTestId('pulls-filter-needs-you-count')).toHaveText('1');
      // The source branch and the author match as well as the title.
      await input.fill('retention');
      await expect.poll(() => listedNumbers(pane)).toEqual([202]);
      await input.fill('Abossedgh');
      await expect.poll(() => listedNumbers(pane)).toEqual([101]);
      await input.fill('zzz-none');
      await expect(pane.getByTestId('pulls-empty')).toHaveText('No merge requests match "zzz-none".');
      await expect(bar.getByTestId('pane-find-count')).toHaveText('No matches');
      await expect(pane.getByTestId('pulls-filter-needs-you-count')).toHaveCount(0);

      // The query composes with the toolbar's filter.
      await input.fill('retry');
      await pickFilter(page, pane, 'all');
      await expect.poll(() => listedNumbers(pane)).toEqual([101, 104]);
      await expect(bar.getByTestId('pane-find-count')).toHaveText('2 matches');
      await expect(pane.getByTestId('pulls-group-waiting')).toContainText('1');
      await expect(input).toHaveValue('retry');

      // Enter moves focus to the first row.
      await input.focus();
      await page.keyboard.press('Enter');
      await expect.poll(() => focusedRowKey(page)).toBe(await row(page, 101).getAttribute('data-pr-key'));
      await expect(bar).toBeVisible();

      // Escape from the row closes the bar and restores the rows and the
      // counts; focus stays on the row the reader moved to.
      const focusedRow = await focusedRowKey(page);
      await page.keyboard.press('Escape');
      await expect(bar).toHaveCount(0);
      expect(await focusedRowKey(page)).toBe(focusedRow);
      await expect(pane.getByTestId('pull-row')).toHaveCount(6);
      await expect(pane.getByTestId('pulls-filter-needs-you-count')).toHaveText('4');

      // Reopened from the filter button, focus returns there on Escape.
      await filterButton.focus();
      await page.keyboard.press('ControlOrMeta+f');
      await expect(input).toHaveValue('retry');
      await expect.poll(() => listedNumbers(pane)).toEqual([101, 104]);
      await page.keyboard.press('Escape');
      await expect(bar).toHaveCount(0);
      await expect(filterButton).toBeFocused();

      if (!compact) {
        // A picked row the filter hides stays open in the review beside it.
        await row(page, 202).getByRole('button').first().click();
        const review = page.getByTestId('review-pane');
        await expect(review.getByTestId('review-pr-header')).toContainText('Trim transcript retention');
        await filterButton.focus();
        await page.keyboard.press('ControlOrMeta+f');
        await expect.poll(() => listedNumbers(pane)).toEqual([101, 104]);
        await expect(row(page, 202)).toHaveCount(0);
        await expect(review.getByTestId('review-pr-header')).toContainText('Trim transcript retention');
        // Escape closes the find first, then the review.
        await input.focus();
        await page.keyboard.press('Escape');
        await expect(bar).toHaveCount(0);
        await expect(review).toBeVisible();
        await expect(row(page, 202)).toHaveAttribute('data-selected', 'true');
        await page.keyboard.press('Escape');
        await expect(review).toHaveCount(0);
      } else {
        // An opened row replaces the list: find there is the detail's, and
        // it has none; Back returns to the list still filtered.
        await filterButton.focus();
        await page.keyboard.press('ControlOrMeta+f');
        await expect.poll(() => listedNumbers(pane)).toEqual([101, 104]);
        await row(page, 104).getByRole('button').first().click();
        const review = page.getByTestId('review-pane');
        await expect(review).toBeVisible();
        await expect(column).toBeHidden();
        await review.getByTestId('review-pr-header').click();
        await page.keyboard.press('ControlOrMeta+f');
        await expect(pane.locator('[data-testid="pane-find-bar"]:visible')).toHaveCount(0);
        await page.getByTestId('pulls-back').click();
        await expect(column).toBeVisible();
        await expect(bar).toBeVisible();
        await expect.poll(() => listedNumbers(pane)).toEqual([101, 104]);
        await input.focus();
        await page.keyboard.press('Escape');
        await expect(bar).toHaveCount(0);
      }

      // A tab switch applies the query to the other tab's rows.
      await filterButton.focus();
      await page.keyboard.press('ControlOrMeta+f');
      await input.fill('v3.9');
      await expect(pane.getByTestId('pulls-empty')).toHaveText('No merge requests match "v3.9".');
      await pane.getByRole('radio', { name: 'Pipelines' }).click();
      await expect(pane.getByTestId('pipeline-row')).toHaveCount(1);
      await expect(pane.getByTestId('pipeline-row')).toContainText('v3.9.2');
      await expect(input).toHaveValue('v3.9');
      await expect(input).toHaveAttribute('placeholder', 'Filter pipelines');
      await expect(bar.getByTestId('pane-find-count')).toHaveText('1 match');
      await input.fill('zzz-none');
      await expect(pane.getByTestId('pipelines-empty')).toContainText('No listed pipelines match "zzz-none".');
      await input.fill('main');
      await expect(pane.getByTestId('pipeline-row')).toHaveCount(2);
      await input.focus();
      await page.keyboard.press('Escape');
      await expect(bar).toHaveCount(0);
      // Every listed pipeline is back, the ones the query hid included.
      await expect(pane.getByTestId('pipeline-row').filter({ hasText: 'v3.9.2' })).toHaveCount(1);
      await expect(pane.getByTestId('pipeline-row').filter({ hasText: 'review' })).toHaveCount(1);
      await expect.poll(() => pane.getByTestId('pipeline-row').count()).toBeGreaterThan(2);
    } finally {
      await page.close();
      await harness.close();
    }
  });
}
