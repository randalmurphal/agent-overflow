// A watch whose script keeps failing under the compact layout
// (docs/specs/standing-agents.md, "Wake supervisor"): one notification
// naming the assistant and the watch, the failing outcome on the watch's
// row in the Background tray, and the fixed script's run withdrawing the
// notice. Desktop: standing-agent-wake-start-failures.spec.ts.
import { test } from './fixtures.js';
import { brokenWatchNotifiesUntilFixed } from './standing-agent-wake-helpers.js';

test('compact: a watch whose script keeps failing notifies once until it is fixed', async ({ page }) => {
  test.setTimeout(150_000);
  await brokenWatchNotifiesUntilFixed(page, 'compact');
});
