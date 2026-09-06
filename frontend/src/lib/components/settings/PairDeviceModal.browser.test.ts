// The network controls and QR must fit a phone-width settings dialog using
// the real CSS; happy-dom cannot detect clipped or overflowing controls.
import { afterEach, expect, it } from 'vitest';
import { page } from 'vitest/browser';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import '../../../app.css';
import PairDeviceModal from './PairDeviceModal.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { __setTransportHelloForTest } from '../../stores/transportStatus.svelte';

afterEach(() => __setTransportHelloForTest(null));

it.each([360, 1280])('keeps the network choice and QR inside a %ipx viewport', async (width) => {
  await page.viewport(width, 800);
  __setTransportHelloForTest({
    backendId: 'mac', backendName: 'Mac', capabilities: ['pairing.networks.v1'], protocolVersion: 1,
    serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0,
  });
  setBindingMock('GetNetworkSettings', async () => ({ bindAll: true, tailnet: { running: true, dnsName: 'mac.ts.net' } }));
  const mint = setBindingMock('MintDevicePairingOnNetwork', async () => ({
    linkId: 'test-link', url: 'http://192.168.1.20:54321/?t=test#pair=test', expiresAtMs: Date.now() + 300_000,
  }));
  setBindingMock('DevicePairingStatus', async () => ({ state: 'pending' }));
  const view = render(PairDeviceModal, { open: true, remoteReachable: true, onClose() {}, onChanged() {} });
  const lan = await view.findByRole('radio', { name: 'Local network' });
  const tailnet = await view.findByRole('radio', { name: 'Tailscale' });
  const dialog = await view.findByRole('dialog', { name: 'Pair a device' });
  await waitFor(() => {
    for (const element of [dialog, lan, tailnet]) {
      const rect = element.getBoundingClientRect();
      expect(rect.width).toBeGreaterThan(0);
      expect(rect.left).toBeGreaterThanOrEqual(0);
      expect(rect.right).toBeLessThanOrEqual(width);
    }
    expect(dialog.scrollWidth).toBeLessThanOrEqual(dialog.clientWidth);
  });
  await fireEvent.click(lan);
  await fireEvent.click(await view.findByRole('button', { name: /Phone or tablet/ }));
  expect(mint).toHaveBeenCalledWith('phone', 'full', 'lan');
  const qr = await view.findByLabelText('Pairing QR code');
  expect(qr.getBoundingClientRect().width).toBe(200);
  expect(dialog.scrollWidth).toBeLessThanOrEqual(dialog.clientWidth);
});
