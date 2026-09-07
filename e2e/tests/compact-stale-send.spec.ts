// A phone can miss activity without losing its RPC connection. The real
// composer must reconcile its provisional send with the backend's active-turn
// placement, then keep that single message before the answer through reload.
import { expect, test, type WebSocketRoute } from '@playwright/test';
import { launchHarness } from '../src/harness.js';
import { advance, seedAgentThread, startMock, waitForGate } from './agent-visibility-helpers.js';
import { PAIRED_APP_MOUNT_MS, confirmOnHost, mintInvite, nonLoopbackIPv4, redeemOnScreen } from './offhost-helpers.js';

interface Item { id: string; kind: string; summary: string; turnIndex: number; meta: string }

test('a stale idle phone sends into the running Codex turn exactly once', async ({ page }) => {
  test.skip(nonLoopbackIPv4() === null, 'requires a LAN interface for a paired phone');
  const harness = await launchHarness();
  let suppressedStarts = 0;
  const sendIDs: string[] = [];
  const errors: string[] = [];
  let connection: { page: WebSocketRoute; server: WebSocketRoute } | undefined;
  page.on('pageerror', error => errors.push(String(error)));
  await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
    const server = socket.connectToServer();
    connection = { page: socket, server };
    socket.onMessage(message => {
      const frame = JSON.parse(String(message));
      if (frame.type === 'rpc' && frame.methodId === 3632185196) sendIDs.push(frame.params[2].sendId);
      server.send(message);
    });
    server.onMessage(message => {
      const frame = JSON.parse(String(message));
      const keep = (event: { channel: string }) => {
        if (event.channel !== 'provider:turn_started') return true;
        suppressedStarts++;
        return false;
      };
      if (frame.type === 'event' && !keep(frame)) return;
      if (frame.type === 'batch') {
        frame.events = frame.events.filter(keep);
        socket.send(JSON.stringify(frame));
      } else socket.send(message);
    });
  });
  try {
    await harness.rpc('SetNetworkSettings', { bindAll: true });
    await harness.rpc('HarnessSetScenario', { name: 'codex-steer-while-running' });
    const thread = await seedAgentThread(harness, 'stale-phone-send', 'Stale phone send', 'codex');
    await confirmOnHost(harness, await redeemOnScreen(page, await mintInvite(harness, 'full'), 'Stale phone'));
    await expect(page.getByTestId('thread-row')).toHaveCount(1, { timeout: PAIRED_APP_MOUNT_MS });
    await page.getByTestId('thread-row').click();
    await expect(page.getByLabel('Message Input')).toBeEnabled();
    const mock = await startMock(harness, thread);
    await harness.rpc('SendMessage', thread, 'Started from the laptop.', null);
    await waitForGate(harness, 'hold-first-turn');
    await expect.poll(() => suppressedStarts).toBeGreaterThan(0);
    await expect(page.getByTestId('user-message-bubble').filter({ hasText: 'Started from the laptop.' })).toHaveCount(1);
    await expect(page.getByRole('button', { name: 'Interrupt current turn', exact: true })).toHaveCount(0);

    const prompt = 'Sent from the phone while its activity state is stale.';
    await page.getByLabel('Message Input').fill(prompt);
    await page.getByRole('button', { name: 'Send message', exact: true }).click();
    const bubbles = page.getByTestId('user-message-bubble');
    await expect(bubbles.filter({ hasText: prompt })).toHaveCount(1);
    await expect.poll(async () => {
      const rows = await harness.rpc<Item[]>('ListItems', thread, true);
      return rows.filter(row => row.kind === 'user_text' && row.summary === prompt).length;
    }).toBe(1);
    expect(sendIDs).toHaveLength(1);
    const rows = await harness.rpc<Item[]>('ListItems', thread, true);
    const accepted = rows.find(row => row.summary === prompt)!;
    expect(JSON.parse(accepted.meta).sendId).toBe(sendIDs[0]);
    expect(accepted.turnIndex).toBe(rows.find(row => row.summary === 'Started from the laptop.')!.turnIndex);
    // The backend hello is unchanged and start events remain suppressed.
    // Reconnect must recover activity from the snapshot, without a reload.
    await connection!.page.close({ code: 1012, reason: 'activity recovery' });
    await connection!.server.close();
    await expect(page.getByRole('button', { name: 'Interrupt current turn', exact: true })).toBeVisible();
    await advance(harness, mock, 'hold-first-turn');
    await harness.waitForEvent('provider:turn_completed');
    await expect(page.getByText('Finished turn 1.', { exact: true })).toBeVisible();
    await expect(bubbles.filter({ hasText: prompt })).toHaveCount(1);
    await expect.poll(() => bubbles.filter({ hasText: prompt }).evaluate(element => {
      const answer = Array.from(document.querySelectorAll('p')).find(row => row.textContent === 'Finished turn 1.');
      return !!answer && !!(element.compareDocumentPosition(answer) & Node.DOCUMENT_POSITION_FOLLOWING);
    })).toBe(true);
    const ordered = await harness.rpc<Item[]>('ListItems', thread, true);
    expect(ordered.findIndex(row => row.id === accepted.id)).toBeLessThan(ordered.findIndex(row => row.summary === 'Finished turn 1.'));
    await page.reload();
    await page.getByTestId('thread-row').click();
    await expect(page.getByText('Finished turn 1.', { exact: true })).toBeVisible();
    await expect(bubbles.filter({ hasText: prompt })).toHaveCount(1);
    expect(errors).toEqual([]);
  } finally {
    await page.goto('about:blank');
    await harness.close();
  }
});
