// Codex usage resets end to end on the desktop layout, through the card's
// "Usage resets (N)" button and its dropdown (codex-usage-resets-helpers.ts):
// Use behind a confirm spends the earliest-expiring reset, and a spend whose
// answer is lost is finished with Finish under the same key, so it spends
// nothing more. The account switcher's row offers the same button, and its
// menu works from the keyboard.
//
// Owns its backend: it signs a Codex account in, process-wide state no other
// spec should inherit.

import { test } from './base-test.js';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import { runCodexUsageResets } from './codex-usage-resets-helpers.js';

let harness: HarnessApp;

test.beforeAll(async () => {
  harness = await launchHarness();
});

test.afterAll(async () => {
  await harness?.close();
});

test('a Codex account offers its earned resets, spends one per confirm, and finishes a lost one', async ({ page }) => {
  await runCodexUsageResets(harness, page);
});
