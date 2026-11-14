// An explicit jump in the review pane reveals a file the file filter hides,
// through the shipped UI, the real backend and the fake forge. With Hide
// tests on, the title bar's unresolved stepper onto a thread in a test file
// and a Conversation card's location both bring that file into the diff
// and the tree, and changing the filter hides it again.
import { randomBytes } from 'node:crypto';
import type { Locator } from '@playwright/test';

import { expect, test, type SeedResult } from './fixtures.js';
import { expectEveryForgeCallHandled, expandReviewSection, openPullRequestReview, publishPullRequest } from './forge-helpers.js';

const TITLE = 'Reveal thread';

// Enough diff below the overview that it scrolls off and the title bar
// grows its stepper.
function longSource(): string {
  return Array.from({ length: 240 }, (_, i) => `export const value${i} = ${i};`).join('\n') + '\n';
}

async function scrollPastOverview(review: Locator): Promise<void> {
  const scroller = review.getByTestId('review-scroll');
  await expect
    .poll(async () => {
      await scroller.evaluate((el) => { el.scrollTop += 600; });
      return review.getByTestId('review-overview-peek').isVisible();
    })
    .toBe(true);
}

test('a jump to a thread in a hidden test file reveals it until the filter changes', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `reveal-${randomBytes(4).toString('hex')}`,
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
      },
    ],
  });
  await publishPullRequest(harness, seed.projects[0].path, {
    forge: 'github',
    project: `ao-e2e/reveal-${randomBytes(4).toString('hex')}`,
    pulls: [{
      number: 31,
      title: 'Values and their test',
      body: '',
      threads: [{ id: 'T_test', path: 'src/values.test.ts', line: 1, comments: [{ author: 'priya-n', body: 'Assert the last value too.', createdAt: '2026-04-01T10:00:00Z' }] }],
    }],
  }, {
    'src/values.ts': longSource(),
    'src/values.test.ts': "import { value0 } from './values';\n",
  });
  await harness.open(page);
  const review = await openPullRequestReview(page, TITLE);
  const headers = review.getByTestId('review-file-header-path');
  const testHeader = headers.filter({ hasText: 'src/values.test.ts' });
  await expect(headers.first()).toBeVisible();
  const treeToggle = review.getByTestId('review-tree-toggle');
  if ((await treeToggle.getAttribute('aria-pressed')) === 'false') await treeToggle.click();
  const tree = review.getByTestId('review-file-tree');
  const testInTree = tree.locator('[data-testid="review-tree-file"][data-file-path="src/values.test.ts"]');
  await expect(testInTree).toHaveCount(1);

  const toggleHideTests = async () => {
    const menu = page.locator('[data-popover]').last();
    if ((await menu.getByTestId('review-tree-hide-tests').count()) === 0) await review.getByTestId('review-tree-ext-trigger').click();
    await menu.getByTestId('review-tree-hide-tests').click();
  };
  await toggleHideTests();
  await page.keyboard.press('Escape');
  await expect(testInTree).toHaveCount(0);
  await expect(testHeader).toHaveCount(0);

  // The title bar's stepper onto the thread in the hidden test file.
  await scrollPastOverview(review);
  const bar = review.getByTestId('review-unresolved-stepper-bar');
  await bar.getByRole('button', { name: 'Next unresolved thread' }).click();
  await expect(review.locator('[data-testid="review-pr-thread"]').filter({ hasText: 'Assert the last value too.' })).toBeInViewport();
  await expect(testHeader.first()).toBeVisible();
  await expect(testInTree).toHaveCount(1);

  // Changing the filter ends the reveal.
  await toggleHideTests();
  await toggleHideTests();
  await page.keyboard.press('Escape');
  await expect(testInTree).toHaveCount(0);
  await expect(testHeader).toHaveCount(0);

  // A Conversation card's location does the same.
  const conversation = await expandReviewSection(page, 'review-pr-conversation');
  await conversation.getByTestId('review-conversation-jump-diff').click();
  await expect(testHeader.first()).toBeVisible();
  await expect(testInTree).toHaveCount(1);
  await expectEveryForgeCallHandled(harness);
});
