// Local/paired session recovery and paired renewal through the production SPA.
// Injected refusal frames exercise a responsive socket; all subsequent
// bootstrap, renewal, ticket, replay, and thread reads use the real backend.
import { expect, test, type WebSocketRoute } from '@playwright/test';
import { launchHarness } from '../src/harness.js';
import { confirmOnHost, mintInvite, nonLoopbackIPv4, redeemOnScreen } from './offhost-helpers.js';

for (const paired of [false, true]) {
  test(`${paired ? 'paired remote' : 'local'} session recovery preserves the open thread and draft`, async ({ page }) => {
    test.skip(paired && nonLoopbackIPv4() === null, 'requires a non-loopback interface');
    const harness = await launchHarness();
    const sockets: WebSocketRoute[] = [];
    await page.routeWebSocket('**/ws*', (socket) => {
      socket.connectToServer();
      sockets.push(socket);
    });
    try {
      await harness.rpc('HarnessSeed', {
        projects: [{ name: 'session-lifecycle', repo: {}, threads: [{
          title: 'Keep this thread', turns: [{ userText: 'hello', items: [{ kind: 'assistant_text', summary: 'Retained history' }] }],
        }] }],
      });
      if (paired) {
        await harness.rpc('SetNetworkSettings', { bindAll: true });
        const shown = await redeemOnScreen(page, await mintInvite(harness, 'full'), 'Session lifecycle browser');
        await confirmOnHost(harness, shown);
      } else {
        await page.goto(harness.bootstrap.url);
      }
      await expect(page.getByTestId('thread-row')).toHaveCount(1);
      await page.getByTestId('thread-row').first().click();
      const composer = page.getByLabel('Message Input');
      await expect(composer).toBeEnabled();
      await composer.fill('Keep my unsent draft');
      await expect(page.getByText('Retained history', { exact: true })).toBeVisible();

      if (paired) {
        // A due renewal on resume must rotate the stored credential while
        // leaving the existing socket, pane, and draft in place.
        const before = sockets.length;
        const oldCredential = await page.evaluate(() => {
          const key = 'agent-overflow:deviceSession';
          const held = JSON.parse(localStorage.getItem(key)!);
          const credential = held.credential;
          held.expiresAtMs = Date.now() + 1000;
          localStorage.setItem(key, JSON.stringify(held));
          window.dispatchEvent(new Event('online'));
          return credential;
        });
        await expect.poll(() => page.evaluate(() => JSON.parse(localStorage.getItem('agent-overflow:deviceSession')!).credential)).not.toBe(oldCredential);
        expect(sockets).toHaveLength(before);
        await expect(composer).toHaveValue('Keep my unsent draft');
      }

      const before = sockets.length;
      sockets.at(-1)!.send(JSON.stringify({ type: 'session-ended', error: { code: 'auth_failed', reason: 'expired_session', message: 'not authorized' } }));
      await expect.poll(() => sockets.length).toBe(before + 1);
      await expect(composer).toBeEnabled();
      await expect(composer).toHaveValue('Keep my unsent draft');
      await expect(page.getByText('Retained history', { exact: true })).toBeVisible();
      await expect(page.getByTestId('transport-status-banner')).toHaveCount(0);
      if (paired) {
        expect(await page.evaluate(() => localStorage.getItem('agent-overflow:deviceSession'))).not.toBeNull();
      }
    } finally {
      await page.close();
      await harness.close();
    }
  });
}
