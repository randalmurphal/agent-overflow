// A comment posted now outlives its pane on desktop: the companion review
// closes with its close button while the forge holds the post's answer;
// the refusal that follows is a toast, and the review opened again shows
// the editor with its text and sentence. Flow and compact twin:
// review-comment-pane-close-flow.ts, compact-review-comment-pane-close.spec.ts.
import { test } from './fixtures.js';
import { failAfterPaneCloses } from './review-comment-pane-close-flow.js';

test('a comment refused after its review closed keeps its text for the next open', async ({ harness, page }) => {
  test.setTimeout(120_000);
  await page.emulateMedia({ colorScheme: 'dark' });
  await failAfterPaneCloses(harness, page, false);
});
