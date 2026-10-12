// A Claude session that dies with a message queued hands the message back to
// the composer, then reconnects on its own.
//
// The death restore and the auto-reconnect run after the same death. The
// reconnect stops the thread first, and a stop drops its queue, so the
// restore must run before it (app_provider_events.go). The lock order between
// them cannot be forced from the browser;
// TestAutoReconnectAfterDeathRestoresTheQueueFirst covers both orders. This
// spec covers the whole path a person sees.
import type { Page } from '@playwright/test';
import { expect, test } from './fixtures.js';
import {
  advance,
  claudeTurnsScenario,
  emit,
  seedAgentThread,
  startMock,
  textLines,
  waitForGate,
} from './agent-visibility-helpers.js';

async function send(page: Page, text: string): Promise<void> {
  const input = page.getByLabel('Message Input');
  await input.fill(text);
  await page.getByRole('button', { name: 'Send message', exact: true }).click();
}

test('a session that dies with a queued message returns it to the composer and reconnects', async ({
  harness,
  page,
}, testInfo) => {
  test.setTimeout(90_000);
  const queued = 'Also update the changelog.';
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario(
      'death-queue',
      [[emit(textLines('msg-1', 'Working on the tree.')), { waitSignal: { name: 'hold' } }, { exit: { code: 1 } }]],
      { holdQueuedInput: true },
    ),
  });
  const threadId = await seedAgentThread(harness, 'death-queue', 'Death queue');
  await harness.open(page);
  await page.getByText('Death queue').click();
  const mockId = await startMock(harness, threadId);

  await send(page, 'Update the tree.');
  await waitForGate(harness, 'hold');
  const input = page.getByLabel('Message Input');
  const flushed = harness.waitForEvent('provider:queue_flushed', (ev: any) => ev.threadId === threadId);
  await input.fill(queued);
  await input.press('Enter');
  await flushed;
  await expect(page.getByTestId('send-queue-preview-row').filter({ hasText: queued })).toBeVisible();

  const restored = harness.waitForEvent('provider:queue_restored', (ev: any) => ev.threadId === threadId);
  const reconnected = harness.waitForEvent(
    'harness:mock',
    (ev: any) => ev.report.kind === 'registered' && ev.mockId !== mockId,
  );
  await advance(harness, mockId, 'hold');
  await restored;
  await reconnected;

  await expect(input).toHaveValue(queued);
  await expect(page.getByTestId('send-queue-preview-row')).toHaveCount(0);
  await expect(page.getByTestId('user-message-bubble').filter({ hasText: queued })).toHaveCount(0);
  await page.screenshot({ path: testInfo.outputPath('death-queue.png') });
});
