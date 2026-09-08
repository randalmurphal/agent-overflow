import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import { __setTransportHelloForTest } from '../../stores/transportStatus.svelte';
import ComputerPairingWindow from './ComputerPairingWindow.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

const WINDOW = { id: 'window-1', address: 'https://workstation.tail123.ts.net', expiresAtMs: Date.now() + 300_000 };
const WAITING = { state: 'waiting', verificationNumber: '', deviceLabel: '', linkId: '', expiresAtMs: WINDOW.expiresAtMs };
const READY = { ...WAITING, state: 'ready', verificationNumber: '135 791', deviceLabel: 'Laptop', linkId: 'link-1' };

function show() {
  return render(ComputerPairingWindow, { networkChoice: 'tailnet', access: 'view-only', onClose: vi.fn(), onChanged: vi.fn() });
}

describe('computer pairing window', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    resetBindingMocks();
    setBindingMock('OpenComputerPairing', async () => WINDOW);
    setBindingMock('CloseComputerPairing', async () => {});
  });
  afterEach(() => { __setTransportHelloForTest(null); cleanup(); vi.useRealTimers(); resetBindingMocks(); });

  it('opens personal pairing using the home controller capability', async () => {
    __setTransportHelloForTest({ backendId: '', backendName: 'My computers', capabilities: ['own-devices.v1'], protocolVersion: 1,
      serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 });
    const own = setBindingMock('OpenOwnComputerPairing', async () => WINDOW);
    const ordinary = setBindingMock('OpenComputerPairing', async () => WINDOW);
    setBindingMock('ComputerPairingStatus', async () => WAITING);
    render(ComputerPairingWindow, { networkChoice: 'lan', access: 'full', onClose: vi.fn(), onChanged: vi.fn() });
    await vi.waitFor(() => expect(own).toHaveBeenCalledExactlyOnceWith('lan'));
    expect(ordinary).not.toHaveBeenCalled();
  });

  it('shows the independently derived number but permits approval only once the credential is ready', async () => {
    const open = setBindingMock('OpenComputerPairing', async () => WINDOW);
    const poll = setBindingMock('ComputerPairingStatus', async () => ({ ...READY, state: 'verifying', linkId: '' }));
    const confirm = setBindingMock('ConfirmDevicePairing', async () => {});
    const close = setBindingMock('CloseComputerPairing', async () => {});
    const view = show();
    await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toHaveTextContent('135 791'));
    expect(open).toHaveBeenCalledWith('tailnet', 'view-only');
    expect(view.getByRole('button', { name: 'It matches — allow' })).toBeDisabled();
    poll.mockResolvedValue(READY);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(view.getByRole('button', { name: 'It matches — allow' })).toBeEnabled();
    await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
    expect(confirm).toHaveBeenCalledExactlyOnceWith('link-1');
    await vi.waitFor(() => expect(view.getByText('Computer paired')).toBeTruthy());
    const calls = poll.mock.calls.length;
    await vi.advanceTimersByTimeAsync(10_000);
    expect(poll.mock.calls).toHaveLength(calls);
    await fireEvent.click(view.getByRole('button', { name: 'Done' }));
    view.unmount();
    await vi.waitFor(() => expect(close).toHaveBeenCalledExactlyOnceWith(WINDOW.id));
  });

  it('closes a window whose open response arrives after the dialog was dismissed', async () => {
    let resolve!: (value: typeof WINDOW) => void;
    setBindingMock('OpenComputerPairing', () => new Promise<typeof WINDOW>((done) => { resolve = done; }));
    const close = setBindingMock('CloseComputerPairing', async () => {});
    const poll = setBindingMock('ComputerPairingStatus', async () => WAITING);
    const view = show();
    view.unmount();
    resolve(WINDOW);
    await vi.waitFor(() => expect(close).toHaveBeenCalledExactlyOnceWith(WINDOW.id));
    expect(poll).not.toHaveBeenCalled();
  });

  it('hides stale approval on a failed status read, retries serially, and ignores a late response after closing', async () => {
    const poll = setBindingMock('ComputerPairingStatus', async () => READY);
    const close = setBindingMock('CloseComputerPairing', async () => {});
    const view = show();
    await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toBeTruthy());
    poll.mockRejectedValueOnce(new Error('offline'));
    await vi.advanceTimersByTimeAsync(2_000);
    expect(view.getByRole('alert')).toHaveTextContent('offline');
    expect(view.queryByRole('button', { name: 'It matches — allow' })).toBeNull();
    let resolve!: (value: typeof READY) => void;
    poll.mockImplementationOnce(() => new Promise<typeof READY>((done) => { resolve = done; }));
    await vi.advanceTimersByTimeAsync(2_000);
    const calls = poll.mock.calls.length;
    await vi.advanceTimersByTimeAsync(8_000);
    expect(poll.mock.calls).toHaveLength(calls);
    view.unmount();
    resolve(READY);
    await tick();
    await vi.advanceTimersByTimeAsync(10_000);
    expect(poll.mock.calls).toHaveLength(calls);
    expect(close).toHaveBeenCalledExactlyOnceWith(WINDOW.id);
  });

  it('does not revoke an ambiguous confirmation when its dialog closes before the response', async () => {
    setBindingMock('ComputerPairingStatus', async () => READY);
    let resolve!: () => void;
    setBindingMock('ConfirmDevicePairing', () => new Promise<void>((done) => { resolve = done; }));
    const close = setBindingMock('CloseComputerPairing', async () => {});
    const view = show();
    await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toBeTruthy());
    await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
    view.unmount();
    resolve();
    await tick();
    expect(close).not.toHaveBeenCalled();
  });

  it('reopens a window in place, with the same network choice, after the first one expires', async () => {
    const open = setBindingMock('OpenComputerPairing', async () => WINDOW);
    open.mockResolvedValueOnce({ ...WINDOW, id: 'window-0', expiresAtMs: Date.now() + 3_000 });
    const close = setBindingMock('CloseComputerPairing', async () => {});
    setBindingMock('ComputerPairingStatus', async () => WAITING);
    const view = show();
    await vi.waitFor(() => expect(view.getByRole('status')).toHaveTextContent('Waiting for a computer on Tailscale'));
    await vi.advanceTimersByTimeAsync(4_000);
    expect(view.getByText('Pairing expired.')).toBeTruthy();
    await fireEvent.click(view.getByRole('button', { name: 'Try again' }));
    await vi.waitFor(() => expect(view.getByRole('status')).toHaveTextContent('Waiting for a computer on Tailscale'));
    expect(view.queryByText('Pairing expired.')).toBeNull();
    expect(open).toHaveBeenCalledTimes(2);
    expect(open).toHaveBeenLastCalledWith('tailnet', 'view-only');
    // The expired window is closed on the host rather than left to lapse.
    expect(close).toHaveBeenCalledExactlyOnceWith('window-0');
  });

  it('offers Try again when the window could not be opened', async () => {
    const open = setBindingMock('OpenComputerPairing', async () => WINDOW);
    open.mockRejectedValueOnce(new Error('host offline'));
    setBindingMock('ComputerPairingStatus', async () => WAITING);
    const view = show();
    await vi.waitFor(() => expect(view.getByRole('alert')).toHaveTextContent('Could not start pairing: host offline'));
    await fireEvent.click(view.getByRole('button', { name: 'Try again' }));
    await vi.waitFor(() => expect(view.getByRole('status')).toHaveTextContent('Waiting for a computer'));
    expect(view.queryByRole('alert')).toBeNull();
    expect(open).toHaveBeenCalledTimes(2);
  });

  it('says when this computer is not discoverable and opens its address by default', async () => {
    const poll = setBindingMock('ComputerPairingStatus', async () => WAITING);
    const view = show();
    await vi.waitFor(() => expect(view.getByRole('status')).toHaveTextContent('Waiting for a computer'));
    const details = () => view.getByLabelText('Computer address').closest('details') as HTMLDetailsElement;
    expect(details().open).toBe(false);
    expect(view.queryByText(/Not discoverable/)).toBeNull();
    poll.mockResolvedValue({ ...WAITING, discoveryError: 'mDNS is unavailable' });
    await vi.advanceTimersByTimeAsync(2_000);
    expect(view.getByText('Not discoverable on this network: mDNS is unavailable. Enter the address on the other computer.')).toBeTruthy();
    expect(details().open).toBe(true);
  });

  it('stops retrying when the pairing expires during a host outage', async () => {
    setBindingMock('OpenComputerPairing', async () => ({ ...WINDOW, expiresAtMs: Date.now() + 3_000 }));
    const poll = setBindingMock('ComputerPairingStatus', async () => { throw new Error('offline'); });
    const view = show();
    await vi.waitFor(() => expect(view.getByRole('alert')).toHaveTextContent('offline'));
    await vi.advanceTimersByTimeAsync(4_000);
    expect(view.getByText(/Pairing expired/)).toBeTruthy();
    expect(view.queryByRole('alert')).toBeNull();
    const calls = poll.mock.calls.length;
    await vi.advanceTimersByTimeAsync(10_000);
    expect(poll.mock.calls).toHaveLength(calls);
  });
});

describe('computer pairing window naming', () => {
  beforeEach(() => {
    resetBindingMocks();
    setBindingMock('OpenComputerPairing', async () => WINDOW);
    setBindingMock('CloseComputerPairing', async () => {});
    setBindingMock('ComputerPairingStatus', async () => WAITING);
  });
  afterEach(() => { __setTransportHelloForTest(null); cleanup(); resetBindingMocks(); });

  it('names the computer from a hello that lands after the window opened', async () => {
    const view = show();
    const instruction = () => view.getByText(/and choose/).textContent ?? '';
    expect(instruction()).toContain('this computer');
    __setTransportHelloForTest({ backendId: '', backendName: 'Workstation', capabilities: [], protocolVersion: 1,
      serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 });
    await tick();
    expect(instruction()).toContain('Workstation');
  });
});
