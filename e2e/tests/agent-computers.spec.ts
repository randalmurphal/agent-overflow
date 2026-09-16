// Real computer enrollment and authorization in both directions. The frontend
// controls each source independently; no model or remote command is executed.
import { test, expect } from '@playwright/test';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { headlessPairing } from './headless-pairing-helpers.js';
import type { PairingInvite } from './offhost-helpers.js';

interface Peer { id: string; enabled: boolean }

for (const ownDevice of [false, true]) {
test(`enables agent access in both directions (${ownDevice ? 'own devices' : 'manual pairing'})`, async ({ page }) => {
  test.setTimeout(90_000);
  page.setDefaultTimeout(10_000);
  let home: HarnessApp | undefined;
  let remote: HarnessApp | undefined;
  try {
    home = await launchHarness();
    remote = await launchHarness();
    let remoteID: string;
    if (ownDevice) {
      const pairing = await headlessPairing(remote);
      try {
        const attachment = await home.rpc<{ id: string; verificationNumber: string }>('AddBackend', pairing.invite.url);
        await pairing.confirm(attachment.verificationNumber);
        remoteID = attachment.id;
      } finally { pairing.close(); }
    } else {
      const invite = await remote.rpc<PairingInvite>('MintDevicePairing', 'desktop', 'full');
      const attachment = await home.rpc<{ id: string; verificationNumber: string }>('AddBackend', invite.url);
      const status = await remote.rpc<{ verificationNumber: string }>('DevicePairingStatus', invite.linkId);
      expect(status.verificationNumber).toBe(attachment.verificationNumber);
      await remote.rpc('ConfirmDevicePairing', invite.linkId);
      remoteID = attachment.id;
    }
    await home.rpc('RenameBackend', remoteID, 'GPU computer');
    await home.open(page);
    await page.getByRole('button', { name: 'Settings', exact: true }).click();
    await page.getByRole('tab', { name: 'Connect to a computer', exact: true }).click();
    await expect(page.getByTestId('attached-system')).toContainText('GPU computer');
    await page.getByRole('tab', { name: 'Agent remote tools', exact: true }).click();
    const peers = page.locator('section').filter({ has: page.getByRole('heading', { name: 'Agent remote tools', exact: true }) });
    await expect(peers).toHaveCount(1);
    await peers.getByRole('button', { name: 'Enable', exact: true }).click();
    await expect(peers.getByRole('button', { name: 'Enabled', exact: true })).toHaveAttribute('aria-pressed', 'true');
    expect(await home.rpc<Peer[]>('ListAgentComputers')).toEqual([expect.objectContaining({ id: remoteID, enabled: true })]);

    await page.getByRole('combobox', { name: 'Computer', exact: true }).selectOption(remoteID);
    let homeID: string;
    if (ownDevice) {
      // Personal enrollment introduces the reverse connection automatically.
      // It grants connectivity, never agent command access.
      await expect.poll(() => remote!.rpc<Peer[]>('ListAgentComputers')).toEqual([
        expect.objectContaining({ enabled: false }),
      ]);
      [ { id: homeID } ] = await remote.rpc<Peer[]>('ListAgentComputers');
      await peers.getByRole('button', { name: 'Enable', exact: true }).click();
    } else {
      // An ordinary invitation adds only one direction. The UI must mint,
      // verify and confirm a separate invitation to enable the reverse hop.
      const candidate = peers.getByRole('combobox', { name: 'Computer for agent commands', exact: true });
      await expect(candidate.locator('option')).toHaveCount(2);
      [homeID] = await candidate.selectOption({ index: 1 });
      await peers.getByRole('button', { name: 'Enable tools', exact: true }).click();
    }
    expect(homeID).not.toBe(remoteID);
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

}
