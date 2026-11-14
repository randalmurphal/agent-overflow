// A Claude usage reading end to end through the isolated usage endpoint: the
// harness sets an answer for the signed-in account (HarnessSetClaudeUsage),
// the backend reads it through the real probe, and the composer's 7d meter
// shows its percentage without waiting for the usage poll.
//
// Owns its backend: it seeds a native Claude login, which a probe can adopt
// as a saved account, process-wide state no other spec should inherit.

import { expect, test } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import { seedAgentThread } from './agent-visibility-helpers.js';
import { seedNativeClaudeLogin } from './provider-account-helpers.js';

const EMAIL = 'usage-reading@agent-overflow.test';

let harness: HarnessApp;

test.beforeAll(async () => {
  harness = await launchHarness();
  await seedNativeClaudeLogin(harness, EMAIL, Date.now() + 5 * 24 * 3600_000);
});

test.afterAll(async () => {
  await harness?.close();
});

test('a usage reading set for the signed-in Claude account drives the composer meter', async ({ page }) => {
  const resetsAt = new Date(Date.now() + 3 * 24 * 3600_000).toISOString();
  await seedAgentThread(harness, 'claude-usage-reading', 'Usage reading');
  await harness.open(page);
  await page.getByTestId('thread-row').click();
  const weekly = page.getByTestId('composer-rate-limit-7d').getByRole('button');
  await expect(weekly).toHaveAttribute('aria-label', /awaiting first update/);

  const usage = harness.waitForEvent('provider:usage', (event: any) =>
    event.action === 'rate_limits' && event.rateLimits?.provider === 'claude');
  await harness.rpc('HarnessSetClaudeUsage', { email: EMAIL, limits: [
    { kind: 'weekly_all', group: 'weekly', percent: 37, resets_at: resetsAt },
  ] });
  await usage;

  await expect(weekly).toHaveAttribute('aria-label', '7-day limit: 37% used');
  // The answer names no session limit, so no 5h meter is mounted.
  await expect(page.getByTestId('composer-rate-limit-5h')).toHaveCount(0);
});
