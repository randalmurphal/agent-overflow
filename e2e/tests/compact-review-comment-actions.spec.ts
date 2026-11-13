// The comment actions on compact, on GitLab against the fake forge: a sent
// comment's actions are one `⋯` menu, opened by tap or long press, a
// reply posts now, a draft is added and sent as a review, a sent comment
// is edited, and the in-place delete confirm fits its header at compact
// width. The fixture and steps
// are in review-comment-actions-flow.ts; review-comment-actions.spec.ts is
// the desktop twin on both forges.
import { expect, test } from './fixtures.js';
import { expectEveryForgeCallHandled, forgeRouteCalls } from './forge-helpers.js';
import { centre, closeAll, openEditor, openPinned, openReply, ROUTES, thread } from './review-comment-actions-flow.js';
import { longPress } from './touch-helpers.js';

const FORGE = 'gitlab';

test('on compact, comment actions are one menu, and reply, draft, edit and delete work by tap and long press', async ({ page }, testInfo) => {
  test.setTimeout(150_000);
  await page.emulateMedia({ colorScheme: 'dark' });
  const { harness, review } = await openPinned(page, FORGE, 'compact', true);
  try {
    const card = thread(review, 'Why start at one?');
    const mine = card.getByTestId('review-thread-comment').filter({ hasText: 'Counting starts there.' });
    await expect(mine).toBeVisible({ timeout: 30_000 });
    // Nothing hovers on a touch screen: the actions are one `⋯`, always shown.
    await expect(mine.getByTestId('review-comment-menu')).toBeVisible();
    await expect(mine.getByTestId('review-comment-edit')).toHaveCount(0);
    await centre(card);
    await card.screenshot({ path: testInfo.outputPath('compact-comment-actions.png') });

    // Reply now.
    const composer = await openReply(review, 'Why start at one?', true);
    await composer.locator('textarea').fill('Agreed, keep it.');
    await composer.getByTestId('review-thread-reply-now').tap();
    await expect(card.getByTestId('review-thread-comment').filter({ hasText: 'Agreed, keep it.' })).toBeVisible({ timeout: 30_000 });
    expect(await forgeRouteCalls(harness, ROUTES[FORGE].reply)).toHaveLength(1);

    // Add a draft, then send it as the review.
    const editor = await openEditor(review, 'const half', true);
    await editor.locator('textarea').fill('Is a half needed?');
    await editor.getByTestId('review-draft-editor-add').tap();
    const strip = review.getByTestId('review-send-strip');
    await expect(strip).toContainText('1 draft');
    await review.getByTestId('review-send-comments').tap();
    await expect(strip).toHaveCount(0, { timeout: 30_000 });
    await expect(thread(review, 'Is a half needed?')).toBeVisible({ timeout: 30_000 });
    expect(await forgeRouteCalls(harness, ROUTES[FORGE].publish)).toHaveLength(1);

    // Edit a sent comment.
    await mine.getByTestId('review-comment-menu').tap();
    await page.getByTestId('review-comment-edit').tap();
    const edit = card.getByTestId('review-comment-edit-editor');
    await edit.locator('textarea').fill('Counting starts at one.');
    await edit.getByTestId('review-comment-save').tap();
    const edited = card.getByTestId('review-thread-comment').filter({ hasText: 'Counting starts at one.' });
    await expect(edited).toBeVisible({ timeout: 30_000 });
    expect(await forgeRouteCalls(harness, ROUTES[FORGE].edit)).toHaveLength(1);

    // The delete confirm sits inside the header, its controls whole.
    // Delete from a long press on the comment's text: the same menu.
    await longPress(page, edited.getByText('Counting starts at one.'), page.getByTestId('review-comment-delete'));
    await page.getByTestId('review-comment-delete').tap();
    const confirm = edited.getByTestId('review-comment-confirm');
    await expect(confirm).toContainText('Delete?');
    const header = (await edited.getByTestId('review-comment-header').boundingBox())!;
    for (const part of [confirm, confirm.getByTestId('review-comment-confirm-yes'), confirm.getByTestId('review-comment-confirm-cancel')]) {
      const box = (await part.boundingBox())!;
      expect(box.x).toBeGreaterThanOrEqual(header.x - 0.5);
      expect(box.x + box.width).toBeLessThanOrEqual(header.x + header.width + 0.5);
      expect(box.y).toBeGreaterThanOrEqual(header.y - 0.5);
      expect(box.y + box.height).toBeLessThanOrEqual(header.y + header.height + 0.5);
    }
    await centre(card);
    await card.screenshot({ path: testInfo.outputPath('compact-delete-confirm.png') });
    await confirm.getByTestId('review-comment-confirm-yes').tap();
    await expect(card.getByTestId('review-thread-comment').filter({ hasText: 'Counting starts at one.' })).toHaveCount(0, { timeout: 30_000 });
    expect(await forgeRouteCalls(harness, ROUTES[FORGE].delete)).toHaveLength(1);
    await expectEveryForgeCallHandled(harness);
  } finally {
    await closeAll(page, harness);
  }
});
