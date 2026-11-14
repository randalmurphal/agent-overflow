// A comment posted now whose pane closes mid-send on compact: Back from
// the focused editor closes the companion review while the forge holds the
// post's answer, then refuses it. One held toast says it was not sent and
// copies the text, and the review opened again has a fresh editor. Flow
// and desktop twin: review-comment-pane-close-flow.ts,
// review-comment-pane-close.spec.ts.
import { test } from './fixtures.js';
import { failAfterPaneCloses } from './review-comment-pane-close-flow.js';

test('on compact, a comment refused after Back closed its review is one held toast with its text to copy', async ({ harness, page }) => {
  test.setTimeout(120_000);
  await page.emulateMedia({ colorScheme: 'dark' });
  await failAfterPaneCloses(harness, page, true, 'refused');
});
