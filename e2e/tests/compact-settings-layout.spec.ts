// Every Settings page lays out within the phone screen: no horizontal
// overflow, no control past its edge, no word broken mid-word, no button
// label wrapped, no text field squeezed unreadable. Pages are measured
// populated (a project, saved provider variables, a Codex account that needs
// signing in again, a rebound chord). That Codex account's card also fits a
// 320px phone at the largest interface text size in Hack Nerd Font.
// settings-layout.spec.ts holds the same contract at desktop widths.
//
// Owns its backends: they adopt provider accounts, which is process-wide
// state no other spec should inherit.
import { test, expect } from './fixtures.js';
import { launchHarness } from '../src/harness.js';
import { codexAccountCardFaults, settingsLayoutFaults } from './settings-layout-helpers.js';

test('settings pages fit a phone screen', async ({ page }) => {
  test.setTimeout(180_000);
  const harness = await launchHarness();
  try {
    expect(await settingsLayoutFaults(harness, page, true)).toEqual([]);
  } finally {
    await harness.close();
  }
});

test('an account card fits a 320px phone at the largest text size', async ({ page }) => {
  test.setTimeout(180_000);
  await page.setViewportSize({ width: 320, height: 720 });
  // This screen's own preferences: text size 20 (the maximum) in the widest
  // interface font.
  await page.addInitScript(() => {
    localStorage.setItem('agent-overflow:frontend:preferences', JSON.stringify({ fontSize: 20, sansFont: 'hack-nerd' }));
  });
  const harness = await launchHarness();
  try {
    const faults = await codexAccountCardFaults(harness, page);
    const root = await page.evaluate(() => ({
      fontSize: getComputedStyle(document.documentElement).fontSize,
      font: getComputedStyle(document.documentElement).getPropertyValue('--font-sans'),
    }));
    expect(parseFloat(root.fontSize)).toBeCloseTo((20 * 16) / 13, 1);
    expect(root.font).toContain('Hack Nerd Font');
    expect(faults).toEqual([]);
  } finally {
    await harness.close();
  }
});
