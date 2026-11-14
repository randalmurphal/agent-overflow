// A comment posted now whose pane closes mid-send on desktop: the
// companion review closes with its close button while the forge holds the
// post's answer. Refused, one held toast says it was not sent; lost, that
// it may have been posted. Either copies the text, and the review opened
// again has a fresh editor. Flow and compact twin:
// review-comment-pane-close-flow.ts, compact-review-comment-pane-close.spec.ts.
import { test } from './fixtures.js';
import { failAfterPaneCloses } from './review-comment-pane-close-flow.js';

for (const outcome of ['refused', 'lost'] as const) {
  test(`a comment ${outcome} after its review closed is one held toast with its text to copy`, async ({ harness, page }) => {
    test.setTimeout(120_000);
    await page.emulateMedia({ colorScheme: 'dark' });
    await failAfterPaneCloses(harness, page, false, outcome);
  });
}
