// A file picked on a paired device while its connection is down.
//
// The picker opens while the composer is usable. The connection can drop
// before the selection comes back (a phone's camera covers the app for
// seconds), and a composer with no connection is disabled. The selection
// is kept, shown as waiting, and attached once the connection returns.
//
// The page is a paired off-host browser: a loopback page holds host
// presence and its composer is never disabled by a dropped socket.

import { expect, test, type BrowserContext, type Page, type WebSocketRoute } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import {
  PAIRED_APP_MOUNT_MS,
  confirmOnHost,
  mintInvite,
  nonLoopbackIPv4,
  redeemOnScreen,
} from './offhost-helpers.js';

const lanIP = nonLoopbackIPv4();

test.describe('attaching while the connection is down', () => {
  test.skip(
    lanIP === null,
    'outside the test network namespace, so no off-host peer can be produced without LAN traffic',
  );

  let harness: HarnessApp;
  let context: BrowserContext;
  let phone: Page;
  // The page's sockets run through this route, so the test can cut them
  // and refuse new ones without taking HTTP away.
  const sockets = new Set<WebSocketRoute>();
  let offline = false;

  test.beforeAll(async ({ browser }) => {
    harness = await launchHarness();
    const network = await harness.rpc<{ bindAll: boolean }>('SetNetworkSettings', { bindAll: true });
    expect(network.bindAll).toBe(true);
    await harness.rpc('HarnessSeed', {
      projects: [{
        name: 'offline-attach',
        repo: {},
        threads: [{ title: 'Offline attach', turns: [{ userText: 'first', items: [{ kind: 'assistant_text', summary: 'one' }] }] }],
      }],
    });
    context = await browser.newContext();
    phone = await context.newPage();
    await phone.routeWebSocket(/.*/, (socket) => {
      if (offline) {
        void socket.close({ code: 1006 });
        return;
      }
      socket.connectToServer();
      sockets.add(socket);
      socket.onClose(() => sockets.delete(socket));
    });
    const shown = await redeemOnScreen(phone, await mintInvite(harness, 'full'), 'Offline attach browser');
    await confirmOnHost(harness, shown);
    await expect(phone.getByTestId('thread-row')).toHaveCount(1, { timeout: PAIRED_APP_MOUNT_MS });
    await phone.getByTestId('thread-row').first().click();
    await expect(phone.getByLabel('Message Input')).toBeEnabled();
  });

  test.afterAll(async () => {
    await context?.close();
    await harness?.rpc('SetNetworkSettings', { bindAll: false }).catch(() => undefined);
    await harness?.close();
  });

  test('a selection returned while disconnected waits and attaches on reconnect', async () => {
    const input = phone.getByLabel('Message Input');
    const chooser = phone.waitForEvent('filechooser');
    await phone.getByTestId('composer-attach').click();
    const picker = await chooser;

    offline = true;
    for (const socket of [...sockets]) await socket.close({ code: 1006 });
    await expect(input).toBeDisabled();
    await expect(input).toHaveAttribute('placeholder', 'Disconnected from the agent backend');

    await picker.setFiles({ name: 'taken-offline.txt', mimeType: 'text/plain', buffer: Buffer.from('picked while offline') });
    await expect(phone.getByTestId('composer-attachment-held')).toHaveText('Waiting to attach…');
    await expect(phone.getByLabel('Remove taken-offline.txt')).toHaveCount(0);

    offline = false;
    await expect(input).toBeEnabled({ timeout: PAIRED_APP_MOUNT_MS });
    await expect(phone.getByLabel('Remove taken-offline.txt')).toBeVisible();
    await expect(phone.getByTestId('composer-attachment-held')).toHaveCount(0);
  });
});
