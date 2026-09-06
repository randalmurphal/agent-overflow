import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import { __setTransportHelloForTest } from '../../stores/transportStatus.svelte';
import PairDeviceModal from './PairDeviceModal.svelte';
import { setBindingMock, resetBindingMocks } from '../../../test/mocks/bindings-app';

const INVITE = {
  linkId: 'link-1',
  url: 'http://192.168.1.20:54321/?t=tik#pair=abc',
  expiresAtMs: Date.now() + 300_000,
};

function renderModal(props: Partial<{ remoteReachable: boolean; onClose: () => void; onChanged: () => void }> = {}) {
  return render(PairDeviceModal, {
    props: {
      open: true,
      remoteReachable: true,
      onClose: () => {},
      onChanged: () => {},
      ...props,
    },
  });
}

describe('<PairDeviceModal>', () => {
  beforeEach(() => {
    resetBindingMocks();
    __setTransportHelloForTest(null);
  });

  afterEach(() => {
    resetBindingMocks();
    __setTransportHelloForTest(null);
    vi.useRealTimers();
  });

  it('mints for the chosen device class and shows the link to share', async () => {
    const minted = setBindingMock('MintDevicePairing', async () => INVITE);
    setBindingMock('DevicePairingStatus', async () => ({
      linkId: 'link-1',
      state: 'pending',
      expiresAtMs: INVITE.expiresAtMs,
    }));
    const { findByRole, findByLabelText } = renderModal();

    await fireEvent.click(await findByRole('button', { name: /Phone or tablet/ }));
    expect(minted).toHaveBeenCalledWith('phone', 'full');

    const link = (await findByLabelText('Pairing link')) as HTMLInputElement;
    expect(link.value).toBe(INVITE.url);
    // The QR carries the same URL the copy row shows.
    expect(await findByLabelText('Pairing QR code')).toBeTruthy();
  });

  it('offers full access by default and mints view-only when it is picked', async () => {
    const minted = setBindingMock('MintDevicePairing', async () => INVITE);
    setBindingMock('DevicePairingStatus', async () => ({
      linkId: 'link-1',
      state: 'pending',
      expiresAtMs: INVITE.expiresAtMs,
    }));
    const { findByRole } = renderModal();

    const full = await findByRole('radio', { name: 'Full access' });
    const viewOnly = await findByRole('radio', { name: 'View only' });
    expect(full.getAttribute('aria-checked')).toBe('true');
    expect(viewOnly.getAttribute('aria-checked')).toBe('false');

    await fireEvent.click(viewOnly);
    await fireEvent.click(await findByRole('button', { name: /Another computer/ }));
    expect(minted).toHaveBeenCalledWith('browser', 'view-only');
  });

  it('shows the loopback note when the server only listens on this machine', async () => {
    const { findByText } = renderModal({ remoteReachable: false });
    await findByText(/currently reaches this computer only/);
  });

  it('moves to the number comparison once the device redeems, and allows it', async () => {
    vi.useFakeTimers();
    setBindingMock('MintDevicePairing', async () => INVITE);
    let state = 'pending';
    setBindingMock('DevicePairingStatus', async () => ({
      linkId: 'link-1',
      state,
      verificationNumber: state === 'redeemed' ? '135 791' : undefined,
      deviceLabel: state === 'redeemed' ? 'iPhone' : undefined,
      expiresAtMs: INVITE.expiresAtMs,
    }));
    const confirmed = setBindingMock('ConfirmDevicePairing', async () => undefined);
    const changed: string[] = [];
    const { findByRole, findByLabelText, findByText } = renderModal({
      onChanged: () => changed.push('changed'),
    });

    await fireEvent.click(await findByRole('button', { name: /Phone or tablet/ }));
    await findByLabelText('Pairing link');

    state = 'redeemed';
    await vi.advanceTimersByTimeAsync(2_100);
    await findByText('135 791');
    await findByText(/iPhone/);

    await fireEvent.click(await findByRole('button', { name: 'It matches — allow' }));
    await vi.waitFor(() => expect(confirmed).toHaveBeenCalledWith('link-1'));
    await findByText('Device paired');
    expect(changed.length).toBeGreaterThan(1);
  });

  it('lands on the ended stage when the link runs out unopened', async () => {
    vi.useFakeTimers();
    setBindingMock('MintDevicePairing', async () => INVITE);
    setBindingMock('DevicePairingStatus', async () => ({
      linkId: 'link-1',
      state: 'expired',
      expiresAtMs: INVITE.expiresAtMs,
    }));
    const { findByRole, findByText, findByLabelText } = renderModal();

    await fireEvent.click(await findByRole('button', { name: /Phone or tablet/ }));
    await findByLabelText('Pairing link');
    await vi.advanceTimersByTimeAsync(2_100);

    await findByText(/ran out before a device opened it/);
    // A fresh mint is one click away.
    await fireEvent.click(await findByRole('button', { name: 'New link' }));
    await findByRole('button', { name: /Phone or tablet/ });
  });

  it('cancels the link from the share stage and closes', async () => {
    setBindingMock('MintDevicePairing', async () => INVITE);
    setBindingMock('DevicePairingStatus', async () => ({
      linkId: 'link-1',
      state: 'pending',
      expiresAtMs: INVITE.expiresAtMs,
    }));
    const canceled = setBindingMock('CancelDevicePairing', async () => undefined);
    let closed = false;
    const { findByRole, findByLabelText } = renderModal({ onClose: () => (closed = true) });

    await fireEvent.click(await findByRole('button', { name: /Phone or tablet/ }));
    await findByLabelText('Pairing link');
    await fireEvent.click(await findByRole('button', { name: 'Cancel link' }));

    await waitFor(() => expect(canceled).toHaveBeenCalledWith('link-1'));
    await waitFor(() => expect(closed).toBe(true));
  });
});

afterEach(() => { __setTransportHelloForTest(null); resetBindingMocks(); });

function enableNetworkChoice(): void {
  __setTransportHelloForTest({
    backendId: 'mac', backendName: 'Mac', capabilities: ['pairing.networks.v1'], protocolVersion: 1,
    serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0,
  });
}

it.each(['lan', 'tailnet'] as const)('mints the explicitly selected %s route while both networks are enabled', async (network) => {
  enableNetworkChoice();
  setBindingMock('GetNetworkSettings', async () => ({ bindAll: true, tailnet: { running: true, dnsName: 'mac.ts.net' } }));
  const mint = setBindingMock('MintDevicePairingOnNetwork', async () => INVITE);
  const legacy = setBindingMock('MintDevicePairing', async () => INVITE);
  const view = renderModal();
  const lan = await view.findByRole('radio', { name: 'Local network' });
  expect(lan.getAttribute('aria-checked')).toBe('true');
  if (network === 'tailnet') await fireEvent.click(await view.findByRole('radio', { name: 'Tailscale' }));
  await fireEvent.click(await view.findByRole('button', { name: /Phone or tablet/ }));
  await view.findByLabelText('Pairing link');
  expect(mint).toHaveBeenCalledWith('phone', 'full', network);
  expect(legacy).not.toHaveBeenCalled();
});

it('blocks minting until networks load and lets a failed read be retried', async () => {
  enableNetworkChoice();
  const read = setBindingMock('GetNetworkSettings', async () => { throw new Error('offline'); });
  const view = renderModal();
  const phone = await view.findByRole('button', { name: /Phone or tablet/ });
  expect((phone as HTMLButtonElement).disabled).toBe(true);
  await view.findByText(/Could not load networks: offline/);
  read.mockResolvedValue({ bindAll: false, tailnet: { running: true, dnsName: 'mac.ts.net' } });
  await fireEvent.click(await view.findByRole('button', { name: 'Try again' }));
  await view.findByRole('radio', { name: 'Tailscale' });
  expect(view.queryByRole('radio', { name: 'Local network' })).toBeNull();
  expect((phone as HTMLButtonElement).disabled).toBe(false);
});
