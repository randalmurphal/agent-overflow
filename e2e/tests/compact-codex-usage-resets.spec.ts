// Codex usage resets under the phone descriptor: the same flow as the
// desktop spec (codex-usage-resets-helpers.ts), with the dropdown anchored at
// its button inside the screen, on the Accounts page and in the switcher, and
// a finger-sized button and menu lines.
//
// Owns its backend: it signs a Codex account in, process-wide state no other
// spec should inherit.

import { expect, test } from './base-test.js';

import { launchHarness } from '../src/harness.js';
import { runCodexUsageResets } from './codex-usage-resets-helpers.js';

test('a Codex account spends and finishes its resets from the dropdown on a phone', async ({ page }) => {
  const harness = await launchHarness();
  try {
    await runCodexUsageResets(harness, page, {
      async checkDropdown(trigger, list) {
        const button = (await trigger.boundingBox())!;
        const box = (await list.boundingBox())!;
        const viewport = page.viewportSize()!;
        // Anchored at the button, above or below it, and on screen.
        const below = Math.abs(box.y - (button.y + button.height));
        const above = Math.abs(box.y + box.height - button.y);
        expect(Math.min(below, above), 'dropdown next to its button').toBeLessThanOrEqual(12);
        expect(box.x).toBeGreaterThanOrEqual(0);
        expect(box.x + box.width).toBeLessThanOrEqual(viewport.width);
        // The button and every line of its menu take a finger.
        expect(button.height, 'finger-sized button').toBeGreaterThanOrEqual(36);
        for (const line of await list.getByRole('menuitem').all()) {
          expect((await line.boundingBox())!.height, 'finger-sized line').toBeGreaterThanOrEqual(36);
        }
      },
    });
  } finally {
    await harness.close();
  }
});
