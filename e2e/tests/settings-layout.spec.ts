// Every Settings page lays out within its panel at desktop widths, from a
// wide window down to one where the rail leaves the page under 30rem: no
// horizontal overflow, no word broken mid-word, no button label wrapped, no
// text field squeezed unreadable. Pages are measured populated (a project,
// saved provider variables, a Codex account that needs signing in again, a
// rebound chord). compact-settings-layout.spec.ts holds the same contract on
// a phone.
//
// Owns its backends: each width adopts a provider account, which is
// process-wide state no other spec should inherit.
import { test, expect } from './fixtures.js';
import { launchHarness } from '../src/harness.js';
import { settingsLayoutFaults } from './settings-layout-helpers.js';

for (const width of [1280, 1024, 800]) {
  test(`settings pages fit a ${width}px window`, async ({ page }) => {
    test.setTimeout(180_000);
    await page.setViewportSize({ width, height: 760 });
    const harness = await launchHarness();
    try {
      expect(await settingsLayoutFaults(harness, page, false)).toEqual([]);
    } finally {
      await harness.close();
    }
  });
}
