import { afterEach, expect, it } from 'vitest';
import { page } from 'vitest/browser';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import '../../../app.css';
import PairDeviceModal from './PairDeviceModal.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { __setTransportHelloForTest } from '../../stores/transportStatus.svelte';

afterEach(() => { cleanup(); resetBindingMocks(); __setTransportHelloForTest(null); });

it.each([360, 1280])('keeps computer verification readable and approval reachable at %ipx', async (width) => {
  await page.viewport(width, 800);
  __setTransportHelloForTest({
    backendId: 'mac', backendName: 'Development workstation', capabilities: ['pairing.networks.v1', 'pairing.nearby.v1'], protocolVersion: 1,
    serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0,
  });
  setBindingMock('GetNetworkSettings', async () => ({ bindAll: false, tailnet: { running: true, dnsName: 'workstation.ts.net' } }));
  setBindingMock('OpenComputerPairing', async () => ({ id: 'window', address: 'https://workstation.ts.net', expiresAtMs: Date.now() + 300_000 }));
  setBindingMock('CloseComputerPairing', async () => {});
  setBindingMock('ComputerPairingStatus', async () => ({ state: 'ready', verificationNumber: '135 791', deviceLabel: 'My other development workstation', linkId: 'link', expiresAtMs: Date.now() + 300_000 }));
  const confirm = setBindingMock('ConfirmDevicePairing', async () => {});
  const view = render(PairDeviceModal, { open: true, remoteReachable: true, canEnrollOwnDevice: true, onClose: () => {}, onChanged: () => {} });
  await view.findByRole('radio', { name: 'Tailscale' });
  await fireEvent.click(view.getByRole('button', { name: /Another computer/ }));
  const number = await view.findByLabelText('Verification number');
  const allow = view.getByRole('button', { name: 'It matches — allow' });
  const reject = view.getByRole('button', { name: 'It doesn’t match' });
  await waitFor(() => {
    for (const element of [number, allow, reject]) {
      const rect = element.getBoundingClientRect();
      expect(rect.width).toBeGreaterThan(0);
      expect(rect.left).toBeGreaterThanOrEqual(0);
      expect(rect.right).toBeLessThanOrEqual(width);
      expect(rect.bottom).toBeLessThanOrEqual(800);
    }
  });
  await page.getByRole('button', { name: 'It matches — allow' }).click();
  await waitFor(() => expect(confirm).toHaveBeenCalledExactlyOnceWith('link'));
  await view.findByText('Computer paired');
});
