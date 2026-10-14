// A usage window whose reset time passes, through the shipped UI and the real
// backend. The next window starts at the account's next request, so its reset
// time is unknown until a reading reports it: the meter shows 0% with no
// countdown and no "Resetting now", while a window still running keeps its
// countdown. Driven by a mock Codex account/rateLimits/updated notification,
// the partial reading the backend still accepts from a live session.
import { test, expect } from './fixtures.js';
import { seedAgentThread, startMock } from './agent-visibility-helpers.js';

function line(method: string, params: Record<string, unknown>): string {
  return JSON.stringify({ jsonrpc: '2.0', method, params });
}

test('an expired usage window shows 0% with no reset countdown', async ({ harness, page }) => {
  const nowSec = Math.floor(Date.now() / 1000);
  const sessionReset = nowSec + 8;
  const weeklyReset = nowSec + 3 * 24 * 3600;
  const ids = { threadId: '${THREAD_ID}', turnId: '${TURN_ID}' };
  await harness.rpc('HarnessSetScenario', { scenario: {
    version: 1,
    name: 'usage-meter-expired-window',
    provider: 'codex',
    turns: [{ label: 'usage-reading', steps: [{ emit: { delayBetweenMs: 5, lines: [
      line('turn/started', { threadId: ids.threadId, turn: { id: ids.turnId } }),
      line('account/rateLimits/updated', { rateLimits: {
        limitId: 'codex',
        limitName: 'Codex',
        primary: { usedPercent: 64, windowDurationMins: 300, resetsAt: sessionReset },
        secondary: { usedPercent: 30, windowDurationMins: 10080, resetsAt: weeklyReset },
      } }),
      line('item/completed', { ...ids, item: { type: 'agentMessage', id: 'msg-${TURN}', status: 'completed', text: 'Done.' } }),
      line('turn/completed', { threadId: ids.threadId, turn: { id: ids.turnId, status: 'completed' } }),
    ] } }] }],
    afterTurns: 'repeatLast',
  } });
  const threadId = await seedAgentThread(harness, 'usage-expired-window', 'Expired window', 'codex');
  await harness.open(page);
  await page.getByTestId('thread-row').click();
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'go', null);
  await harness.waitForEvent('provider:turn_completed');

  const tooltipFor = async (window: '5h' | '7d') => {
    await page.getByTestId('chat-header-title').hover();
    await expect(page.getByRole('tooltip')).toHaveCount(0);
    await page.getByTestId(`composer-rate-limit-${window}`).getByRole('button').hover();
    const tooltip = page.getByRole('tooltip');
    await expect(tooltip).toBeVisible();
    return tooltip;
  };

  // Before the reset: the reading and its countdown.
  const before = await tooltipFor('5h');
  await expect(before).toContainText('64% used');
  await expect(before).toContainText('Resets in');

  // After the reset: 0%, and nothing claims to know when the next window ends.
  // The store's expiry timer fires at the boundary; reopen until it has.
  await expect(async () => {
    await expect(await tooltipFor('5h')).toContainText('0% used', { timeout: 1000 });
  }).toPass({ timeout: 30_000 });
  await expect(page.getByRole('tooltip')).not.toContainText('Reset');

  // A window still running keeps its countdown.
  const weekly = await tooltipFor('7d');
  await expect(weekly).toContainText('30% used');
  await expect(weekly).toContainText('Resets in 2d 23h');
});
