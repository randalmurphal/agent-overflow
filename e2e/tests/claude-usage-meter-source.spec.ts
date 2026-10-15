// The composer's Claude usage meters through the shipped UI and the real
// backend: a Claude turn whose stream carries rate_limit_event lines (a
// warning-band weekly reading and a refusal) leaves both meters and the
// backend's quota cache untouched. The event cannot say which account it
// reports, so the meters read only the OAuth usage endpoint, which an
// isolated boot refuses; "awaiting first update" is therefore the only
// correct state here.
import { test, expect } from './fixtures.js';
import {
  claudeScenario, claudeUsageResult, emit, seedAgentThread, startMock, textLines,
} from './agent-visibility-helpers.js';

test('Claude rate_limit_event lines do not move the usage meters', async ({ harness, page }) => {
  const resetsAt = Math.floor(Date.now() / 1000) + 6 * 24 * 3600;
  const threadId = await seedAgentThread(harness, 'usage-meter-source', 'Usage meter source');
  await harness.rpc('HarnessSetScenario', { scenario: claudeScenario('usage-meter-source', [emit([
    ...textLines('msg-usage', 'Working on it'),
    JSON.stringify({ type: 'rate_limit_event', rate_limit_info: {
      status: 'allowed_warning', resetsAt, rateLimitType: 'seven_day', utilization: 0.28, isUsingOverage: false,
    } }),
    JSON.stringify({ type: 'rate_limit_event', rate_limit_info: {
      status: 'rejected', resetsAt: resetsAt - 6 * 24 * 3600 + 3600, rateLimitType: 'five_hour',
    } }),
    claudeUsageResult(1200, 340),
  ])]) });
  await harness.open(page);
  await page.getByTestId('thread-row').click();
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'go', null);
  await harness.waitForEvent('provider:turn_completed');

  // The rate_limit_event lines precede the result on the same ordered
  // thread worker, so by turn completion they have been fully handled.
  expect(await harness.rpc('GetRateLimitsSnapshots')).toEqual([]);
  for (const window of ['5h', '7d']) {
    await expect(page.getByTestId(`composer-rate-limit-${window}`).getByRole('button'))
      .toHaveAttribute('aria-label', /awaiting first update/);
  }
});
