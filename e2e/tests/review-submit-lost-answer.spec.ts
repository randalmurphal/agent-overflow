// A PR review submit whose answer is lost (the connection drops after the
// backend took the call) posts once. On the Pull requests page, on GitHub
// and GitLab, a review with one line comment is submitted and the socket is
// closed instead of delivering SubmitPRReview's answer: the backend posted
// the review and marked the draft sent in the same call, the pane asks the
// backend what its attempt did (GetPRReviewSubmitOutcome), ends as a
// success, and the forge holds the review once.
import { randomBytes } from 'node:crypto';
import type { Page } from '@playwright/test';

import { expect, test, type SeedResult } from './fixtures.js';
import { expectEveryForgeCallHandled, forgeInvocations, publishPullRequest, type ForgeRepo } from './forge-helpers.js';
import { launchPullsHarness, openPage, pickFilter, row } from './pulls-page-flow.js';

const SUBMIT_PR_REVIEW = 2692607191;
const PUBLISH: Record<ForgeRepo['forge'], { route: string }> = {
  gitlab: { route: 'glab api publish draft notes' },
  github: { route: 'gh api pull reviews' },
};

// Closes the page's socket in place of the first SubmitPRReview answer.
async function loseTheSubmitAnswer(page: Page): Promise<{ lost: () => boolean }> {
  let lost = false;
  let submitId: unknown;
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const server = socket.connectToServer();
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message));
      if (frame.type === 'rpc' && frame.methodId === SUBMIT_PR_REVIEW && submitId === undefined) submitId = frame.id;
      server.send(message);
    });
    server.onMessage((message) => {
      const frame = JSON.parse(String(message));
      if (!lost && submitId !== undefined && frame.type === 'rpc' && frame.id === submitId) {
        lost = true;
        void socket.close();
        return;
      }
      socket.send(message);
    });
  });
  return { lost: () => lost };
}

for (const forge of ['gitlab', 'github'] as const) {
  test(`a lost submit answer on ${forge} posts the review once and ends as a success`, async ({ page }) => {
    test.setTimeout(120_000);
    const harness = await launchPullsHarness();
    try {
      const seed = await harness.rpc<SeedResult>('HarnessSeed', {
        projects: [{ name: `submit-lost-${forge}`, repo: { commits: [{ message: 'init', files: { 'README.md': '# Submit\n' } }] }, threads: [] }],
      });
      await publishPullRequest(harness, seed.projects[0].path, {
        forge,
        project: `ao-e2e/submit-lost-${randomBytes(4).toString('hex')}`,
        pulls: [{ number: 7, title: 'Values', author: 'octo' }],
      }, { 'src/values.ts': 'export const one = 1;\nexport const two = 2;\n' });
      const wire = await loseTheSubmitAnswer(page);
      await harness.open(page);
      const pane = await openPage(page);
      await pickFilter(page, pane, 'all');
      await row(page, 7).getByRole('button').first().click();
      const review = page.getByTestId('review-pane');
      await expect(review.getByTestId('review-diff-stats')).toContainText('1 file', { timeout: 60_000 });

      const line = review.getByTestId('review-line-block').first();
      await line.hover();
      await review.getByTestId('review-add-comment').first().click();
      const editor = review.getByTestId('review-draft-editor');
      await editor.locator('textarea').fill('Guard this.');
      await editor.getByTestId('review-draft-editor-add').click();
      const strip = review.getByTestId('review-send-strip');
      await expect(strip).toBeVisible();

      await review.getByTestId('review-send-comments').click();
      await expect.poll(wire.lost).toBe(true);
      // The pane asks what the attempt did: it posted, the strip goes and
      // nothing says the submit failed.
      await expect(strip).toHaveCount(0, { timeout: 30_000 });
      await expect(review.getByText('may already be on the PR')).toHaveCount(0);
      const published = async () => (await forgeInvocations(harness))
        .filter((inv) => inv.via === 'http' && inv.route === PUBLISH[forge].route && inv.status < 300).length;
      await expect.poll(published).toBe(1);
      await expectEveryForgeCallHandled(harness);
    } finally {
      await page.close();
      await harness.close();
    }
  });
}
