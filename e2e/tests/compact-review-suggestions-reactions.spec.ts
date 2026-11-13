// Suggestions, reactions, Preview and Copy link on compact, on GitLab
// against the fake forge: a comment's hover actions show without hover;
// a chip toggles and the popover adds a reaction by tap; a new comment's
// Suggest change and Preview fit the editor, its button row wrapping; Apply lands a
// commit and the stale-head banner takes over; Copy link fills the
// clipboard. The fixture and steps are in
// review-suggestions-reactions-flow.ts; review-suggestions-reactions.spec.ts
// is the desktop twin on both forges.
import { expect, test } from './fixtures.js';
import { expectEveryForgeCallHandled, forgeRouteCalls } from './forge-helpers.js';
import { centre, chip, closeAll, openEditor, openPinned, ROUTES, SUGGEST_ID, thread } from './review-suggestions-reactions-flow.js';

const FORGE = 'gitlab';

test('on compact, reactions, suggestions, Preview and Copy link work by tap', async ({ page }, testInfo) => {
  test.setTimeout(180_000);
  await page.emulateMedia({ colorScheme: 'dark' });
  const { harness, review } = await openPinned(page, FORGE, 'compact-suggest', true, { clipboard: true });
  try {
    const card = thread(review, 'A half reads better');
    const header = card.locator('[data-first-comment]').first();
    const actions = header.getByTestId('review-comment-actions');
    // Nothing hovers on a touch screen: the actions are always shown, and
    // fit the header.
    await expect(actions.locator('> span')).toHaveCSS('opacity', '1', { timeout: 30_000 });
    const headBox = (await header.boundingBox())!;
    for (const button of await actions.locator('button').all()) {
      const box = (await button.boundingBox())!;
      expect(box.x + box.width).toBeLessThanOrEqual(headBox.x + headBox.width + 0.5);
      expect(box.x).toBeGreaterThanOrEqual(headBox.x - 0.5);
    }
    await centre(card);
    await page.screenshot({ path: testInfo.outputPath('compact-reactions.png') });

    // A chip toggles by tap; the popover adds one.
    const up = chip(card, 'thumbsup');
    await up.tap();
    await expect(up).toHaveAttribute('aria-pressed', 'true', { timeout: 30_000 });
    await expect(up).toHaveText(/2/);
    await actions.getByTestId('review-comment-react').tap();
    const picker = page.getByTestId('review-reaction-picker');
    await expect(picker.getByTestId('review-reaction-option')).toHaveCount(8);
    await page.screenshot({ path: testInfo.outputPath('compact-reaction-popover.png') });
    await picker.locator('[data-reaction="eyes"]').tap();
    await expect(chip(card, 'eyes')).toHaveAttribute('aria-pressed', 'true', { timeout: 30_000 });
    expect(await forgeRouteCalls(harness, ROUTES[FORGE].react)).toHaveLength(2);

    // Copy link.
    await actions.getByTestId('review-comment-copy-link').tap();
    await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toMatch(new RegExp(`#note_${SUGGEST_ID}$`));

    // A new comment's Suggest change and Preview sit in its button row.
    const editor = await openEditor(review, 'const five', true);
    await editor.getByTestId('review-editor-suggest').tap();
    await expect(editor.locator('textarea')).toHaveValue('```suggestion\nexport const five = 5;\n```');
    // The row wraps whole groups: every button stays inside the editor.
    const box = (await editor.boundingBox())!;
    for (const button of await editor.locator('[data-editor-control]').all()) {
      const b = (await button.boundingBox())!;
      expect(b.x).toBeGreaterThanOrEqual(box.x - 0.5);
      expect(b.x + b.width).toBeLessThanOrEqual(box.x + box.width + 0.5);
    }
    await centre(editor);
    await page.screenshot({ path: testInfo.outputPath('compact-editor-suggestion.png') });
    await editor.getByTestId('review-editor-preview-toggle').tap();
    await expect(editor.getByTestId('review-editor-preview').getByTestId('review-suggestion-removed')).toHaveText(['export const five = 5;']);
    await page.screenshot({ path: testInfo.outputPath('compact-editor-preview.png') });
    await editor.getByTestId('review-editor-preview-toggle').tap();
    await editor.getByRole('button', { name: 'Cancel' }).tap();

    // Apply: the commit lands and the stale-head banner takes over.
    const suggestion = card.getByTestId('review-suggestion');
    await centre(card);
    await page.screenshot({ path: testInfo.outputPath('compact-suggestion.png') });
    await suggestion.getByTestId('review-suggestion-apply').tap();
    await expect(review.getByTestId('review-pr-stale')).toBeVisible({ timeout: 30_000 });
    expect(await forgeRouteCalls(harness, ROUTES[FORGE].apply)).toHaveLength(1);
    await expectEveryForgeCallHandled(harness);
  } finally {
    await closeAll(page, harness);
  }
});
