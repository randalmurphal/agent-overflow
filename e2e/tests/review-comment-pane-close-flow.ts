// A comment posted now whose pane closes while the forge holds its answer,
// then refuses it: review-comment-pane-close.spec.ts closes the thread's
// companion review with its close button on desktop, and
// compact-review-comment-pane-close.spec.ts with Back from the focused
// editor on compact. A toast names the comment and the forge's sentence;
// the PR's review, opened again, shows the editor on that line with the
// text and the sentence. The fake forge holds the post's answer
// (HarnessForgeRefuse with hold) until the pane is gone.
import type { Locator, Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect } from './fixtures.js';
import { expectEveryForgeCallHandled, forgeInvocations, openPullRequestReview, refuseForgeRoute, releaseForgeRoute } from './forge-helpers.js';
import { openEditor, ROUTES, seedPull } from './review-comment-actions-flow.js';

const FORGE = 'gitlab';
const TITLE = 'Pane close review';
const TEXT = 'Five needs a test.';

/** Every call on `route`, refused ones included. */
async function routeCalls(harness: HarnessApp, route: string): Promise<number> {
  return (await forgeInvocations(harness)).filter((call) => call.via === 'http' && call.route === route).length;
}

// Opens the thread's companion review; on compact from the sidebar the
// first time, from the thread's screen (where Back left it) after.
async function openCompanion(page: Page, compact: boolean, fromSidebar: boolean): Promise<Locator> {
  if (!compact) return openPullRequestReview(page, TITLE);
  if (fromSidebar) await page.getByTestId('thread-row').filter({ hasText: TITLE }).tap();
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
  return review;
}

export async function failAfterPaneCloses(harness: HarnessApp, page: Page, compact: boolean): Promise<void> {
  await seedPull(harness, FORGE, compact ? 'pane-close-compact' : 'pane-close', TITLE);
  await harness.open(page);
  const route = ROUTES[FORGE].lineComment;
  let review = await openCompanion(page, compact, true);
  await expect(review.getByTestId('review-diff-stats')).toBeVisible({ timeout: 60_000 });

  await refuseForgeRoute(harness, { route, status: 422, times: 1, hold: true, body: { message: 'Note could not be resolved' } });
  const editor = await openEditor(review, 'const five', compact);
  const text = editor.locator('textarea');
  await text.fill(TEXT);
  await editor.getByTestId('review-draft-editor-now').click();
  // The post is at the forge, its answer held.
  await expect.poll(() => routeCalls(harness, route)).toBe(1);

  if (compact) {
    await text.focus();
    await page.goBack();
  } else {
    await review.getByTestId('review-close').click();
  }
  await expect(page.getByTestId('review-pr-header')).toHaveCount(0);

  await releaseForgeRoute(harness, route);
  const toast = page.getByTestId('toast').filter({ hasText: 'Your comment on src/values.ts:6 did not post.' });
  await expect(toast).toBeVisible({ timeout: 30_000 });

  review = await openCompanion(page, compact, false);
  await expect(review.getByTestId('review-diff-stats')).toBeVisible({ timeout: 60_000 });
  const again = await openEditor(review, 'const five', compact);
  await expect(again.locator('textarea')).toHaveValue(TEXT);
  const sentence = (await again.getByTestId('review-editor-error').textContent())!.trim();
  expect(sentence).not.toBe('');
  await expect(toast).toContainText(sentence);
  expect(await routeCalls(harness, route)).toBe(1);
  await expectEveryForgeCallHandled(harness);
}
