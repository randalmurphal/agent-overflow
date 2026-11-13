// A pull request's image diff across a transport reconnect, on GitHub,
// through the shipped UI and the real backend: the page's WebSocket is
// dropped with an image row on screen and comes back; an image row read
// after it (and the one on screen, remounted) still shows its sides, and
// no read is retried in a loop. The open answered a files handle the
// image reads name; one the server no longer holds after the reconnect is
// reopened once (stores/reviewDiffFiles.ts).

import type { WebSocketRoute } from '@playwright/test';

import { expect, test } from './fixtures.js';
import { expectEveryForgeCallHandled } from './forge-helpers.js';
import { openReview, reach, seed } from './review-diff-reading-flow.js';

test('image rows read after a reconnect still show their sides', async ({ harness, page }) => {
  test.setTimeout(120_000);
  await page.emulateMedia({ colorScheme: 'dark' });
  let socket: { page: WebSocketRoute; server: WebSocketRoute } | null = null;
  let opens = 0;
  const reads: string[] = [];
  await page.routeWebSocket(/\/ws(?:\?|$)/, (route) => {
    const server = route.connectToServer();
    socket = { page: route, server };
    route.onMessage((message) => {
      const text = String(message);
      if (text.includes('"OpenPRDiff"')) opens += 1;
      for (const method of ['GetReviewDiffImage', 'GetReviewDiffFileSizes']) if (text.includes(`"${method}"`)) reads.push(method);
      server.send(message);
    });
    server.onMessage((message) => route.send(message));
  });
  await seed(harness, 'github');
  await harness.open(page);
  const review = await openReview(page, 'github', false);
  const logo = await reach(review, 'logo.png', review.locator('[data-testid="review-image-diff"][data-path="logo.png"]'), false);
  await expect(logo.getByTestId('review-image')).toHaveCount(2, { timeout: 30_000 });
  const opensBefore = opens;

  // The connection drops and the page reconnects on its own.
  const dropped = socket!;
  await dropped.page.close({ code: 1012, reason: 'test reconnect' });
  await dropped.server.close();
  await expect.poll(() => socket !== dropped).toBe(true);
  await expect(page.getByTestId('transport-status-banner')).toHaveCount(0, { timeout: 30_000 });

  const readsBefore = reads.length;
  const icon = await reach(review, 'icon.png', review.locator('[data-testid="review-image-diff"][data-path="icon.png"]'), false);
  await expect(icon.getByTestId('review-image')).toHaveCount(1, { timeout: 30_000 });
  await expect(icon.getByTestId('review-image-caption')).toContainText('24 × 12 px');
  const old = await reach(review, 'old.png', review.locator('[data-testid="review-image-diff"][data-path="old.png"]'), false);
  await expect(old.getByTestId('review-image')).toHaveCount(1, { timeout: 30_000 });
  // Each row read at most twice (a refused handle, then the reopened one),
  // and the diff opened at most once more for the whole page.
  expect(reads.length - readsBefore).toBeLessThanOrEqual(4 + 2);
  expect(opens - opensBefore).toBeLessThanOrEqual(1);
  await expect(review.getByTestId('review-binary-reason')).toHaveCount(0);
  await expectEveryForgeCallHandled(harness);
});
