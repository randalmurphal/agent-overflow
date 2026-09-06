// Drop accepted-send replies at the real WebSocket boundary. Reconnect retries
// must preserve the send identity and create exactly one backend user message.
import { expect, test } from '@playwright/test';
import { launchHarness } from '../src/harness.js';
import { RESULT_LINE, advance, claudeScenario, emit, seedAgentThread, startMock, textLines, waitForGate } from './agent-visibility-helpers.js';
import { confirmOnHost, mintInvite, nonLoopbackIPv4, redeemOnScreen } from './offhost-helpers.js';
import { COMPACT_SURFACE, DESKTOP_SURFACE } from './preview-gateway-helpers.js';

const SEND_WITH_OPTIONS = 3632185196;

export function sendRecoveryFlow(): void {
  for (const lostReplies of [1, 2]) {
    test(`an accepted send survives ${lostReplies} lost replies without a duplicate turn`, async ({ page }, info) => {
      test.skip(nonLoopbackIPv4() === null, 'requires a LAN interface for a paired browser');
      test.setTimeout(60_000);
      const harness = await launchHarness();
      let remaining = lostReplies;
      const sendIds: string[] = [];
      let dropped = 0;
      await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
        const server = socket.connectToServer();
        let pending: string | undefined;
        socket.onMessage(message => {
          const frame = JSON.parse(String(message));
          if (frame.type === 'rpc' && frame.methodId === SEND_WITH_OPTIONS) {
            sendIds.push(frame.params[2].sendId);
            if (remaining > 0) pending = frame.id;
          }
          server.send(message);
        });
        server.onMessage(message => {
          const frame = JSON.parse(String(message));
          if (pending !== undefined) {
            if (frame.type === 'rpc' && frame.id === pending) {
              expect(frame.error, 'the dropped reply must acknowledge a successful send').toBeUndefined();
              remaining--;
              dropped++;
              void socket.close({ code: 1012, reason: 'lost accepted-send reply' });
              void server.close();
            }
            // Hide echoes as well, so recovery cannot rely on an earlier event.
            return;
          }
          socket.send(message);
        });
      });
      try {
        await harness.rpc('SetNetworkSettings', { bindAll: true });
        const thread = await seedAgentThread(harness, 'send-recovery', 'Send recovery');
        await harness.rpc('HarnessSetScenario', { scenario: claudeScenario('send-recovery', [
          emit(textLines('start', 'Working once.')),
          { waitSignal: { name: 'finish' } },
          emit([...textLines('finish', 'Completed once.'), RESULT_LINE]),
        ]) });
        await confirmOnHost(harness, await redeemOnScreen(page, await mintInvite(harness, 'full'), 'Send recovery phone'));
        await expect(page.getByTestId('thread-row')).toHaveCount(1);
        await (info.project.name === 'compact' ? COMPACT_SURFACE : DESKTOP_SURFACE).openThread(page, 'Send recovery');
        const mock = await startMock(harness, thread);
        const message = 'Run this exactly once despite a bad connection.';
        await page.getByLabel('Message Input').fill(message);
        await page.getByRole('button', { name: 'Send message', exact: true }).click();
        await waitForGate(harness, 'finish');
        await expect.poll(() => dropped).toBe(lostReplies);
        await expect.poll(() => sendIds.length).toBe(2);
        expect(sendIds[0]).toBeTruthy();
        expect(new Set(sendIds).size).toBe(1);
        if (lostReplies === 2) {
          await expect(page.getByRole('dialog')).toContainText('This message may have reached the agent.');
          await page.getByRole('button', { name: 'Leave it', exact: true }).click();
        }
        await expect(page.getByRole('dialog')).toHaveCount(0);
        await expect(page.getByText(message, { exact: true })).toHaveCount(1);
        await expect(page.getByLabel('Message Input')).toHaveValue('');
        const history = await harness.rpc<Array<{ summary: string }>>('GetThreadUserMessageHistory', thread, 20);
        expect(history.filter(row => row.summary === message)).toHaveLength(1);
        await advance(harness, mock, 'finish');
        await harness.waitForEvent('provider:turn_completed');
        await expect(page.getByText('Completed once.', { exact: true })).toBeVisible();
        await expect(page.getByRole('button', { name: 'Interrupt current turn', exact: true })).toHaveCount(0);
        expect(sendIds).toHaveLength(2);
      } finally {
        await page.goto('about:blank');
        await harness.close();
      }
    });
  }
}
