// Suggested changes, reactions, the editors' Preview and Copy link in the
// review pane, on GitHub and GitLab against the fake forge: a suggestion
// written on a range with Suggest change (its fence holding the lines'
// current text), previewed as the diff it will become and posted, then
// rendered from the forge's answer; Apply landing a commit on the head
// branch, the pane's stale-head banner taking over, and after Reload the
// diff at the new head showing the suggested line and the thread
// resolved (GitHub) or the suggestion Applied (GitLab); a refused apply's sentence under its
// suggestion; Applied and Outdated suggestions; a reaction toggled on and
// off from its chip and one added from the popover, each chip showing the
// forge's answer; GitLab's unknown awards as chips; GitLab's reactions
// read refused, shown as one line while the threads stay; a comment's
// link copied to the clipboard; and the description editor's Preview
// toggle. Every test ends with every forge call handled.
import { expect, test } from './fixtures.js';
import { expandReviewSection, expectEveryForgeCallHandled, forgeInvocations, forgeRouteCalls, refuseForgeRoute } from './forge-helpers.js';
import { centre, chip, closeAll, line, openEditor, openPinned, ROUTES, SUGGEST_ID, thread } from './review-suggestions-reactions-flow.js';

for (const forge of ['github', 'gitlab'] as const) {
  const routes = ROUTES[forge];

  test(`on ${forge}, a suggestion is written on a range, previewed as its diff and posted`, async ({ page }, testInfo) => {
    test.setTimeout(150_000);
    await page.emulateMedia({ colorScheme: 'dark' });
    const { harness, review } = await openPinned(page, forge, 'suggest-write');
    try {
      const editor = await openEditor(review, 'const two');
      const end = line(review, 'const three');
      await end.hover();
      await end.getByTestId('review-add-comment').click({ modifiers: ['Shift'] });
      await expect(editor.getByTestId('review-draft-editor-range')).toContainText('Lines 2–4');

      // Suggest change inserts the lines' current text at the caret.
      const text = editor.locator('textarea');
      await text.fill('Fold these:\n');
      await editor.getByTestId('review-editor-suggest').click();
      await expect(text).toHaveValue('Fold these:\n```suggestion\nexport const two = 2;\nexport const half = 2.5;\nexport const three = 3;\n```');
      await expect(text).toBeFocused();
      // The caret sits inside the block, at the end of its content.
      expect(await text.evaluate((el: HTMLTextAreaElement) => el.value.slice(0, el.selectionStart))).toBe('Fold these:\n```suggestion\nexport const two = 2;\nexport const half = 2.5;\nexport const three = 3;');
      await text.fill('Fold these:\n```suggestion\nexport const two = 2;\nexport const three = 3;\n```');
      await centre(editor);
      await editor.screenshot({ path: testInfo.outputPath(`${forge}-editor-suggestion.png`) });

      // Preview renders the block as the diff it will become, and keeps
      // the editor's height; off returns the text and the caret.
      await text.evaluate((el: HTMLTextAreaElement) => el.setSelectionRange(5, 5));
      const height = (await text.boundingBox())!.height;
      await editor.getByTestId('review-editor-preview-toggle').click();
      const preview = editor.getByTestId('review-editor-preview');
      await expect(preview).toContainText('Fold these:');
      const diff = preview.getByTestId('review-suggestion');
      await expect(diff.getByTestId('review-suggestion-removed')).toHaveText(['export const two = 2;', 'export const half = 2.5;', 'export const three = 3;']);
      await expect(diff.getByTestId('review-suggestion-added')).toHaveText(['export const two = 2;', 'export const three = 3;']);
      // The fence is the diff, not also a code block.
      await expect(preview.locator('pre')).toHaveCount(0);
      expect((await preview.boundingBox())!.height).toBeGreaterThanOrEqual(height - 0.5);
      await editor.screenshot({ path: testInfo.outputPath(`${forge}-editor-preview.png`) });
      await editor.getByTestId('review-editor-preview-toggle').click();
      await expect(text).toBeFocused();
      expect(await text.evaluate((el: HTMLTextAreaElement) => [el.selectionStart, el.selectionEnd])).toEqual([5, 5]);

      await editor.getByTestId('review-draft-editor-now').click();
      const posted = thread(review, 'Fold these:');
      await expect(posted).toBeVisible({ timeout: 30_000 });
      const sent = posted.getByTestId('review-suggestion');
      await expect(sent.getByTestId('review-suggestion-added')).toHaveText(['export const two = 2;', 'export const three = 3;']);
      // GitLab reports the replaced text; GitHub's comes from the diff.
      await expect(sent.getByTestId('review-suggestion-removed')).toHaveText(['export const two = 2;', 'export const half = 2.5;', 'export const three = 3;']);
      await expect(posted.locator('pre')).toHaveCount(0);
      const calls = await forgeRouteCalls(harness, routes.lineComment);
      expect(calls).toHaveLength(1);
      expect(JSON.stringify(calls[0].body)).toContain('```suggestion');
      await expectEveryForgeCallHandled(harness);
    } finally {
      await closeAll(page, harness);
    }
  });

  test(`on ${forge}, Apply lands a commit, the head moves and the thread follows; a refusal shows under the suggestion`, async ({ page }, testInfo) => {
    test.setTimeout(150_000);
    await page.emulateMedia({ colorScheme: 'dark' });
    const { harness, review } = await openPinned(page, forge, 'suggest-apply');
    try {
      const card = thread(review, 'A half reads better');
      const suggestion = card.getByTestId('review-suggestion');
      await expect(suggestion.getByTestId('review-suggestion-added')).toHaveText(['export const half = 5 / 2;'], { timeout: 30_000 });
      await expect(suggestion.getByTestId('review-suggestion-removed')).toHaveText(['export const half = 2.5;']);
      // The markdown around the block still renders, once.
      await expect(card).toContainText('A half reads better as a fraction:');
      await expect(card).toContainText('Your call.');
      await expect(card.locator('pre')).toHaveCount(0);
      // An outdated thread's suggestion says so and offers no Apply.
      const outdated = thread(review, 'Old code:');
      await expect(outdated.getByTestId('review-suggestion-outdated')).toBeVisible();
      await expect(outdated.getByTestId('review-suggestion-apply')).toHaveCount(0);
      if (forge === 'gitlab') {
        const applied = thread(review, 'Done already:');
        await expect(applied.getByTestId('review-suggestion-applied')).toBeVisible();
        await expect(applied.getByTestId('review-suggestion-apply')).toHaveCount(0);
        await centre(applied);
        await applied.screenshot({ path: testInfo.outputPath(`${forge}-suggestion-applied.png`) });
      }
      await centre(outdated);
      await outdated.screenshot({ path: testInfo.outputPath(`${forge}-suggestion-outdated.png`) });
      await centre(card);
      await card.screenshot({ path: testInfo.outputPath(`${forge}-suggestion-apply.png`) });

      // The forge refuses the first apply: its sentence under the
      // suggestion, nothing written.
      await refuseForgeRoute(harness, forge === 'gitlab'
        ? { route: routes.apply, status: 403, times: 1, body: { message: '403 Forbidden' } }
        : { route: routes.apply, status: 200, times: 1, body: { data: { createCommitOnBranch: null }, errors: [{ type: 'FORBIDDEN', path: ['createCommitOnBranch'], message: 'ao-viewer does not have permission to push to this branch' }] } });
      await suggestion.getByTestId('review-suggestion-apply').click();
      const error = suggestion.getByTestId('review-suggestion-error');
      await expect(error).not.toBeEmpty({ timeout: 30_000 });
      await expect(review.getByTestId('review-pr-stale')).toHaveCount(0);
      await centre(card);
      await card.screenshot({ path: testInfo.outputPath(`${forge}-suggestion-refused.png`) });

      // The second goes through: the fake commits it to the pull's origin,
      // the head moves and the pane's stale-head banner takes over.
      await suggestion.getByTestId('review-suggestion-apply').click();
      await expect(review.getByTestId('review-pr-stale')).toBeVisible({ timeout: 30_000 });
      await expect(error).toHaveCount(0);
      // GitHub refuses a mutation with a 200, so its refused call counts.
      expect(await forgeRouteCalls(harness, routes.apply)).toHaveLength(forge === 'github' ? 2 : 1);
      // Reloaded, the diff is the new head's and shows the suggested line.
      await review.getByTestId('review-pr-stale').getByRole('button', { name: 'Reload' }).click();
      await expect(line(review, 'export const half = 5 / 2;')).toBeVisible({ timeout: 30_000 });
      await expect(line(review, 'export const half = 2.5;')).toHaveCount(0);
      await expect(review.getByTestId('review-pr-stale')).toHaveCount(0);
      if (forge === 'github') {
        // GitHub resolves the thread an applied suggestion came from.
        await expect(card).toHaveAttribute('data-state', 'resolved', { timeout: 30_000 });
      } else {
        await expect(card.getByTestId('review-suggestion-applied')).toBeVisible({ timeout: 30_000 });
        await expect(card.getByTestId('review-suggestion-apply')).toHaveCount(0);
      }
      await centre(line(review, 'export const half = 5 / 2;'));
      await page.screenshot({ path: testInfo.outputPath(`${forge}-after-apply.png`) });
      await expectEveryForgeCallHandled(harness);
    } finally {
      await closeAll(page, harness);
    }
  });

  test(`on ${forge}, a reaction toggles from its chip, one is added from the popover, and a comment's link is copied`, async ({ page }, testInfo) => {
    test.setTimeout(150_000);
    await page.emulateMedia({ colorScheme: 'dark' });
    const { harness, review } = await openPinned(page, forge, 'react', false, { clipboard: true });
    try {
      const card = thread(review, 'A half reads better');
      const up = chip(card, 'thumbsup');
      await expect(up).toHaveText(/1/, { timeout: 30_000 });
      await expect(up).toHaveAttribute('aria-pressed', 'false');
      await expect(chip(card, 'heart')).toHaveAttribute('aria-pressed', 'true');
      if (forge === 'gitlab') {
        await expect(chip(card, 'fire')).toContainText('🔥');
        await expect(chip(card, 'party_parrot')).toContainText(':party_parrot:');
      }

      // React, and the chip shows the forge's answer; then un-react.
      await up.click();
      await expect(up).toHaveAttribute('aria-pressed', 'true', { timeout: 30_000 });
      await expect(up).toHaveText(/2/);
      expect(await forgeRouteCalls(harness, routes.react)).toHaveLength(1);
      await up.click();
      await expect(up).toHaveAttribute('aria-pressed', 'false', { timeout: 30_000 });
      await expect(up).toHaveText(/1/);
      expect(await forgeRouteCalls(harness, routes.unreact)).toHaveLength(1);

      // The hover actions, in order, and the popover's eight.
      const header = card.locator('[data-first-comment]').first();
      await card.locator('[data-first-comment]').last().hover();
      const actions = header.getByTestId('review-comment-actions');
      await expect(actions.locator('> span')).toHaveCSS('opacity', '1');
      // GitHub lets a WRITE viewer edit and delete another's comment;
      // GitLab only a maintainer.
      const labels = forge === 'github' ? ['Add reaction', 'Copy link', 'Edit', 'Delete'] : ['Add reaction', 'Copy link'];
      await expect(actions.locator('button')).toHaveCount(labels.length);
      expect(await actions.locator('button').evaluateAll((els) => els.map((el) => el.getAttribute('aria-label')))).toEqual(labels);
      await centre(card);
      await card.screenshot({ path: testInfo.outputPath(`${forge}-reactions-hover.png`) });
      await actions.getByTestId('review-comment-react').click();
      const picker = page.getByTestId('review-reaction-picker');
      await expect(picker.getByTestId('review-reaction-option')).toHaveCount(8);
      await page.screenshot({ path: testInfo.outputPath(`${forge}-reaction-popover.png`) });
      await picker.locator('[data-reaction="rocket"]').click();
      await expect(picker).toHaveCount(0);
      await expect(chip(card, 'rocket')).toHaveAttribute('aria-pressed', 'true', { timeout: 30_000 });
      await expect(chip(card, 'rocket')).toHaveText(/1/);
      expect(await forgeRouteCalls(harness, routes.react)).toHaveLength(2);

      // Copy link puts the forge's link to the comment on the clipboard.
      await card.locator('[data-first-comment]').last().hover();
      await actions.getByTestId('review-comment-copy-link').click();
      await expect.poll(() => page.evaluate(() => navigator.clipboard.readText()))
        .toMatch(forge === 'github' ? new RegExp(`/pull/7#discussion_r${SUGGEST_ID}$`) : new RegExp(`/merge_requests/7#note_${SUGGEST_ID}$`));
      await expect(actions.getByTestId('review-comment-copy-link')).toHaveAttribute('aria-label', 'Copied');
      await expectEveryForgeCallHandled(harness);
    } finally {
      await closeAll(page, harness);
    }
  });

  test(`on ${forge}, Preview in the PR comment field and the description editor`, async ({ page }, testInfo) => {
    test.setTimeout(150_000);
    await page.emulateMedia({ colorScheme: 'dark' });
    const { harness, review } = await openPinned(page, forge, 'preview');
    try {
      const conversation = await expandReviewSection(page, 'review-pr-conversation');
      await conversation.getByTestId('review-pr-comment-open').scrollIntoViewIfNeeded();
      await conversation.getByTestId('review-pr-comment-open').click();
      const field = conversation.getByTestId('review-pr-comment-composer');
      await field.locator('textarea').fill('Looks **good**.');
      await field.getByTestId('review-editor-preview-toggle').click();
      await expect(field.getByTestId('review-editor-preview').locator('strong')).toHaveText('good');
      await field.getByTestId('review-editor-preview-toggle').click();
      await expect(field.locator('textarea')).toHaveValue('Looks **good**.');

      const description = review.getByTestId('review-pr-description');
      await description.locator(':scope > div').first().hover();
      await description.getByTestId('review-description-edit').click();
      const body = description.getByTestId('review-description-input');
      await body.fill('Values, *reworked*.');
      // No Write and Preview tabs: one toggle in the button row.
      await expect(description.getByTestId('review-description-write')).toHaveCount(0);
      await description.getByTestId('review-description-editor').screenshot({ path: testInfo.outputPath(`${forge}-description-editor.png`) });
      await description.getByTestId('review-description-preview').click();
      await expect(description.getByTestId('review-description-preview-body').locator('em')).toHaveText('reworked');
      await description.getByTestId('review-description-editor').screenshot({ path: testInfo.outputPath(`${forge}-description-preview.png`) });
      await description.getByTestId('review-description-preview').click();
      await expect(body).toHaveValue('Values, *reworked*.');
      await description.getByTestId('review-description-cancel').click();
      await expectEveryForgeCallHandled(harness);
    } finally {
      await closeAll(page, harness);
    }
  });
}

test('with a thread in the Conversation and the diff, a reaction picked from either copy is sent once and both copies follow', async ({ page }, testInfo) => {
  test.setTimeout(150_000);
  const routes = ROUTES.github;
  const { harness, review } = await openPinned(page, 'github', 'react-two-copies');
  try {
    const conversation = await expandReviewSection(page, 'review-pr-conversation');
    const listed = conversation.getByTestId('review-conversation-thread').filter({ hasText: 'A half reads better' });
    const inDiff = thread(review, 'A half reads better');
    await expect(listed).toHaveCount(1, { timeout: 30_000 });
    await expect(inDiff).toHaveCount(1);
    const picker = page.getByTestId('review-reaction-picker');

    const pick = async (card: typeof listed, name: string) => {
      await card.scrollIntoViewIfNeeded();
      await card.locator('[data-first-comment]').last().hover();
      await card.locator('[data-first-comment]').first().getByTestId('review-comment-react').click();
      await expect(picker).toHaveCount(1);
      await expect(picker.getByTestId('review-reaction-option')).toHaveCount(8);
      await picker.locator(`[data-reaction="${name}"]`).click();
      await expect(picker).toHaveCount(0);
    };

    // Open from the Conversation's copy: one write, both chips show it.
    await pick(listed, 'rocket');
    for (const card of [listed, inDiff]) {
      await expect(chip(card, 'rocket')).toHaveAttribute('aria-pressed', 'true', { timeout: 30_000 });
    }
    expect(await forgeRouteCalls(harness, routes.react)).toHaveLength(1);
    await listed.screenshot({ path: testInfo.outputPath('github-reaction-two-copies.png') });

    // Open from the diff's copy: picking it again removes it, once.
    await pick(inDiff, 'rocket');
    for (const card of [listed, inDiff]) await expect(chip(card, 'rocket')).toHaveCount(0, { timeout: 30_000 });
    expect(await forgeRouteCalls(harness, routes.unreact)).toHaveLength(1);
    expect(await forgeRouteCalls(harness, routes.react)).toHaveLength(1);
    await expectEveryForgeCallHandled(harness);
  } finally {
    await closeAll(page, harness);
  }
});

test('on gitlab, a refused reactions read shows one line and the threads stay', async ({ page }, testInfo) => {
  test.setTimeout(150_000);
  await page.emulateMedia({ colorScheme: 'dark' });
  const { harness, review } = await openPinned(page, 'gitlab', 'react-refused', false, { refuseReactionsRead: true });
  try {
    const card = thread(review, 'A half reads better');
    await expect(card).toBeVisible({ timeout: 30_000 });
    await expect(card.getByTestId('review-reaction-chips')).toHaveCount(0);
    await expect(card.getByTestId('review-comment-react')).toHaveCount(0);
    const conversation = await expandReviewSection(page, 'review-pr-conversation');
    const note = conversation.getByTestId('review-reactions-unavailable');
    await expect(note).toHaveCount(1);
    await expect(note).toHaveText("Reactions are not shown: GitLab's reactions could not be read.");
    expect((await forgeInvocations(harness)).some((call) => call.via === 'http' && call.route === ROUTES.gitlab.reactionsRead && call.status === 403)).toBe(true);
    await expect(conversation.getByTestId('review-conversation-thread').first()).toBeVisible();
    await conversation.screenshot({ path: testInfo.outputPath('gitlab-reactions-unavailable.png') });
    await expectEveryForgeCallHandled(harness);
  } finally {
    await closeAll(page, harness);
  }
});
