// Find inside the review pane under the compact layout: with the review on
// screen, mod+f opens its bar under the toolbar, the count covers a match
// the virtualizer has not mounted, the query brings it on screen painted
// current, and Back closes the find before it closes the review. Desktop
// coverage: review-find.spec.ts.
import { randomBytes } from 'node:crypto';
import type { Page } from '@playwright/test';

import { expect, test, type SeedResult } from './fixtures.js';
import { expectEveryForgeCallHandled, publishPullRequest } from './forge-helpers.js';

const TITLE = 'Phone review find';
const NEEDLE = 'needle-phone';

function painted(page: Page, name: string): Promise<string[]> {
  return page.evaluate((n) => {
    const highlight = CSS.highlights.get(n);
    return highlight ? [...highlight].map((range) => range.toString()) : [];
  }, name);
}

test('mod+f finds in the review on screen and Back closes the find first', async ({ harness, page }) => {
  test.setTimeout(90_000);
  const seeded = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: `compact-review-find-${randomBytes(4).toString('hex')}`,
      repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
      threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
    }],
  });
  const body = Array.from({ length: 2000 }, (_, i) => (i === 1500 ? `export const found = '${NEEDLE}';` : `export const value${i} = ${i};`)).join('\n') + '\n';
  await publishPullRequest(harness, seeded.projects[0].path, {
    forge: 'gitlab',
    project: `ao-e2e/compact-review-find-${randomBytes(4).toString('hex')}`,
    pulls: [{ number: 7, title: 'Values', body: '' }],
  }, { 'src/values.ts': body });
  await harness.open(page);

  await page.getByTestId('thread-row').filter({ hasText: TITLE }).tap();
  const more = page.getByTestId('chat-header-more');
  const menu = page.locator('[data-popover]');
  const prRow = menu.getByRole('menuitem', { name: 'MR !7' });
  await expect(async () => {
    if ((await menu.count()) > 0) await page.keyboard.press('Escape');
    await expect(menu).toHaveCount(0);
    await more.tap();
    await expect(prRow).toBeVisible({ timeout: 2_000 });
  }).toPass({ timeout: 30_000 });
  await prRow.tap();
  const review = page.locator('section[data-pane-kind="review"]');
  await expect(review.getByTestId('review-pr-header')).toBeVisible();
  await expect(review).toBeInViewport({ ratio: 1 });
  await expect(review.getByTestId('review-diff-stats')).toContainText('1 file', { timeout: 30_000 });
  await expect(review.getByText(NEEDLE)).toHaveCount(0);

  await page.keyboard.press('ControlOrMeta+f');
  const bar = review.getByTestId('pane-find-bar');
  await expect(bar).toBeVisible();
  const toolbarBox = (await review.getByTestId('review-toolbar').boundingBox())!;
  const barBox = (await bar.boundingBox())!;
  expect(barBox.y).toBeGreaterThanOrEqual(toolbarBox.y + toolbarBox.height - 1);
  await review.getByTestId('pane-find-input').fill(NEEDLE);
  await expect(bar.getByTestId('pane-find-count')).toHaveText('1/1');
  const id = await review.getAttribute('data-pane-id');
  const name = `ao-find-${id!.replace(/[^A-Za-z0-9_-]/g, '_')}`;
  await expect.poll(() => painted(page, `${name}-current`)).toEqual([NEEDLE]);
  await expect(review.getByText(NEEDLE)).toBeInViewport();

  await page.goBack();
  await expect(bar).toHaveCount(0);
  await expect(review).toBeInViewport({ ratio: 1 });
  await expect.poll(() => painted(page, name)).toEqual([]);
  await page.goBack();
  await expect(review.getByTestId('review-pr-header')).toHaveCount(0);
  await expectEveryForgeCallHandled(harness);
});
