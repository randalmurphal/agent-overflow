// Real computer enrollment and authorization in both directions. The frontend
// controls each source independently; no model or remote command is executed.
import { test, expect } from '@playwright/test';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { headlessPairing } from './headless-pairing-helpers.js';

interface Peer { id: string; enabled: boolean }

test('enables agent access from home to an attached computer and back again', async ({ page }) => {
  test.setTimeout(90_000);
  page.setDefaultTimeout(10_000);
  let home: HarnessApp | undefined;
  let remote: HarnessApp | undefined;
  try {
    home = await launchHarness();
    remote = await launchHarness();
    const pairing = await headlessPairing(remote);
    let remoteID: string;
    try {
      const attachment = await home.rpc<{ id: string; verificationNumber: string }>('AddBackend', pairing.invite.url);
      await pairing.confirm(attachment.verificationNumber);
      remoteID = attachment.id;
      await home.rpc('RenameBackend', remoteID, 'GPU computer');
    } finally { pairing.close(); }
    await home.open(page);
    await page.getByRole('button', { name: 'Settings', exact: true }).click();
    await page.getByRole('tab', { name: 'Computers', exact: true }).click();
    await expect(page.getByTestId('attached-system')).toContainText('GPU computer');
    await page.getByTestId('home-computer').getByRole('button', { name: 'Access & sharing', exact: true }).click();
    const peers = page.locator('section').filter({ has: page.getByRole('heading', { name: 'Agent access to other computers', exact: true }) });
    await expect(peers).toHaveCount(1);
    await peers.getByRole('button', { name: 'Enable', exact: true }).click();
    await expect(peers.getByRole('button', { name: 'Enabled', exact: true })).toHaveAttribute('aria-pressed', 'true');
    expect(await home.rpc<Peer[]>('ListAgentComputers')).toEqual([expect.objectContaining({ id: remoteID, enabled: true })]);

    // This step mints the invitation on HOME and enrolls the REMOTE source.
    // Both identities and both verification numbers are checked by the app.
    await page.getByRole('combobox', { name: 'Computer', exact: true }).selectOption(remoteID);
    const candidate = peers.getByRole('combobox', { name: 'Computer for agent commands', exact: true });
    await expect(candidate.locator('option')).toHaveCount(2);
    const [homeID] = await candidate.selectOption({ index: 1 });
    expect(homeID).not.toBe(remoteID);
    await peers.getByRole('button', { name: 'Enable access', exact: true }).click();
    await expect(peers.getByRole('button', { name: 'Enabled', exact: true })).toHaveAttribute('aria-pressed', 'true');
    expect(await remote.rpc<Peer[]>('ListAgentComputers')).toEqual([expect.objectContaining({ id: homeID, enabled: true })]);

    await peers.getByRole('button', { name: 'Enabled', exact: true }).click();
    await expect(peers.getByRole('button', { name: 'Enable', exact: true })).toBeVisible();
    expect(await remote.rpc<Peer[]>('ListAgentComputers')).toEqual([expect.objectContaining({ id: homeID, enabled: false })]);
    expect(await home.rpc<Peer[]>('ListAgentComputers')).toEqual([expect.objectContaining({ id: remoteID, enabled: true })]);
  } finally {
    await page.close();
    await home?.close();
    await remote?.close();
  }
});
