import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import { __setTransportHelloForTest } from '../../stores/transportStatus.svelte';
import PairDeviceModal from './PairDeviceModal.svelte';
import { setBindingMock, resetBindingMocks } from '../../../test/mocks/bindings-app';
import { TransportError } from '../../transport/wsClient';

const INVITE = {
  linkId: 'link-1',
  url: 'http://192.168.1.20:54321/?t=tik#pair=abc',
  expiresAtMs: Date.now() + 300_000,
};
const LEGACY_HELLO = {
  backendId: 'mac', backendName: 'Mac', capabilities: [], protocolVersion: 1,
  serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0,
};

function renderModal(props: Partial<{ remoteReachable: boolean; canEnrollOwnDevice: boolean | null; onClose: () => void; onChanged: () => void }> = {}) {
  return render(PairDeviceModal, {
    props: {
      open: true,
      remoteReachable: true,
      canEnrollOwnDevice: true,
      onClose: () => {},
      onChanged: () => {},
      ...props,
    },
  });
}

describe('<PairDeviceModal>', () => {
  beforeEach(() => {
    resetBindingMocks();
    __setTransportHelloForTest(LEGACY_HELLO);
  });

  afterEach(() => {
    resetBindingMocks();
    __setTransportHelloForTest(null);
    vi.useRealTimers();
  });

  it('defaults capable hosts to personal-device pairing while view-only remains restricted', async () => {
    __setTransportHelloForTest({ ...LEGACY_HELLO, capabilities: ['own-devices.v1', 'pairing.networks.v1'] });
    setBindingMock('GetNetworkSettings', async () => ({ bindAll: true }));
    setBindingMock('DevicePairingStatus', async () => ({ state: 'pending', expiresAtMs: INVITE.expiresAtMs }));
    const personal = setBindingMock('MintOwnDevicePairingOnNetwork', async () => INVITE);
    const restricted = setBindingMock('MintDevicePairingOnNetwork', async () => INVITE);
    const view = renderModal();
    expect(await view.findByRole('radio', { name: 'My device' })).toBeChecked();
    expect(view.getByText(/including devices already connected/)).toBeTruthy();
    await waitFor(() => expect(view.getByRole('button', { name: /Phone or tablet/ })).toBeEnabled());
    await fireEvent.click(view.getByRole('button', { name: /Phone or tablet/ }));
    expect(personal).toHaveBeenCalledExactlyOnceWith('phone', 'lan');
    expect(restricted).not.toHaveBeenCalled();
    view.unmount();
    const second = renderModal();
    await fireEvent.click(await second.findByRole('radio', { name: 'View only' }));
    await waitFor(() => expect(second.getByRole('button', { name: /Phone or tablet/ })).toBeEnabled());
    await fireEvent.click(second.getByRole('button', { name: /Phone or tablet/ }));
    expect(restricted).toHaveBeenCalledExactlyOnceWith('phone', 'view-only', 'lan');
    expect(personal).toHaveBeenCalledOnce();
  });

  it('offers ordinary pairing, and says where My device lives, when the backend refuses this caller a personal join', async () => {
    // A passkey-signed browser or an ordinary full-access device sees
    // `own-devices.v1` advertised and is still refused by `ownPairingAdmin`.
    // DevicesSection reads that verdict off `ListOwnDevices().canEnroll`
    // and hands it in; the modal must not offer the option that always
    // failed with a toast (found by harness-passkey-lifecycle case 4).
    __setTransportHelloForTest({ ...LEGACY_HELLO, capabilities: ['own-devices.v1', 'pairing.networks.v1'] });
    setBindingMock('GetNetworkSettings', async () => ({ bindAll: true }));
    setBindingMock('DevicePairingStatus', async () => ({ state: 'pending', expiresAtMs: INVITE.expiresAtMs }));
    const personal = setBindingMock('MintOwnDevicePairingOnNetwork', async () => INVITE);
    const ordinary = setBindingMock('MintDevicePairingOnNetwork', async () => INVITE);
    const view = renderModal({ canEnrollOwnDevice: false });
    expect(await view.findByRole('radio', { name: 'Full access' })).toBeChecked();
    expect(view.queryByRole('radio', { name: 'My device' })).toBeNull();
    expect(view.getByText(/“My device” pairing is available from your own devices/)).toBeTruthy();
    expect(view.queryByText(/including devices already connected/)).toBeNull();
    await waitFor(() => expect(view.getByRole('button', { name: /Phone or tablet/ })).toBeEnabled());
    await fireEvent.click(view.getByRole('button', { name: /Phone or tablet/ }));
    expect(ordinary).toHaveBeenCalledExactlyOnceWith('phone', 'full', 'lan');
    expect(personal).not.toHaveBeenCalled();
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
    expect(minted).not.toHaveBeenCalled();
    await findByRole('button', { name: 'Use a pairing link instead' });
    await fireEvent.click(await findByRole('button', { name: 'Use a pairing link instead' }));
    expect(minted).toHaveBeenCalledWith('browser', 'view-only');
  });

  it('waits for a hello instead of treating unknown support as a legacy host', async () => {
    __setTransportHelloForTest(null);
    const minted = setBindingMock('MintDevicePairing', async () => INVITE);
    const view = renderModal();
    expect(view.getByRole('button', { name: /Another computer/ })).toBeDisabled();
    expect(view.getByText(/Connecting to this computer/)).toBeTruthy();
    expect(view.queryByText('Direct computer pairing is unavailable')).toBeNull();
    __setTransportHelloForTest(LEGACY_HELLO);
    await waitFor(() => expect(view.getByRole('button', { name: /Another computer/ })).toBeEnabled());
    await fireEvent.click(view.getByRole('button', { name: /Another computer/ }));
    expect(view.getByText('Direct computer pairing is unavailable')).toBeTruthy();
    expect(view.queryByLabelText('Pairing QR code')).toBeNull();
    expect(minted).not.toHaveBeenCalled();
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

it('opens discovery pairing for another computer while preserving QR pairing for phones', async () => {
  __setTransportHelloForTest({
    backendId: 'mac', backendName: 'Studio Mac', capabilities: ['pairing.networks.v1', 'pairing.nearby.v1'], protocolVersion: 1,
    serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0,
  });
  setBindingMock('GetNetworkSettings', async () => ({ bindAll: true }));
  const open = setBindingMock('OpenComputerPairing', async () => ({ id: 'window', address: 'https://192.168.1.20:443', expiresAtMs: Date.now() + 300_000 }));
  const close = setBindingMock('CloseComputerPairing', async () => {});
  setBindingMock('ComputerPairingStatus', async () => ({ state: 'waiting', verificationNumber: '', deviceLabel: '', linkId: '', expiresAtMs: Date.now() + 300_000 }));
  const mint = setBindingMock('MintDevicePairingOnNetwork', async () => INVITE);
  const view = renderModal();
  await view.findByRole('radio', { name: 'Local network' });
  await fireEvent.click(view.getByRole('button', { name: /Another computer/ }));
  await view.findByText(/Studio Mac/);
  expect(open).toHaveBeenCalledExactlyOnceWith('lan', 'full');
  expect(mint).not.toHaveBeenCalled();
  await view.rerender({ open: false, remoteReachable: true, onClose: () => {}, onChanged: () => {} });
  await waitFor(() => expect(close).toHaveBeenCalledExactlyOnceWith('window'));
  await view.rerender({ open: true, remoteReachable: true, onClose: () => {}, onChanged: () => {} });
  await view.findByRole('radio', { name: 'Local network' });
  await fireEvent.click(view.getByRole('button', { name: /Phone or tablet/ }));
  await view.findByLabelText('Pairing QR code');
  expect(mint).toHaveBeenCalledExactlyOnceWith('phone', 'full', 'lan');
});

// Each completion applies only to the opening and the link it started in.
// The tests close and reopen the modal between a call and its reply, then
// release the reply.
describe('<PairDeviceModal> across a reopen', () => {
  const PROPS = { remoteReachable: true, canEnrollOwnDevice: true, onChanged: () => {} };
  beforeEach(() => {
    resetBindingMocks();
    __setTransportHelloForTest(LEGACY_HELLO);
  });
  afterEach(() => {
    resetBindingMocks();
    __setTransportHelloForTest(null);
    vi.useRealTimers();
  });

  async function reopen(view: ReturnType<typeof renderModal>, onClose: () => void = () => {}): Promise<void> {
    await view.rerender({ ...PROPS, open: false, onClose });
    await view.rerender({ ...PROPS, open: true, onClose });
    await view.findByRole('button', { name: /Phone or tablet/ });
  }

  async function verifyStage(view: ReturnType<typeof renderModal>): Promise<void> {
    await fireEvent.click(await view.findByRole('button', { name: /Phone or tablet/ }));
    await view.findByLabelText('Pairing link');
    await vi.advanceTimersByTimeAsync(2_100);
    await view.findByText('135 791');
  }

  it('mints again from New link after a link ran out', async () => {
    vi.useFakeTimers();
    const minted = setBindingMock('MintDevicePairing', async () => INVITE);
    setBindingMock('DevicePairingStatus', async () => ({ state: 'expired', expiresAtMs: INVITE.expiresAtMs }));
    const view = renderModal();
    await fireEvent.click(await view.findByRole('button', { name: /Phone or tablet/ }));
    await view.findByLabelText('Pairing link');
    await vi.advanceTimersByTimeAsync(2_100);
    await view.findByText(/ran out before a device opened it/);
    await fireEvent.click(view.getByRole('button', { name: 'New link' }));
    const phone = await view.findByRole('button', { name: /Phone or tablet/ });
    expect(phone).toBeEnabled();
    await fireEvent.click(phone);
    expect(minted).toHaveBeenCalledTimes(2);
  });

  it('leaves a link minted for an earlier opening on the pending row', async () => {
    vi.useFakeTimers();
    let resolve!: (invite: typeof INVITE) => void;
    setBindingMock('MintDevicePairing', () => new Promise<typeof INVITE>((done) => { resolve = done; }));
    const poll = setBindingMock('DevicePairingStatus', async () => ({ state: 'pending', expiresAtMs: INVITE.expiresAtMs }));
    const changed = vi.fn();
    const view = renderModal({ onChanged: changed });
    await fireEvent.click(await view.findByRole('button', { name: /Phone or tablet/ }));
    await reopen(view);
    resolve(INVITE);
    await vi.advanceTimersByTimeAsync(10_000);
    expect(view.queryByLabelText('Pairing link')).toBeNull();
    expect(view.getByRole('button', { name: /Phone or tablet/ })).toBeEnabled();
    expect(poll).not.toHaveBeenCalled();
  });

  it('shows a device another owner view already approved as paired', async () => {
    vi.useFakeTimers();
    setBindingMock('MintDevicePairing', async () => INVITE);
    setBindingMock('DevicePairingStatus', async () => ({ state: 'redeemed', verificationNumber: '135 791', expiresAtMs: INVITE.expiresAtMs }));
    setBindingMock('ConfirmDevicePairing', async () => { throw new TransportError('pairing_already_allowed', 'this device is already allowed'); });
    const view = renderModal();
    await verifyStage(view);
    await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
    await view.findByText('Device paired');
  });

  it('says the access ended, not paired, when the poll finds an approval whose access was removed', async () => {
    vi.useFakeTimers();
    setBindingMock('MintDevicePairing', async () => INVITE);
    const poll = setBindingMock('DevicePairingStatus', async () => ({ state: 'redeemed', verificationNumber: '135 791', expiresAtMs: INVITE.expiresAtMs }));
    const view = renderModal();
    await verifyStage(view);
    poll.mockResolvedValue({ state: 'accessEnded', expiresAtMs: INVITE.expiresAtMs });
    await vi.advanceTimersByTimeAsync(2_100);
    await view.findByText('This device was allowed, and its access has since been removed or expired. Choose New link to pair it again.');
    expect(view.queryByText('Device paired')).toBeNull();
    expect(view.getByRole('button', { name: 'New link' })).toBeEnabled();
  });

  // Answers can arrive out of the host's order; what the modal shows never
  // moves backward. A confirmation that lost its access stays ended.
  it('keeps the ended access when a late poll or a late approval reply says confirmed', async () => {
    vi.useFakeTimers();
    setBindingMock('MintDevicePairing', async () => INVITE);
    const REDEEMED = { state: 'redeemed', verificationNumber: '135 791', expiresAtMs: INVITE.expiresAtMs };
    const poll = setBindingMock('DevicePairingStatus', async () => REDEEMED);
    let approve!: () => void;
    setBindingMock('ConfirmDevicePairing', () => new Promise<void>((done) => { approve = done; }));
    const view = renderModal();
    await verifyStage(view);
    await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
    // A poll that read the link while it was confirmed is still in flight
    // when a later one finds its access removed.
    let late!: (value: unknown) => void;
    poll.mockImplementationOnce(() => new Promise((done) => { late = done; }));
    await vi.advanceTimersByTimeAsync(2_000);
    poll.mockResolvedValue({ state: 'accessEnded', expiresAtMs: INVITE.expiresAtMs });
    await vi.advanceTimersByTimeAsync(2_000);
    const ended = 'This device was allowed, and its access has since been removed or expired. Choose New link to pair it again.';
    await view.findByText(ended);
    late({ state: 'confirmed', expiresAtMs: INVITE.expiresAtMs });
    approve();
    await vi.advanceTimersByTimeAsync(10_000);
    expect(view.getByText(ended)).toBeTruthy();
    expect(view.queryByText('Device paired')).toBeNull();
  });

  // The share stage follows every answer, not only a redemption.
  it.each([
    ['confirmed', 'Device paired'],
    ['accessEnded', 'This device was allowed, and its access has since been removed or expired. Choose New link to pair it again.'],
  ])('leaves the shared link when the poll says %s', async (state, text) => {
    vi.useFakeTimers();
    setBindingMock('MintDevicePairing', async () => INVITE);
    const poll = setBindingMock('DevicePairingStatus', async () => ({ state: 'pending', expiresAtMs: INVITE.expiresAtMs }));
    const view = renderModal();
    await fireEvent.click(await view.findByRole('button', { name: /Phone or tablet/ }));
    await view.findByLabelText('Pairing link');
    poll.mockResolvedValue({ state, expiresAtMs: INVITE.expiresAtMs });
    await vi.advanceTimersByTimeAsync(2_100);
    await view.findByText(text);
    expect(view.queryByLabelText('Pairing link')).toBeNull();
    const calls = poll.mock.calls.length;
    await vi.advanceTimersByTimeAsync(10_000);
    expect(poll.mock.calls).toHaveLength(calls);
  });

  it('does not complete a reopened modal with an approval sent before it closed', async () => {
    vi.useFakeTimers();
    setBindingMock('MintDevicePairing', async () => INVITE);
    setBindingMock('DevicePairingStatus', async () => ({ state: 'redeemed', verificationNumber: '135 791', expiresAtMs: INVITE.expiresAtMs }));
    let resolve!: () => void;
    setBindingMock('ConfirmDevicePairing', () => new Promise<void>((done) => { resolve = done; }));
    const view = renderModal();
    await verifyStage(view);
    await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
    await reopen(view);
    resolve();
    await vi.advanceTimersByTimeAsync(0);
    expect(view.queryByText('Device paired')).toBeNull();
    expect(view.getByRole('button', { name: /Phone or tablet/ })).toBeEnabled();
  });

  it('does not close a reopened modal with a cancellation sent before it closed', async () => {
    setBindingMock('MintDevicePairing', async () => INVITE);
    setBindingMock('DevicePairingStatus', async () => ({ state: 'pending', expiresAtMs: INVITE.expiresAtMs }));
    let resolve!: () => void;
    setBindingMock('CancelDevicePairing', () => new Promise<void>((done) => { resolve = done; }));
    const onClose = vi.fn();
    const view = renderModal({ onClose });
    await fireEvent.click(await view.findByRole('button', { name: /Phone or tablet/ }));
    await view.findByLabelText('Pairing link');
    await fireEvent.click(view.getByRole('button', { name: 'Cancel link' }));
    await reopen(view, onClose);
    resolve();
    await new Promise((done) => setTimeout(done, 0));
    expect(onClose).not.toHaveBeenCalled();
  });

  it('does not move a newer link to the comparison with a poll of the previous one', async () => {
    vi.useFakeTimers();
    setBindingMock('MintDevicePairing', async () => INVITE);
    let resolve!: (status: { state: string; verificationNumber: string; expiresAtMs: number }) => void;
    const poll = setBindingMock('DevicePairingStatus', async () => ({ state: 'pending', expiresAtMs: INVITE.expiresAtMs }));
    poll.mockImplementationOnce(() => new Promise((done) => { resolve = done; }));
    const view = renderModal();
    await fireEvent.click(await view.findByRole('button', { name: /Phone or tablet/ }));
    await view.findByLabelText('Pairing link');
    await vi.advanceTimersByTimeAsync(2_100);
    await reopen(view);
    await fireEvent.click(view.getByRole('button', { name: /Phone or tablet/ }));
    await view.findByLabelText('Pairing link');
    resolve({ state: 'redeemed', verificationNumber: '999 999', expiresAtMs: INVITE.expiresAtMs });
    await vi.advanceTimersByTimeAsync(0);
    expect(view.queryByText('999 999')).toBeNull();
    expect(view.getByLabelText('Pairing link')).toBeTruthy();
  });
});

describe('<PairDeviceModal> across a hello refresh', () => {
  beforeEach(() => resetBindingMocks());
  afterEach(() => {
    resetBindingMocks();
    __setTransportHelloForTest(null);
  });

  it('keeps the shared link when a reconnect republishes the hello with new capabilities', async () => {
    __setTransportHelloForTest(LEGACY_HELLO);
    setBindingMock('MintDevicePairing', async () => INVITE);
    setBindingMock('DevicePairingStatus', async () => ({ linkId: 'link-1', state: 'pending', expiresAtMs: INVITE.expiresAtMs }));
    const networks = setBindingMock('GetNetworkSettings', async () => ({ bindAll: true }));
    const view = renderModal();
    await fireEvent.click(await view.findByRole('button', { name: /Phone or tablet/ }));
    await view.findByLabelText('Pairing link');
    // The backend came back able to name networks. The list loads; the
    // link somebody may already be scanning does not disappear.
    __setTransportHelloForTest({ ...LEGACY_HELLO, capabilities: ['pairing.networks.v1'], serverTimeMs: 1 });
    await waitFor(() => expect(networks).toHaveBeenCalledTimes(1));
    expect(view.getByLabelText('Pairing link')).toBeTruthy();
  });
});
