// Recovery fails twice: the first replay disconnects, the second never ends
// despite live traffic. Switch threads and send during that second attempt.
// The real watchdog must restore the watched timeline and active-turn state
// without a reload, duplicate send, lost draft, or borrowing the other thread.
import { expect, test, type WebSocketRoute } from '@playwright/test';
import { launchHarness } from '../src/harness.js';
import {
  RESULT_LINE, advance, claudeScenario, emit, seedAgentThread, startMock, textLines, waitForGate,
} from './agent-visibility-helpers.js';
import { confirmOnHost, instrument, mintInvite, nonLoopbackIPv4, redeemOnScreen } from './offhost-helpers.js';
import { COMPACT_SURFACE } from './preview-gateway-helpers.js';

test('interrupted then stalled replay recovers a changed thread and an accepted send', async ({ page }) => {
  test.skip(nonLoopbackIPv4() === null, 'requires a LAN interface for a paired browser');
  // Uses the production 60s replay deadline, not a test-only timing override.
  test.setTimeout(120_000);
  const harness = await launchHarness();
  let fault: 'none' | 'interrupt' | 'stall' | 'recovered' = 'none';
  let connection: { page: WebSocketRoute; server: WebSocketRoute } | undefined;
  let stalled = false;
  let completedRecovery = false;
  let interrupted = 0;
  let trafficWhileStalled = 0;
  let recoveredWatch: string[] = [];
  const sendIds: string[] = [];
  const surfaced = await instrument(page);
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
    const mode = fault;
    const server = socket.connectToServer();
    connection = { page: socket, server };
    socket.onMessage(message => {
      const frame = JSON.parse(String(message));
      if (frame.type === 'rpc' && frame.methodId === 3632185196) sendIds.push(frame.params[2].sendId);
      if (mode === 'recovered' && frame.type === 'watch') recoveredWatch = frame.threads;
      server.send(message);
    });
    server.onMessage(message => {
      const frame = JSON.parse(String(message));
      if (frame.type === 'replay' && mode === 'interrupt') {
        interrupted++;
        fault = 'stall';
        void socket.close({ code: 1012, reason: 'disconnect before replay completion' });
        void server.close();
        return;
      }
      if (frame.type === 'replay' && mode === 'stall') {
        stalled = true;
        fault = 'recovered';
        return;
      }
      if (mode === 'stall' && stalled && ['event', 'batch', 'ping'].includes(frame.type)) trafficWhileStalled++;
      if (mode === 'recovered' && frame.type === 'replay') completedRecovery = true;
      socket.send(message);
    });
  });
  try {
    await harness.rpc('SetNetworkSettings', { bindAll: true });
    const first = await seedAgentThread(harness, 'interrupted-first', 'First recovery thread');
    const second = await seedAgentThread(harness, 'interrupted-second', 'Second recovery thread');
    await confirmOnHost(harness, await redeemOnScreen(page, await mintInvite(harness, 'full'), 'Interrupted recovery phone'));
    await expect(page.getByTestId('thread-row')).toHaveCount(2);
    await COMPACT_SURFACE.openThread(page, 'First recovery thread');
    await harness.rpc('HarnessSetScenario', { scenario: claudeScenario('first-recovery', [
      emit(textLines('first-start', 'First thread is working.')),
      { waitSignal: { name: 'first-finish' } },
      emit([...textLines('first-end', 'First thread finished during recovery.'), RESULT_LINE]),
    ]) });
    const firstMock = await startMock(harness, first);
    await harness.rpc('SendMessage', first, 'Start first thread.', null);
    await waitForGate(harness, 'first-finish');
    await expect(page.getByText('First thread is working.', { exact: true })).toBeVisible();
    const stop = page.getByRole('button', { name: 'Interrupt current turn', exact: true });
    await expect(stop).toBeVisible();

    fault = 'interrupt';
    await connection!.page.close({ code: 1012, reason: 'start interrupted recovery' });
    await connection!.server.close();
    await expect.poll(() => stalled).toBe(true);
    expect(interrupted).toBe(1);
    await advance(harness, firstMock, 'first-finish');
    await harness.waitForEvent('provider:turn_completed', (event: any) => event.threadId === first);

    await page.getByTestId('compact-back').click();
    await COMPACT_SURFACE.openThread(page, 'Second recovery thread');
    await harness.rpc('HarnessSetScenario', { scenario: claudeScenario('second-recovery', [
      emit(textLines('second-start', 'Second thread is working.')),
      { waitSignal: { name: 'second-finish' } },
      emit([...textLines('second-end', 'Second thread finished after recovery.'), RESULT_LINE]),
    ]) });
    const secondMock = await startMock(harness, second);
    const prompt = 'Send while recovery is stalled.';
    await page.getByLabel('Message Input').fill(prompt);
    await page.getByRole('button', { name: 'Send message', exact: true }).click();
    await waitForGate(harness, 'second-finish');
    await expect(page.getByLabel('Message Input')).toHaveValue('');
    await page.getByLabel('Message Input').fill('Keep the next message.');
    await expect.poll(async () => (await harness.rpc<{ content: string }>('GetDraft', second)).content)
      .toBe('Keep the next message.');
    expect(completedRecovery).toBe(false);

    await expect.poll(() => completedRecovery, { timeout: 80_000 }).toBe(true);
    expect(trafficWhileStalled).toBeGreaterThan(0);
    expect(recoveredWatch).toContain(second);
    expect(recoveredWatch).not.toContain(first);
    await expect(page.getByText('Second thread is working.', { exact: true })).toBeVisible();
    // A nonempty next draft makes the main button Send even during a turn.
    // The activity indicator is the running-state contract in that state.
    await expect(page.getByTestId('activity-rail-working')).toBeVisible();
    await expect(page.getByTestId('user-message-bubble').filter({ hasText: prompt })).toHaveCount(1);
    await expect(page.getByLabel('Message Input')).toHaveValue('Keep the next message.');
    expect(sendIds).toHaveLength(1);
    const history = await harness.rpc<Array<{ summary: string }>>('GetThreadUserMessageHistory', second, 20);
    expect(history.filter(row => row.summary === prompt)).toHaveLength(1);

    await advance(harness, secondMock, 'second-finish');
    await expect(page.getByText('Second thread finished after recovery.', { exact: true })).toBeVisible();
    await expect(page.getByTestId('activity-rail-working')).toHaveCount(0);
    await page.getByTestId('compact-back').click();
    await COMPACT_SURFACE.openThread(page, 'First recovery thread');
    await expect(page.getByText('First thread finished during recovery.', { exact: true })).toBeVisible();
    await expect(stop).toHaveCount(0);
    await expect(page.getByLabel('Message Input')).toHaveValue('');
    expect(errors).toEqual([]);
    expect(surfaced.errorToasts).toEqual([]);
  } finally {
    await page.goto('about:blank');
    await harness.close();
  }
});
