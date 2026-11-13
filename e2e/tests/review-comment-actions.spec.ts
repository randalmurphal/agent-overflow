// Sending and managing PR comments from the review pane, on GitHub and
// GitLab against the fake forge: a line comment posted now (after one
// refusal that keeps the editor's text), a reply posted now and a
// PR-level comment from the Conversation; a draft and a held reply sent as
// one review; editing a sent comment, deleting a reply and deleting a
// thread's only comment; a range over unchanged and added lines posted
// with its span; and a batch with a held reply sent to the linked agent,
// whose delivered message quotes the thread. Each asserts the forge calls
// the fake answered and what the pane shows from the forge's next state,
// and that every forge call was handled.
import { expect, test } from './fixtures.js';
import { expandReviewSection, expectEveryForgeCallHandled, forgeRouteCalls, openPullRequestReview, refuseForgeRoute } from './forge-helpers.js';
import { launchPullsHarness } from './pulls-page-flow.js';
import { centre, closeAll, line, ONLY_ID, openEditor, openPinned, openReply, REPLY_ID, ROOT_ID, ROUTES, seedPull, thread } from './review-comment-actions-flow.js';

for (const forge of ['github', 'gitlab'] as const) {
  const routes = ROUTES[forge];

  test(`on ${forge}, a line comment, a reply and a PR comment post now, and a refusal keeps the text`, async ({ page }, testInfo) => {
    test.setTimeout(150_000);
    await page.emulateMedia({ colorScheme: 'dark' });
    const { harness, review } = await openPinned(page, forge, 'now');
    try {
      // The forge refuses the first post: the editor keeps its text and
      // shows the forge's sentence; the second post goes up.
      await refuseForgeRoute(harness, {
        route: routes.lineComment, status: 422, times: 1,
        body: forge === 'github' ? { message: 'Validation Failed', errors: ['line could not be resolved'] } : { message: 'Note could not be resolved' },
      });
      const editor = await openEditor(review, 'const five');
      const text = editor.locator('textarea');
      await expect(text).toHaveAttribute('placeholder', /\+Enter to comment now\)/);
      await text.fill('Five needs a test.');
      await editor.getByTestId('review-draft-editor-now').click();
      await expect(editor.getByTestId('review-editor-error')).not.toBeEmpty();
      await expect(text).toHaveValue('Five needs a test.');
      await expect(review.getByTestId('review-send-strip')).toHaveCount(0);

      await editor.getByTestId('review-draft-editor-now').click();
      await expect(review.getByTestId('review-draft-editor')).toHaveCount(0, { timeout: 30_000 });
      await expect(thread(review, 'Five needs a test.')).toBeVisible();
      await expect.poll(async () => (await forgeRouteCalls(harness, routes.lineComment)).length).toBe(1);
      // Nothing was held: posting now never makes a draft.
      await expect(review.getByTestId('review-send-strip')).toHaveCount(0);

      // A reply now, under the thread it answers.
      const composer = await openReply(review, 'Why start at one?');
      await composer.locator('textarea').fill('Agreed, keep it.');
      await composer.getByTestId('review-thread-reply-now').click();
      await expect(thread(review, 'Why start at one?').getByTestId('review-thread-comment').filter({ hasText: 'Agreed, keep it.' })).toBeVisible({ timeout: 30_000 });
      await expect(thread(review, 'Why start at one?').getByTestId('review-thread-composer')).toHaveCount(0);
      const replies = await forgeRouteCalls(harness, routes.reply);
      expect(replies).toHaveLength(1);
      if (forge === 'github') expect(replies[0].path).toMatch(new RegExp(`/comments/${ROOT_ID}/replies$`));

      // A comment on the PR itself, from the end of the Conversation feed.
      const conversation = await expandReviewSection(page, 'review-pr-conversation');
      await conversation.getByTestId('review-pr-comment-open').scrollIntoViewIfNeeded();
      await conversation.screenshot({ path: testInfo.outputPath(`${forge}-conversation-comment-field.png`) });
      await conversation.getByTestId('review-pr-comment-open').click();
      const field = conversation.getByTestId('review-pr-comment-composer');
      await field.locator('textarea').fill('Ready once CI is green.');
      await field.getByTestId('review-pr-comment-now').click();
      await expect(conversation.getByTestId('review-pr-comment-composer')).toHaveCount(0, { timeout: 30_000 });
      await expect(conversation.getByTestId('review-conversation')).toContainText('Ready once CI is green.');
      expect(await forgeRouteCalls(harness, routes.prComment)).toHaveLength(1);
      await expectEveryForgeCallHandled(harness);
    } finally {
      await closeAll(page, harness);
    }
  });

  test(`on ${forge}, a draft and a held reply go up as one review`, async ({ page }, testInfo) => {
    test.setTimeout(150_000);
    await page.emulateMedia({ colorScheme: 'dark' });
    const { harness, review } = await openPinned(page, forge, 'hold');
    try {
      const editor = await openEditor(review, 'const half');
      await editor.locator('textarea').fill('Is a half needed?');
      await editor.getByTestId('review-draft-editor-add').click();
      const strip = review.getByTestId('review-send-strip');
      await expect(strip).toContainText('1 draft');

      // With a draft held, the reply's primary action holds too.
      const composer = await openReply(review, 'Why start at one?');
      await expect(composer.locator('textarea')).toHaveAttribute('placeholder', /\+Enter to add draft\)/);
      await composer.locator('textarea').fill('Held until the review.');
      await composer.locator('textarea').press('ControlOrMeta+Enter');
      const held = thread(review, 'Why start at one?').getByTestId('review-held-replies');
      await expect(held).toContainText('Held until the review.');
      await centre(thread(review, 'Why start at one?'));
      await thread(review, 'Why start at one?').screenshot({ path: testInfo.outputPath(`${forge}-held-reply.png`) });
      await expect(strip).toContainText('2 drafts');
      expect(await forgeRouteCalls(harness, routes.reply)).toHaveLength(0);

      await review.getByTestId('review-send-comments').click();
      await expect(strip).toHaveCount(0, { timeout: 30_000 });
      // The forge's next state: the reply is a comment on its thread and
      // the draft is a thread of its own.
      await expect(thread(review, 'Why start at one?').getByTestId('review-thread-comment').filter({ hasText: 'Held until the review.' })).toBeVisible({ timeout: 30_000 });
      await expect(thread(review, 'Why start at one?').getByTestId('review-held-replies')).toHaveCount(0);
      await expect(thread(review, 'Is a half needed?')).toBeVisible();
      expect(await forgeRouteCalls(harness, routes.publish)).toHaveLength(1);
      const replies = await forgeRouteCalls(harness, routes.heldReply);
      if (forge === 'github') {
        expect(replies).toHaveLength(1);
        expect(replies[0].path).toMatch(new RegExp(`/comments/${ROOT_ID}/replies$`));
      } else {
        // The line comment and the reply are both draft notes.
        expect(replies).toHaveLength(2);
      }
      await expectEveryForgeCallHandled(harness);
    } finally {
      await closeAll(page, harness);
    }
  });

  test(`on ${forge}, a sent comment is edited, a reply deleted and a thread's only comment deleted`, async ({ page }, testInfo) => {
    test.setTimeout(150_000);
    await page.emulateMedia({ colorScheme: 'dark' });
    const { harness, review } = await openPinned(page, forge, 'edit');
    try {
      const card = thread(review, 'Why start at one?');
      const mine = card.getByTestId('review-thread-comment').filter({ hasText: 'Counting starts there.' });
      // GitHub lets a viewer with write access (the fixture's default)
      // edit another user's comment; GitLab only its author.
      await expect(card.locator('[data-testid="review-comment-edit"]')).toHaveCount(forge === 'github' ? 2 : 1);
      await centre(card);
      await mine.hover();
      await expect(mine.locator('[data-testid="review-comment-actions"] > span')).toHaveCSS('opacity', '1');
      // A reply's hover reveals only its own actions, not the first comment's.
      if (forge === 'github') await expect(card.locator('[data-first-comment] [data-testid="review-comment-actions"] > span')).toHaveCSS('opacity', '0');
      await card.screenshot({ path: testInfo.outputPath(`${forge}-comment-hover-actions.png`) });
      await mine.getByTestId('review-comment-edit').click();
      // The open edit holds the text in its field, so find it by the card.
      const edit = card.getByTestId('review-comment-edit-editor');
      await expect(edit.locator('textarea')).toHaveValue('Counting starts there.');
      await edit.locator('textarea').fill('Counting starts at one.');
      await edit.getByTestId('review-comment-save').click();
      const edited = card.getByTestId('review-thread-comment').filter({ hasText: 'Counting starts at one.' });
      await expect(edited).toBeVisible({ timeout: 30_000 });
      // GitHub says when a body was edited (lastEditedAt); GitLab's notes
      // do not (their updated_at also moves on a resolve), so no marker.
      await expect(edited.getByTestId('review-comment-edited')).toHaveCount(forge === 'github' ? 1 : 0);
      await expect(card.getByTestId('review-comment-edit-editor')).toHaveCount(0);
      const edits = await forgeRouteCalls(harness, routes.edit);
      expect(edits).toHaveLength(1);
      if (forge === 'gitlab') expect(edits[0].path).toMatch(new RegExp(`/notes/${REPLY_ID}$`));

      // Delete asks in place, then removes only that comment.
      await edited.hover();
      await edited.getByTestId('review-comment-delete').click();
      await expect(edited.getByTestId('review-comment-confirm')).toContainText('Delete?');
      await card.screenshot({ path: testInfo.outputPath(`${forge}-delete-confirm.png`) });
      await edited.getByTestId('review-comment-confirm-yes').click();
      await expect(card.getByTestId('review-thread-comment').filter({ hasText: 'Counting starts at one.' })).toHaveCount(0, { timeout: 30_000 });
      await expect(card).toContainText('Why start at one?');

      // Deleting a thread's only comment takes the thread with it.
      const only = thread(review, 'Only my note.');
      // The first comment's body reveals the actions in the card's header.
      await only.locator('[data-first-comment]').last().hover();
      await expect(only.locator('[data-first-comment] [data-testid="review-comment-actions"] > span')).toHaveCSS('opacity', '1');
      await only.getByTestId('review-comment-delete').first().click();
      await expect(only.getByTestId('review-comment-confirm')).toBeVisible();
      await only.screenshot({ path: testInfo.outputPath(`${forge}-first-comment-delete-confirm.png`) });
      await only.getByTestId('review-comment-confirm-yes').click();
      await expect(thread(review, 'Only my note.')).toHaveCount(0, { timeout: 30_000 });
      const deletes = await forgeRouteCalls(harness, routes.delete);
      expect(deletes).toHaveLength(2);
      if (forge === 'gitlab') {
        expect(deletes[0].path).toMatch(new RegExp(`/notes/${REPLY_ID}$`));
        expect(deletes[1].path).toMatch(new RegExp(`/notes/${ONLY_ID}$`));
      }
      await expectEveryForgeCallHandled(harness);
    } finally {
      await closeAll(page, harness);
    }
  });

  test(`on ${forge}, a range over unchanged and added lines posts with its span`, async ({ page }, testInfo) => {
    test.setTimeout(150_000);
    await page.emulateMedia({ colorScheme: 'dark' });
    const { harness, review } = await openPinned(page, forge, 'range');
    try {
      const editor = await openEditor(review, 'const two');
      const end = line(review, 'const three');
      await end.hover();
      await end.getByTestId('review-add-comment').click({ modifiers: ['Shift'] });
      await expect(editor.getByTestId('review-draft-editor-range')).toContainText('Lines 2–4');
      await expect(review.locator('[data-review-range]')).toHaveCount(3);
      // The editor sits after the range's last line.
      const editorTop = (await editor.boundingBox())!.y;
      expect(editorTop).toBeGreaterThan((await end.boundingBox())!.y);

      await editor.locator('textarea').fill('These three belong together.');
      await review.screenshot({ path: testInfo.outputPath(`${forge}-range-editor.png`) });
      await editor.getByTestId('review-draft-editor-now').click();
      const posted = thread(review, 'These three belong together.');
      // The span comes back from the forge as the thread's start line.
      await expect(posted.getByTestId('review-thread-range')).toHaveText('Lines 2–4', { timeout: 30_000 });
      expect(await forgeRouteCalls(harness, routes.lineComment)).toHaveLength(1);
      await expectEveryForgeCallHandled(harness);
    } finally {
      await closeAll(page, harness);
    }
  });

  test(`on ${forge}, a batch with a held reply sent to the agent quotes the thread`, async ({ page }) => {
    test.setTimeout(150_000);
    const harness = await launchPullsHarness();
    try {
      await seedPull(harness, forge, 'agent', 'Values thread');
      await harness.open(page);
      const review = await openPullRequestReview(page, 'Values thread');
      await expect(review.getByTestId('review-diff-stats')).toBeVisible({ timeout: 60_000 });

      const editor = await openEditor(review, 'const half');
      await editor.locator('textarea').fill('Drop the half.');
      await editor.getByTestId('review-draft-editor-add').click();
      const composer = await openReply(review, 'Why start at one?');
      await composer.locator('textarea').fill('Please answer this one.');
      await composer.getByTestId('review-thread-reply-draft').click();
      const strip = review.getByTestId('review-send-strip');
      await expect(strip).toContainText('2 drafts');
      await expect(review.getByTestId('review-submit-target')).toHaveValue('agent');

      await review.getByTestId('review-send-comments').click();
      await expect(strip).toHaveCount(0, { timeout: 30_000 });
      const delivered = page.getByTestId('user-message-bubble').filter({ hasText: 'Please answer this one.' });
      await expect(delivered).toBeVisible({ timeout: 30_000 });
      await expect(delivered).toContainText('src/values.ts:1: reply to this thread');
      await expect(delivered).toContainText('> octo: Why start at one?');
      await expect(delivered).toContainText('> ao-viewer: Counting starts there.');
      await expect(delivered).toContainText('Drop the half.');
      // Nothing went to the forge.
      expect(await forgeRouteCalls(harness, routes.reply)).toHaveLength(0);
      expect(await forgeRouteCalls(harness, routes.publish)).toHaveLength(0);
      await expectEveryForgeCallHandled(harness);
    } finally {
      await closeAll(page, harness);
    }
  });
}
