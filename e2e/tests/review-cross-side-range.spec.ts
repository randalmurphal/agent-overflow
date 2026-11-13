// A forge thread whose range starts on a removed line and ends on an added
// one, on GitHub and GitLab against the fake forge: the forge names its
// first line by its old number on the old half (GitHub startDiffSide
// LEFT, GitLab a line_range start of type old). The card reads both ends
// with their halves, sits under its last line, and the diff tints the
// rows from the removed line through the added one, and no others.
import { randomBytes } from 'node:crypto';

import { expect, test, type SeedResult } from './fixtures.js';
import { expandReviewSection, expectEveryForgeCallHandled, publishPullRequest } from './forge-helpers.js';
import { launchPullsHarness, openPinnedReview } from './pulls-page-flow.js';

// Old lines 3 and 4 are removed; old 5 is new 3; new 4 is added. The
// range runs from old 4 through new 4: three rows.
const BASE = 'export const one = 1;\nexport const two = 2;\nexport const three = 3;\nexport const four = 4;\nexport const five = 5;\n';
const HEAD = 'export const one = 1;\nexport const two = 2;\nexport const five = 5;\nexport const six = 6;\n';

for (const forge of ['github', 'gitlab'] as const) {
  test(`on ${forge}, a range from a removed line to an added one reads and tints on its own rows`, async ({ page }) => {
    test.setTimeout(120_000);
    const harness = await launchPullsHarness();
    try {
      const seed = await harness.rpc<SeedResult>('HarnessSeed', {
        projects: [{ name: `cross-${forge}`, repo: { commits: [{ message: 'init', files: { 'src/values.ts': BASE } }] }, threads: [] }],
      });
      await publishPullRequest(harness, seed.projects[0].path, {
        forge,
        project: `ao-e2e/cross-${randomBytes(4).toString('hex')}`,
        pulls: [{
          number: 7, title: 'Values', author: 'octo',
          threads: [{
            path: 'src/values.ts', line: 4, startLine: 4, startPoint: { type: 'old', oldLine: 4 },
            comments: [{ author: 'octo', body: 'From four through six.' }],
          }],
        }],
      }, { 'src/values.ts': HEAD });
      await harness.open(page);
      const review = await openPinnedReview(page, 7);

      const card = review.getByTestId('review-pr-thread').filter({ hasText: 'From four through six.' });
      await expect(card).toBeVisible({ timeout: 30_000 });

      const tinted = review.locator('[data-review-range]');
      await expect(tinted).toHaveCount(3);
      await expect(tinted.nth(0)).toContainText('four = 4');
      await expect(tinted.nth(1)).toContainText('five = 5');
      await expect(tinted.nth(2)).toContainText('six = 6');
      // The card follows its last line.
      expect((await card.boundingBox())!.y).toBeGreaterThan((await tinted.nth(2).boundingBox())!.y);
      // The Conversation card names both ends by their halves.
      const conversation = await expandReviewSection(page, 'review-pr-conversation');
      await expect(conversation.getByTestId('review-conversation-thread').getByTestId('review-conversation-jump-diff')).toHaveText('values.ts:-4 to +4');
      await expectEveryForgeCallHandled(harness);
    } finally {
      await page.close();
      await harness.close();
    }
  });
}
