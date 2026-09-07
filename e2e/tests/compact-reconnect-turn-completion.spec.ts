// A paired phone disconnects during its first live turn. The provider finishes
// offline; replay must restore both the answer and the composer's idle state,
// even though this client has never received a turn_completed event before.
// The overlap variant delivers a newer live copy ahead of replay, proving the
// connected phone cannot discard earlier answer rows as already received.
import { expect, test, type WebSocketRoute } from '@playwright/test';
import { launchHarness } from '../src/harness.js';
import {
  RESULT_LINE, advance, claudeScenario, emit, seedAgentThread, startMock, textLines, waitForGate,
} from './agent-visibility-helpers.js';
import {
  PAIRED_APP_MOUNT_MS, confirmOnHost, mintInvite, nonLoopbackIPv4, redeemOnScreen,
} from './offhost-helpers.js';

for (const overlap of [false, true]) {
test(`completion during an outage clears Stop on the paired phone without reloading${overlap ? ' with live events overtaking replay' : ''}`, async ({ page }) => {
  test.skip(nonLoopbackIPv4() === null, 'requires a non-loopback interface for the paired peer');
  const harness = await launchHarness();
  let online = true;
  let connection: { page: WebSocketRoute; server: WebSocketRoute } | undefined;
  const completions: unknown[] = [];
  const replays: Array<Record<string, number>> = [];
  let recovering = false;
  let injectedOverlap = false;
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    if (!online) {
      void socket.close({ code: 1012, reason: 'test network outage' });
      return;
    }
    const server = socket.connectToServer();
    const held: string[] = [];
    connection = { page: socket, server };
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message));
      if (frame.type === 'replay') replays.push(frame.lastSeqByChannel);
      server.send(message);
    });
    server.onMessage((message) => {
      const frame = JSON.parse(String(message));
      const events = frame.type === 'batch' ? frame.events : frame.type === 'event' ? [frame] : [];
      for (const event of events) {
        if (event.channel === 'provider:turn_completed') completions.push(event);
      }
      if (overlap && recovering && (frame.type === 'event' || frame.type === 'batch')) {
        held.push(String(message));
        return;
      }
      if (overlap && recovering && frame.type === 'replay') {
        // Replay and live fanout overlap. Deliver a newer live copy before
        // the older replay entries: advancing the cursor immediately would
        // discard the answer's earlier upserts as duplicates.
        const items = held.flatMap((raw) => {
          const buffered = JSON.parse(raw);
          return buffered.type === 'batch' ? buffered.events : [buffered];
        }).filter((event) => event.channel === 'provider:item_event');
        items.sort((a, b) => a.seq - b.seq);
        if (items.length > 1) {
          socket.send(JSON.stringify({ ...items.at(-1), type: 'event' }));
          injectedOverlap = true;
        }
        for (const raw of held) socket.send(raw);
        held.length = 0;
        recovering = false;
      }
      socket.send(message);
    });
  });
  try {
    await harness.rpc('SetNetworkSettings', { bindAll: true });
    const threadId = await seedAgentThread(harness, 'reconnect-completion', 'Finish while offline');
    await harness.rpc('HarnessSetScenario', {
      scenario: claudeScenario('finish-offline', [
        emit(textLines('msg-start', 'Working before disconnect.')),
        { waitSignal: { name: 'finish' } },
        emit([
          ...textLines('msg-middle', 'An earlier answer received during the outage.'),
          ...textLines('msg-end', 'Finished while the phone was offline.'), RESULT_LINE,
        ]),
      ]),
    });
    const invite = await mintInvite(harness, 'full');
    await confirmOnHost(harness, await redeemOnScreen(page, invite, 'Reconnect phone'));
    await expect(page.getByTestId('thread-row')).toHaveCount(1, { timeout: PAIRED_APP_MOUNT_MS });
    await page.getByTestId('thread-row').click();
    await expect(page.getByLabel('Message Input')).toBeEnabled();
    const mockId = await startMock(harness, threadId);
    await harness.rpc('SendMessage', threadId, 'finish while disconnected', null);
    await waitForGate(harness, 'finish');
    await expect(page.getByText('Working before disconnect.', { exact: true })).toBeVisible();
    const stop = page.getByRole('button', { name: 'Interrupt current turn', exact: true });
    await expect(stop).toBeVisible();
    expect(completions).toHaveLength(0);
    expect(connection).toBeDefined();

    online = false;
    await connection!.page.close({ code: 1012, reason: 'test network outage' });
    await connection!.server.close();
    await advance(harness, mockId, 'finish');
    await harness.waitForEvent('provider:turn_completed');
    await expect.poll(async () => (await harness.rpc<{ activeTurn?: unknown }>(
      'GetThreadLiveState', threadId,
    )).activeTurn).toBeFalsy();
    expect(completions).toHaveLength(0);

    recovering = overlap;
    online = true;
    await expect.poll(() => completions.length).toBe(1);
    expect(replays.some((cursors) => cursors['provider:turn_completed'] === 0)).toBe(true);
    await expect(page.getByText('Finished while the phone was offline.', { exact: true })).toBeVisible();
    await expect(page.getByText('An earlier answer received during the outage.', { exact: true })).toBeVisible();
    if (overlap) expect(injectedOverlap).toBe(true);
    await expect(stop).toHaveCount(0);
    await expect(page.getByLabel('Message Input')).toBeEnabled();
  } finally {
    await page.close();
    await harness.close();
  }
});
}
