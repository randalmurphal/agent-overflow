// ao-tool-rows-more.spec.ts under the compact layout, on both mock
// providers: every other AO tool row renders its decoded header and body
// inside the phone-width thread screen. Screenshots of every row collapsed
// and expanded go to the test's output directory.

import { test, expect } from './fixtures.js';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { startTogether } from './launch-helpers.js';
import { driveMoreAoToolRows, readMoreRows } from './ao-tool-rows-more-helpers.js';

for (const provider of ['claude', 'codex'] as const) {
  test(`${provider}: every other AO tool row reads on the compact thread screen`, async ({ page }) => {
    test.setTimeout(300_000);
    let app: HarnessApp | undefined;
    let remote: HarnessApp | undefined;
    try {
      [app, remote] = await startTogether(launchHarness(), launchHarness());
      await driveMoreAoToolRows(app, remote, provider, `compact-${provider}`);
      const viewport = page.viewportSize()!;
      await readMoreRows(app, page, (name) => test.info().outputPath(`compact-${provider}-more-${name}.png`), async (row) => {
        const box = (await row.boundingBox())!;
        expect(box.x).toBeGreaterThanOrEqual(0);
        expect(box.x + box.width).toBeLessThanOrEqual(viewport.width);
      });
    } finally {
      await Promise.allSettled([app?.close(), remote?.close()]);
    }
  });
}
