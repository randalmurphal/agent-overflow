// A comment posted now outlives its pane on compact: Back from the focused
// editor closes the companion review while the forge holds the post's
// answer; the refusal that follows is a toast, and the review opened again
// shows the editor with its text and sentence. Flow and desktop twin:
// review-comment-pane-close-flow.ts, review-comment-pane-close.spec.ts.
import { test } from './fixtures.js';
import { failAfterPaneCloses } from './review-comment-pane-close-flow.js';

test('on compact, a comment refused after Back closed its review keeps its text for the next open', async ({ harness, page }) => {
  test.setTimeout(120_000);
  await page.emulateMedia({ colorScheme: 'dark' });
  await failAfterPaneCloses(harness, page, true);
});
