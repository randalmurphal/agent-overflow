// Real desktop pairing across isolated host/frontend processes, using only an
// address typed into the shipped Connect to a computer UI. Both screens independently
// display the bootstrap comparison and the host explicitly approves or denies.
// LAN binding persists, so this spec owns its host. Address entry deliberately
// avoids depending on multicast availability in CI; discovery has its own tests.
import { expect, test, type Page } from '@playwright/test';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { launchFrontendClient } from './frontend-client-helpers.js';
import { seedAgentThread } from './agent-visibility-helpers.js';
import { nonLoopbackIPv4, pairedDevices } from './offhost-helpers.js';

const HOST_NAME = 'Pairing workstation';
const THREAD = 'Conversation on the pairing workstation';

async function settings(page: Page, name: string): Promise<void> {
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await page.getByRole('tab', { name, exact: true }).click();
}

async function openPairing(hostPage: Page): Promise<string> {
  await hostPage.getByRole('button', { name: 'Allow a device to connect', exact: true }).click();
  const dialog = hostPage.getByRole('dialog', { name: 'Allow a device to connect' });
  await expect(dialog.getByRole('radio', { name: 'Local network' })).toBeVisible();
  await dialog.getByRole('button', { name: /^Another computer/ }).click();
  await dialog.getByText('Can’t find this computer?', { exact: true }).click();
  const shown = dialog.getByLabel('Computer address');
  await expect(shown).toBeVisible();
  const address = (await shown.textContent())!.trim();
  const url = new URL(address);
  expect(url.protocol).toBe('https:');
  expect(url.hash).toBe('');
  expect(url.search).toBe('');
  expect(url.hostname).not.toMatch(/^(localhost|127\.|\[?::1)/);
  return address;
}

async function connect(page: Page, address: string): Promise<void> {
  const input = page.getByRole('textbox', { name: 'Computer address or pairing link', exact: true });
  await input.fill(address);
  await input.press('Enter');
}

async function compare(hostPage: Page, client: Page): Promise<void> {
  const hostDialog = hostPage.getByRole('dialog', { name: 'Allow a device to connect' });
  const clientNumber = client.getByTestId('pending-attachment').getByLabel('Verification number');
  await expect(clientNumber).toBeVisible();
  const number = (await clientNumber.textContent())!.trim();
  expect(number.replace(/\s/g, '')).toMatch(/^\d{6}$/);
  await expect(hostDialog.getByLabel('Verification number')).toHaveText(number);
  await expect(hostDialog.getByRole('button', { name: 'It matches — allow' })).toBeEnabled();
  await expect(client.getByTestId('attached-system')).toHaveCount(0);
  await expect(client.getByTestId('thread-row').filter({ hasText: THREAD })).toHaveCount(0);
}

test('desktop address pairing requires matching-number approval and preserves existing connections on rejected attempts', async ({ browser, page }) => {
  test.setTimeout(120_000);
  test.skip(nonLoopbackIPv4() === null, 'A non-loopback interface is required for production LAN pairing.');
  const root = await mkdtemp(join(tmpdir(), 'ao-address-pairing-'));
  let host: HarnessApp | undefined;
  let frontend: Awaited<ReturnType<typeof launchFrontendClient>> | undefined;
  const ownerContext = await browser.newContext();
  const owner = await ownerContext.newPage();
  owner.setDefaultTimeout(10_000);
  page.setDefaultTimeout(10_000);
  const pageErrors: string[] = [];
  for (const screen of [owner, page]) screen.on('pageerror', error => pageErrors.push(error.message));
  try {
    host = await launchHarness();
    await host.rpc('SetDeviceName', HOST_NAME);
    await host.rpc('SetNetworkSettings', { bindAll: true });
    await seedAgentThread(host, 'address-pairing-project', THREAD);
    await host.open(owner);
    await settings(owner, 'Allow device access');
    frontend = await launchFrontendClient(join(root, 'profiles'), join(root, 'frontend'), '');
    await frontend.open(page);
    await settings(page, 'Connect to a computer');

    await test.step('rejecting the comparison leaves no attached computer or usable session', async () => {
      const address = await openPairing(owner);
      await connect(page, address);
      await compare(owner, page);
      await owner.getByRole('button', { name: 'It doesn’t match', exact: true }).click();
      await expect(owner.getByRole('dialog', { name: 'Allow a device to connect' })).toHaveCount(0);
      await expect(page.getByTestId('pending-attachment')).toHaveCount(0);
      await expect(page.getByTestId('attached-system')).toHaveCount(0);
      expect((await pairedDevices(host!)).flatMap(device => device.sessions ?? [])).toHaveLength(0);
    });

    const address = await openPairing(owner);
    await test.step('a fresh matching comparison attaches the host and exposes its real conversation', async () => {
      await connect(page, address);
      await compare(owner, page);
      await owner.getByRole('button', { name: 'It matches — allow', exact: true }).click();
      await expect(owner.getByText('Computer paired', { exact: true })).toBeVisible();
      await owner.getByRole('button', { name: 'Done', exact: true }).click();
      await expect(page.getByTestId('pending-attachment')).toHaveCount(0);
      await expect(page.getByTestId('attached-system')).toHaveCount(1);
      await expect(page.getByTestId('attached-system')).toContainText(HOST_NAME);
      await expect(page.getByTestId('attached-system')).toContainText('Connected');
      await page.getByRole('button', { name: 'Close Settings', exact: true }).click();
      await page.getByTestId('thread-row').filter({ hasText: THREAD }).click();
      await expect(page.getByTestId('chat-header-title')).toHaveText(THREAD);
      await expect(page.getByText('Ready.', { exact: true })).toBeVisible();
      await settings(page, 'Connect to a computer');
    });

    await test.step('typing an already-connected address keeps its saved connection intact', async () => {
      await openPairing(owner);
      await connect(page, address);
      await expect(page.getByRole('alert').filter({ hasText: 'already connected' })).toBeVisible();
      await expect(page.getByTestId('attached-system')).toHaveCount(1);
      await expect(page.getByTestId('attached-system')).toContainText('Connected');
      await expect(page.getByTestId('pending-attachment')).toHaveCount(0);
      await owner.keyboard.press('Escape');
      await expect(owner.getByRole('dialog', { name: 'Allow a device to connect' })).toHaveCount(0);
    });
    await test.step('a computer cannot enroll itself by entering its own address', async () => {
      await openPairing(owner);
      const local = await ownerContext.newPage();
      local.setDefaultTimeout(10_000);
      local.on('pageerror', error => pageErrors.push(error.message));
      await host!.open(local);
      await settings(local, 'Connect to a computer');
      await connect(local, address);
      await expect(local.getByRole('alert').filter({ hasText: 'already using' })).toBeVisible();
      await expect(local.getByTestId('attached-system')).toHaveCount(0);
      await expect(local.getByTestId('pending-attachment')).toHaveCount(0);
      await local.close();
      await owner.keyboard.press('Escape');
    });
    expect(pageErrors).toEqual([]);
  } finally {
    try { await ownerContext.close(); await page.close(); await frontend?.close(); await host?.close(); }
    finally { await rm(root, { recursive: true, force: true, maxRetries: 3 }); }
  }
});
