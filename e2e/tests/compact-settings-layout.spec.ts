// Every Settings page lays out within the phone screen: no horizontal
// overflow, no word broken mid-word, no button label wrapped, no text field
// squeezed unreadable. Pages are measured populated (a project, saved
// provider variables, a Codex account that needs signing in again, a rebound
// chord). settings-layout.spec.ts holds the same contract at desktop widths.
//
// Owns its backend: it adopts a provider account, which is process-wide state
// no other spec should inherit.
import { test, expect } from './fixtures.js';
import { launchHarness } from '../src/harness.js';
import { settingsLayoutFaults } from './settings-layout-helpers.js';

test('settings pages fit a phone screen', async ({ page }) => {
  test.setTimeout(180_000);
  const harness = await launchHarness();
  try {
    expect(await settingsLayoutFaults(harness, page, true)).toEqual([]);
  } finally {
    await harness.close();
  }
});
