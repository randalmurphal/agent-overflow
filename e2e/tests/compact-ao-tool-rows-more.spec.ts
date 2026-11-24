// ao-tool-rows-more.spec.ts under the compact layout, on the Claude mock
// (the provider changes no layout): every other AO tool row renders its
// decoded header and body inside the phone-width thread screen. With
// AO_E2E_SCREENSHOTS=1, screenshots of every row collapsed and expanded go
// to the test's output directory.

import { test, expect } from './fixtures.js';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { startTogether } from './launch-helpers.js';
import { driveMoreAoToolRows, readMoreRows } from './ao-tool-rows-more-helpers.js';

test('claude: every other AO tool row reads on the compact thread screen', async ({ page }) => {
  test.setTimeout(300_000);
  let app: HarnessApp | undefined;
  let remote: HarnessApp | undefined;
  try {
    [app, remote] = await startTogether(launchHarness(), launchHarness());
    await driveMoreAoToolRows(app, remote, 'claude', 'compact-claude');
    const viewport = page.viewportSize()!;
    await readMoreRows(app, page, (name) => test.info().outputPath(`compact-claude-more-${name}.png`), async (row) => {
      const box = (await row.boundingBox())!;
      expect(box.x).toBeGreaterThanOrEqual(0);
      expect(box.x + box.width).toBeLessThanOrEqual(viewport.width);
    });
  } finally {
    await Promise.allSettled([app?.close(), remote?.close()]);
  }
});
