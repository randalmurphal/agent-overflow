// Every Settings page lays out within the phone screen: no horizontal
// overflow, no word broken mid-word, no button label wrapped, no text field
// squeezed unreadable. Pages are measured populated (a project, saved
// provider variables, a rebound chord). settings-layout.spec.ts holds the
// same contract at desktop widths.
import { test, expect } from './fixtures.js';
import { settingsLayoutFaults } from './settings-layout-helpers.js';

test('settings pages fit a phone screen', async ({ harness, page }) => {
  test.setTimeout(180_000);
  expect(await settingsLayoutFaults(harness, page, true)).toEqual([]);
});
