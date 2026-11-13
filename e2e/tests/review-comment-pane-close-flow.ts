// A comment posted now whose pane closes while the forge holds its answer:
// review-comment-pane-close.spec.ts closes the thread's companion review
// with its close button on desktop, compact-review-comment-pane-close.spec.ts
// with Back from the focused editor on compact. The fake forge holds the
// post's answer (HarnessForgeRefuse with hold) until the pane is gone,
// then refuses it or loses it. One toast says what became of the comment,
// with the forge's sentence; it stays until dismissed, and Copy text puts
// the comment's text on the clipboard. The review opened again has a fresh
// editor on that line.
import type { Locator, Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect } from './fixtures.js';
import { expectEveryForgeCallHandled, forgeInvocations, openPullRequestReview, refuseForgeRoute, releaseForgeRoute } from './forge-helpers.js';
import { openEditor, ROUTES, seedPull } from './review-comment-actions-flow.js';

const FORGE = 'gitlab';
const TITLE = 'Pane close review';
const TEXT = 'Five needs a test.';
const DEFAULT_TOAST_MS = 5_000;

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

/** `refused`: the forge refuses the post; `lost`: its answer is lost, so
 * the post may have landed. */
export async function failAfterPaneCloses(harness: HarnessApp, page: Page, compact: boolean, outcome: 'refused' | 'lost'): Promise<void> {
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], { origin: new URL(harness.url).origin });
  await seedPull(harness, FORGE, `pane-close-${compact ? 'compact' : 'desktop'}-${outcome}`, TITLE);
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

  await releaseForgeRoute(harness, route, outcome === 'lost');
  const head = outcome === 'refused' ? 'Your comment on src/values.ts:6 was not sent. ' : 'Your comment on src/values.ts:6 may have been posted. ';
  const toast = page.getByTestId('toast').filter({ hasText: head });
  await expect(toast).toHaveCount(1, { timeout: 30_000 });
  const message = (await toast.getByTestId('toast-message').textContent())!;
  // The forge's sentence follows; a lost answer never reads as not sent.
  expect(message.length).toBeGreaterThan(head.length + 10);
  if (outcome === 'lost') {
    expect(message).toMatch(/Check the pull request before sending it again\.$/);
    expect(message).not.toContain('not sent');
  }

  // Copy text puts the whole comment on the clipboard and the toast stays.
  await toast.getByRole('button', { name: 'Copy text' }).click();
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(TEXT);
  await expect(toast).toHaveCount(1);
  // Held past an ordinary toast's lifetime: the property under test is time.
  await page.waitForTimeout(DEFAULT_TOAST_MS + 1_000);
  await expect(toast).toHaveCount(1);

  // The review opened again has a fresh editor on that line.
  review = await openCompanion(page, compact, false);
  await expect(review.getByTestId('review-diff-stats')).toBeVisible({ timeout: 60_000 });
  const again = await openEditor(review, 'const five', compact);
  await expect(again.locator('textarea')).toHaveValue('');
  await expect(again.getByTestId('review-editor-error')).toHaveCount(0);
  await expect(toast).toHaveCount(1);
  expect(await routeCalls(harness, route)).toBe(1);
  await expectEveryForgeCallHandled(harness);
}
