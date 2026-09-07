// Hold an outbound send while its cleared composer saves the next draft.
// The real wire/host must consume the captured draft, never this newer one.
import { expect, test } from '@playwright/test';
import { launchHarness } from '../src/harness.js';
import { RESULT_LINE, advance, claudeScenario, emit, seedAgentThread, startMock, textLines, waitForGate } from './agent-visibility-helpers.js';
import { confirmOnHost, mintInvite, nonLoopbackIPv4, redeemOnScreen } from './offhost-helpers.js';
import { COMPACT_SURFACE } from './preview-gateway-helpers.js';

test('the next draft survives earlier send acceptance and reopening the paired phone', async ({ page }) => {
  test.skip(nonLoopbackIPv4() === null, 'requires a LAN interface for a paired browser');
  const harness = await launchHarness();
  let releaseSend: (() => void) | undefined;
  await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
    const server = socket.connectToServer();
    socket.onMessage(message => {
      const frame = JSON.parse(String(message));
      if (frame.type === 'rpc' && frame.methodId === 3632185196) {
        releaseSend = () => server.send(message);
      } else server.send(message);
    });
    server.onMessage(message => socket.send(message));
  });
  try {
    await harness.rpc('SetNetworkSettings', { bindAll: true });
    const thread = await seedAgentThread(harness, 'next-draft', 'Next draft');
    await harness.rpc('SaveDraft', thread, 'Old saved draft.', [], [], null);
    await harness.rpc('HarnessSetScenario', { scenario: claudeScenario('next-draft', [
      emit(textLines('start', 'Working.')),
      { waitSignal: { name: 'finish' } },
      emit([...textLines('finish', 'Completed.'), RESULT_LINE]),
    ]) });
    await confirmOnHost(harness, await redeemOnScreen(page, await mintInvite(harness, 'full'), 'Next draft phone'));
    await expect(page.getByTestId('thread-row')).toHaveCount(1);
    await COMPACT_SURFACE.openThread(page, 'Next draft');
    const mock = await startMock(harness, thread);
    const input = page.getByLabel('Message Input');
    await input.fill('First message.');
    await page.getByRole('button', { name: 'Send message', exact: true }).click();
    await expect.poll(() => releaseSend !== undefined).toBe(true);
    const saved = () => harness.rpc<{ content: string }>('GetDraft', thread);
    expect((await saved()).content).toBe('First message.');
    await input.fill('Keep this next draft.');
    await expect.poll(async () => (await saved()).content).toBe('Keep this next draft.');
    releaseSend!();
    await waitForGate(harness, 'finish');
    expect((await saved()).content).toBe('Keep this next draft.');
    await advance(harness, mock, 'finish');
    await harness.waitForEvent('provider:turn_completed');
    await page.reload();
    await COMPACT_SURFACE.openThread(page, 'Next draft');
    await expect(page.getByLabel('Message Input')).toHaveValue('Keep this next draft.');
  } finally {
    await page.goto('about:blank');
    await harness.close();
  }
});
