// A second computer paired as an agent computer, for specs whose agents run
// remote jobs.
import type { HarnessApp } from '../src/harness.js';
import { expect } from './fixtures.js';

/**
 * Pairs `remote` with `harness` as Build box, which agents may use, and
 * returns its computer id. Both computers are named so no capture shows the
 * host's own name.
 */
export async function pairBuildBox(harness: HarnessApp, remote: HarnessApp): Promise<string> {
  await harness.rpc('SetDeviceName', 'Agent computer');
  await remote.rpc('SetDeviceName', 'Other computer');
  const invite = await remote.rpc<{ url: string; linkId: string }>('MintDevicePairing', 'desktop', 'full');
  const attachment = await harness.rpc<{ id: string }>('AddBackend', invite.url);
  await remote.rpc('ConfirmDevicePairing', invite.linkId);
  const computer = attachment.id;
  await harness.rpc('RenameBackend', computer, 'Build box');
  await expect
    .poll(async () => {
      try {
        await harness.rpc('SetAgentComputerEnabled', computer, true);
        return 'enabled';
      } catch (error) {
        return String(error);
      }
    }, { timeout: 30_000 })
    .toBe('enabled');
  return computer;
}
