import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import { __setTransportHelloForTest } from '../../stores/transportStatus.svelte';
import { getToasts, resetToastsForTest } from '../../stores/toast.svelte';
import ComputerPairingWindow from './ComputerPairingWindow.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { TransportError } from '../../transport/wsClient';

const WINDOW = { id: 'window-1', address: 'https://workstation.tail123.ts.net', expiresAtMs: Date.now() + 300_000 };
const WAITING = { state: 'waiting', verificationNumber: '', deviceLabel: '', linkId: '', expiresAtMs: WINDOW.expiresAtMs };
const READY = { ...WAITING, state: 'ready', verificationNumber: '135 791', deviceLabel: 'Laptop', linkId: 'link-1' };

function show() {
  return render(ComputerPairingWindow, { networkChoice: 'tailnet', access: 'view-only', ownEnroll: false, onClose: vi.fn(), onChanged: vi.fn() });
}

describe('computer pairing window', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    resetBindingMocks();
    setBindingMock('OpenComputerPairing', async () => WINDOW);
    setBindingMock('CloseComputerPairing', async () => {});
  });
  afterEach(() => { __setTransportHelloForTest(null); cleanup(); vi.useRealTimers(); resetBindingMocks(); resetToastsForTest(); });

  it('opens personal pairing where the modal established this caller may enroll', async () => {
    __setTransportHelloForTest({ backendId: '', backendName: 'My computers', capabilities: ['own-devices.v1'], protocolVersion: 1,
      serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 });
    const own = setBindingMock('OpenOwnComputerPairing', async () => WINDOW);
    const ordinary = setBindingMock('OpenComputerPairing', async () => WINDOW);
    setBindingMock('ComputerPairingStatus', async () => WAITING);
    render(ComputerPairingWindow, { networkChoice: 'lan', access: 'full', ownEnroll: true, onClose: vi.fn(), onChanged: vi.fn() });
    await vi.waitFor(() => expect(own).toHaveBeenCalledExactlyOnceWith('lan'));
    expect(ordinary).not.toHaveBeenCalled();
  });

  it('opens ordinary full-access pairing where the backend refuses this caller a personal join', async () => {
    // The capability alone is not the verdict: a passkey-signed browser
    // sees it advertised and is still refused by `ownPairingAdmin`.
    __setTransportHelloForTest({ backendId: '', backendName: 'My computers', capabilities: ['own-devices.v1'], protocolVersion: 1,
      serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 });
    const own = setBindingMock('OpenOwnComputerPairing', async () => WINDOW);
    const ordinary = setBindingMock('OpenComputerPairing', async () => WINDOW);
    setBindingMock('ComputerPairingStatus', async () => WAITING);
    render(ComputerPairingWindow, { networkChoice: 'lan', access: 'full', ownEnroll: false, onClose: vi.fn(), onChanged: vi.fn() });
    await vi.waitFor(() => expect(ordinary).toHaveBeenCalledExactlyOnceWith('lan', 'full'));
    expect(own).not.toHaveBeenCalled();
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

  // The host explains an unsaved cancellation on its own: the window closed
  // and its pairing stays refused, which "it will expire" would contradict.
  it("shows the host's sentence alone when the close could not save its cancellation", async () => {
    setBindingMock('ComputerPairingStatus', async () => WAITING);
    setBindingMock('CloseComputerPairing', async () => {
      throw new TransportError('pairing_close_unfinished', 'this computer could not save the cancellation yet; it keeps the pairing refused and tries again on its own');
    });
    const view = show();
    await vi.waitFor(() => expect(view.getByRole('status')).toHaveTextContent('Waiting for a computer'));
    view.unmount();
    await vi.waitFor(() => expect(getToasts().map((toast) => [toast.type, toast.message])).toEqual([
      ['warning', 'This computer could not save the cancellation yet; it keeps the pairing refused and tries again on its own.'],
    ]));
  });

  it('closes a window whose open response arrives after the dialog was dismissed', async () => {
    let resolve!: (value: typeof WINDOW) => void;
    const open = setBindingMock('OpenComputerPairing', () => new Promise<typeof WINDOW>((done) => { resolve = done; }));
    const close = setBindingMock('CloseComputerPairing', async () => {});
    const poll = setBindingMock('ComputerPairingStatus', async () => WAITING);
    const view = show();
    await vi.waitFor(() => expect(open).toHaveBeenCalledOnce());
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

  // The approval and the close are separate calls the host may run in
  // either order, so the close is sent only once the approval has settled.
  it('closes its window only after an approval sent before dismissal has succeeded', async () => {
    setBindingMock('ComputerPairingStatus', async () => READY);
    const order: string[] = [];
    let resolve!: () => void;
    setBindingMock('ConfirmDevicePairing', () => new Promise<void>((done) => { resolve = () => { order.push('approved'); done(); }; }));
    const close = setBindingMock('CloseComputerPairing', async () => { order.push('close'); });
    const view = show();
    await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toBeTruthy());
    await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
    view.unmount();
    await vi.advanceTimersByTimeAsync(10_000);
    expect(close).not.toHaveBeenCalled();
    resolve();
    await vi.waitFor(() => expect(close).toHaveBeenCalledExactlyOnceWith(WINDOW.id));
    expect(order).toEqual(['approved', 'close']);
    expect(getToasts()).toHaveLength(0);
  });

  it('reports an approval that fails after its dialog was dismissed, then closes its window', async () => {
    setBindingMock('ComputerPairingStatus', async () => READY);
    const order: string[] = [];
    let reject!: (err: Error) => void;
    setBindingMock('ConfirmDevicePairing', () => new Promise<void>((_, fail) => { reject = (err) => { order.push('refused'); fail(err); }; }));
    const close = setBindingMock('CloseComputerPairing', async () => { order.push('close'); });
    const view = show();
    await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toBeTruthy());
    await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
    view.unmount();
    await vi.advanceTimersByTimeAsync(10_000);
    expect(close).not.toHaveBeenCalled();
    reject(new Error('this pairing is no longer open; start pairing again'));
    await vi.waitFor(() => expect(getToasts().map((toast) => toast.message)).toContain('Could not allow this computer: this pairing is no longer open; start pairing again'));
    await vi.waitFor(() => expect(close).toHaveBeenCalledExactlyOnceWith(WINDOW.id));
    expect(order).toEqual(['refused', 'close']);
  });

  // The window can pass its deadline while an approval admitted before it is
  // still writing. That approval decides this window; no replacement opens
  // until it has.
  it('offers no Try again while an approval is outstanding, and shows that approval\'s result', async () => {
    const open = setBindingMock('OpenComputerPairing', async () => ({ ...WINDOW, expiresAtMs: Date.now() + 3_000 }));
    setBindingMock('ComputerPairingStatus', async () => READY);
    let resolve!: () => void;
    setBindingMock('ConfirmDevicePairing', () => new Promise<void>((done) => { resolve = done; }));
    const view = show();
    await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toBeTruthy());
    await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
    await vi.advanceTimersByTimeAsync(4_000);
    expect(view.getByText('Pairing expired.')).toBeTruthy();
    expect(view.getByRole('button', { name: 'Try again' })).toBeDisabled();
    await fireEvent.click(view.getByRole('button', { name: 'Try again' }));
    expect(open).toHaveBeenCalledTimes(1);
    resolve();
    await vi.waitFor(() => expect(view.getByText('Computer paired')).toBeTruthy());
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
    // The host's open retires the window it replaces: Try again sends no close.
    expect(close).not.toHaveBeenCalled();
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

describe('computer pairing window decisions', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    resetBindingMocks();
    setBindingMock('CloseComputerPairing', async () => {});
  });
  afterEach(() => { cleanup(); vi.useRealTimers(); resetBindingMocks(); resetToastsForTest(); });

  // Opening a window retires the previous one on the host, so a dialog
  // opened while a dismissed dialog's approval is in flight waits for it.
  it('opens a new window only after the approval a dismissed dialog sent has settled', async () => {
    const order: string[] = [];
    const open = setBindingMock('OpenComputerPairing', async () => { order.push('open'); return WINDOW; });
    setBindingMock('ComputerPairingStatus', async () => READY);
    let resolve!: () => void;
    setBindingMock('ConfirmDevicePairing', () => new Promise<void>((done) => { resolve = () => { order.push('approved'); done(); }; }));
    setBindingMock('CloseComputerPairing', async () => { order.push('close'); });
    const first = show();
    await vi.waitFor(() => expect(first.getByLabelText('Verification number')).toBeTruthy());
    await fireEvent.click(first.getByRole('button', { name: 'It matches — allow' }));
    first.unmount();
    show();
    await vi.advanceTimersByTimeAsync(5_000);
    expect(open).toHaveBeenCalledTimes(1);
    resolve();
    await vi.waitFor(() => expect(open).toHaveBeenCalledTimes(2));
    expect(order.slice(0, 2)).toEqual(['open', 'approved']);
  });

  it('shows a computer as paired when the approval reply is lost after the host admitted it', async () => {
    setBindingMock('OpenComputerPairing', async () => WINDOW);
    const poll = setBindingMock('ComputerPairingStatus', async () => READY);
    setBindingMock('ConfirmDevicePairing', async () => {
      poll.mockResolvedValue({ ...WAITING, state: 'confirmed', linkId: 'link-1', deviceLabel: 'Laptop' });
      throw new Error('connection lost');
    });
    const view = show();
    await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toBeTruthy());
    await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
    await vi.waitFor(() => expect(view.getByText('Computer paired')).toBeTruthy());
    expect(view.queryByRole('alert')).toBeNull();
  });

  it('reads the host once more at the deadline, and shows an approval admitted before it', async () => {
    setBindingMock('OpenComputerPairing', async () => ({ ...WINDOW, expiresAtMs: Date.now() + 3_000 }));
    const poll = setBindingMock('ComputerPairingStatus', async () => READY);
    const view = show();
    await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toBeTruthy());
    await vi.advanceTimersByTimeAsync(2_000);
    poll.mockResolvedValue({ ...WAITING, state: 'confirmed', linkId: 'link-1' });
    await vi.advanceTimersByTimeAsync(2_000);
    expect(view.getByText('Computer paired')).toBeTruthy();
    expect(view.queryByText('Pairing expired.')).toBeNull();
  });

  it('shows expiry when the read at the deadline finds no decision', async () => {
    setBindingMock('OpenComputerPairing', async () => ({ ...WINDOW, expiresAtMs: Date.now() + 3_000 }));
    const poll = setBindingMock('ComputerPairingStatus', async () => READY);
    const view = show();
    await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toBeTruthy());
    await vi.advanceTimersByTimeAsync(4_000);
    expect(view.getByText('Pairing expired.')).toBeTruthy();
    const calls = poll.mock.calls.length;
    await vi.advanceTimersByTimeAsync(10_000);
    expect(poll.mock.calls).toHaveLength(calls);
  });

  // Each reason the host records for a window's end (ComputerPairingView.Ended)
  // reads as its own sentence; only the deadline reads as expiry.
  it.each([
    ['deadline', 'Pairing expired.'],
    ['canceled', 'Pairing was canceled from Devices.'],
    ['networkChanged', 'The network of this computer changed, so this pairing ended. Choose Try again to start a new one.'],
    ['disconnected', 'Pairing stopped because the connection to this computer dropped.'],
    ['replaced', 'A newer pairing was started on this computer, which ended this one.'],
    ['closed', 'Pairing was closed.'],
    ['stopped', 'Pairing stopped because this computer shut down.'],
    ['accessEnded', 'This computer was paired, and its access has since ended or expired. Choose Try again to pair it again.'],
    ['deviceRemoved', 'This computer was paired, and its access was removed in Devices. Choose Restore or Forget on it in Devices, then choose Try again.'],
    ['', 'Pairing ended.'],
  ])('says why the window ended: %s', async (ended, text) => {
    setBindingMock('OpenComputerPairing', async () => WINDOW);
    setBindingMock('ComputerPairingStatus', async () => ({ ...WAITING, state: 'expired', ended }));
    const view = show();
    await vi.waitFor(() => expect(view.getByText(text)).toBeTruthy());
    expect(view.getByRole('button', { name: 'Try again' })).toBeEnabled();
  });

  it('says why a window ended when the read at its deadline finds it ended for another reason', async () => {
    setBindingMock('OpenComputerPairing', async () => ({ ...WINDOW, expiresAtMs: Date.now() + 3_000 }));
    const poll = setBindingMock('ComputerPairingStatus', async () => WAITING);
    const view = show();
    await vi.waitFor(() => expect(view.getByRole('status')).toHaveTextContent('Waiting for a computer'));
    await vi.advanceTimersByTimeAsync(2_000);
    poll.mockResolvedValue({ ...WAITING, state: 'expired', ended: 'networkChanged' });
    await vi.advanceTimersByTimeAsync(2_000);
    expect(view.getByText(/The network of this computer changed/)).toBeTruthy();
    expect(view.queryByText('Pairing expired.')).toBeNull();
  });

  it('shows why the window ended beside an approval it refused', async () => {
    setBindingMock('OpenComputerPairing', async () => WINDOW);
    const poll = setBindingMock('ComputerPairingStatus', async () => READY);
    setBindingMock('ConfirmDevicePairing', async () => {
      poll.mockResolvedValue({ ...WAITING, state: 'expired', ended: 'networkChanged' });
      throw new Error('this pairing is no longer open; start pairing again');
    });
    const view = show();
    await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toBeTruthy());
    await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
    await vi.waitFor(() => expect(view.getByText(/The network of this computer changed/)).toBeTruthy());
    expect(view.getByRole('alert')).toHaveTextContent('Could not allow this computer: this pairing is no longer open; start pairing again');
    expect(view.queryByLabelText('Verification number')).toBeNull();
  });

  // Another owner view approved it and the device's access was then
  // removed: the refusal and the ending both show, and nothing says paired.
  it('shows that an approved computer lost its access beside the approval it refused', async () => {
    setBindingMock('OpenComputerPairing', async () => WINDOW);
    const poll = setBindingMock('ComputerPairingStatus', async () => READY);
    setBindingMock('ConfirmDevicePairing', async () => {
      poll.mockResolvedValue({ ...WAITING, state: 'expired', ended: 'accessEnded' });
      throw new TransportError('pairing_access_ended', 'this device was allowed, and its access has since ended or expired; pair it again');
    });
    const view = show();
    await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toBeTruthy());
    await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
    await vi.waitFor(() => expect(view.getByText(/its access has since ended or expired. Choose Try again/)).toBeTruthy());
    expect(view.getByRole('alert')).toHaveTextContent('Could not allow this computer: this device was allowed, and its access has since ended or expired; pair it again');
    expect(view.queryByText('Computer paired')).toBeNull();
    expect(view.getByRole('button', { name: 'Try again' })).toBeEnabled();
  });

  // An approval sent just before the deadline; the deadline's read fails, so
  // the dialog ends the window itself; the host then says why it ended.
  it("replaces its own expiry with the host's ending, and keeps the approval controls ended", async () => {
    setBindingMock('OpenComputerPairing', async () => ({ ...WINDOW, expiresAtMs: Date.now() + 3_000 }));
    const poll = setBindingMock('ComputerPairingStatus', async () => READY);
    let refuse!: () => void;
    setBindingMock('ConfirmDevicePairing', () => new Promise<void>((_, reject) => {
      refuse = () => reject(new Error('this pairing is no longer open; start pairing again'));
    }));
    const view = show();
    await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toBeTruthy());
    await vi.advanceTimersByTimeAsync(2_000);
    await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
    poll.mockRejectedValue(new Error('connection lost'));
    await vi.advanceTimersByTimeAsync(2_000);
    await vi.waitFor(() => expect(view.getByText('Pairing expired.')).toBeTruthy());
    poll.mockResolvedValue({ ...WAITING, state: 'expired', ended: 'networkChanged' });
    refuse();
    await vi.waitFor(() => expect(view.getByText(/The network of this computer changed/)).toBeTruthy());
    expect(view.queryByText('Pairing expired.')).toBeNull();
    expect(view.getByRole('alert')).toHaveTextContent('Could not allow this computer: this pairing is no longer open; start pairing again');
    expect(view.queryByLabelText('Verification number')).toBeNull();
    expect(view.queryByRole('button', { name: 'It matches — allow' })).toBeNull();
    poll.mockResolvedValue(READY);
    await vi.advanceTimersByTimeAsync(10_000);
    expect(view.queryByLabelText('Verification number')).toBeNull();
  });

  // A confirmation whose access was then removed never reads as paired
  // again, whether the late `confirmed` comes from a poll or from the
  // approval's own reply.
  it('keeps the ended access when a late poll says confirmed', async () => {
    setBindingMock('OpenComputerPairing', async () => WINDOW);
    const ACCESS_ENDED = { ...WAITING, state: 'expired', ended: 'accessEnded' };
    let late!: (value: unknown) => void;
    const answers: Array<() => Promise<unknown>> = [async () => READY, () => new Promise((done) => { late = done; })];
    const status = setBindingMock('ComputerPairingStatus', () => (answers.shift() ?? (async () => ACCESS_ENDED))());
    setBindingMock('ConfirmDevicePairing', async () => { throw new TransportError('pairing_access_ended', 'this device was allowed, and its access has since ended or expired; pair it again'); });
    const view = show();
    await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toBeTruthy());
    await vi.advanceTimersByTimeAsync(2_000);
    expect(status).toHaveBeenCalledTimes(2);
    await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
    await vi.waitFor(() => expect(view.getByText(/its access has since ended or expired\. Choose Try again/)).toBeTruthy());
    late({ ...READY, state: 'confirmed' });
    await vi.advanceTimersByTimeAsync(10_000);
    expect(view.queryByText('Computer paired')).toBeNull();
    expect(view.getByText(/its access has since ended or expired\. Choose Try again/)).toBeTruthy();
  });

  it('shows the access ended when a read in flight finds it after the approval was confirmed', async () => {
    setBindingMock('OpenComputerPairing', async () => WINDOW);
    let late!: (value: unknown) => void;
    const answers: Array<() => Promise<unknown>> = [async () => READY, () => new Promise((done) => { late = done; })];
    setBindingMock('ComputerPairingStatus', () => (answers.shift() ?? (async () => READY))());
    let approve!: () => void;
    setBindingMock('ConfirmDevicePairing', () => new Promise<void>((done) => { approve = done; }));
    const view = show();
    await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toBeTruthy());
    await vi.advanceTimersByTimeAsync(2_000);
    await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
    approve();
    await vi.waitFor(() => expect(view.getByText('Computer paired')).toBeTruthy());
    late({ ...WAITING, state: 'expired', ended: 'accessEnded' });
    await vi.waitFor(() => expect(view.getByText(/its access has since ended or expired\. Choose Try again/)).toBeTruthy());
    expect(view.queryByText('Computer paired')).toBeNull();
  });

  // An approval admitted before the deadline can land after the host
  // already said the window ended at its deadline.
  it('shows an approval that lands after the host said the window reached its deadline as paired', async () => {
    setBindingMock('OpenComputerPairing', async () => WINDOW);
    const poll = setBindingMock('ComputerPairingStatus', async () => READY);
    let approve!: () => void;
    setBindingMock('ConfirmDevicePairing', () => new Promise<void>((done) => { approve = done; }));
    const view = show();
    await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toBeTruthy());
    await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
    poll.mockResolvedValue({ ...WAITING, state: 'expired', ended: 'deadline' });
    await vi.advanceTimersByTimeAsync(2_000);
    await vi.waitFor(() => expect(view.getByText('Pairing expired.')).toBeTruthy());
    approve();
    await vi.waitFor(() => expect(view.getByText('Computer paired')).toBeTruthy());
  });

  it('keeps the ended access when the approval reply arrives after it', async () => {
    setBindingMock('OpenComputerPairing', async () => WINDOW);
    const poll = setBindingMock('ComputerPairingStatus', async () => READY);
    let approve!: () => void;
    setBindingMock('ConfirmDevicePairing', () => new Promise<void>((done) => { approve = done; }));
    const view = show();
    await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toBeTruthy());
    await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
    poll.mockResolvedValue({ ...WAITING, state: 'expired', ended: 'accessEnded' });
    await vi.advanceTimersByTimeAsync(2_000);
    await vi.waitFor(() => expect(view.getByText(/its access has since ended or expired\. Choose Try again/)).toBeTruthy());
    approve();
    await vi.advanceTimersByTimeAsync(10_000);
    expect(view.queryByText('Computer paired')).toBeNull();
  });

  // Another owner view approves while this Allow is in flight, a poll reads
  // it confirmed, the computer then loses its access, and the status read
  // fails: the Allow's refusal names the loss, and the dialog shows it.
  it.each([
    ['pairing_access_ended', 'This computer was paired, and its access has since ended or expired. Choose Try again to pair it again.'],
    ['pairing_device_removed', 'This computer was paired, and its access was removed in Devices. Choose Restore or Forget on it in Devices, then choose Try again.'],
  ])('shows the lost access an approval names after a poll said confirmed and the status is unreachable: %s', async (code, text) => {
    setBindingMock('OpenComputerPairing', async () => WINDOW);
    const poll = setBindingMock('ComputerPairingStatus', async () => READY);
    let refuse!: () => void;
    setBindingMock('ConfirmDevicePairing', () => new Promise<void>((_, reject) => {
      refuse = () => reject(new TransportError(code, 'this device was allowed, and has since lost its access'));
    }));
    const view = show();
    await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toBeTruthy());
    await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
    poll.mockResolvedValue({ ...READY, state: 'confirmed' });
    await vi.advanceTimersByTimeAsync(2_000);
    await vi.waitFor(() => expect(view.getByText('Computer paired')).toBeTruthy());
    poll.mockRejectedValue(new Error('connection lost'));
    refuse();
    await vi.waitFor(() => expect(view.getByText(text)).toBeTruthy());
    expect(view.getByRole('alert')).toHaveTextContent('Could not allow this computer: this device was allowed, and has since lost its access');
    expect(view.queryByText('Computer paired')).toBeNull();
  });

  // A refusal that names the link's state shows in the dialog only while the
  // dialog follows the window and holds that same state; otherwise it is a toast.
  it.each(['pairing_access_ended', 'pairing_device_removed'])('reports a named refusal that arrives after its dialog was dismissed: %s', async (code) => {
    setBindingMock('OpenComputerPairing', async () => WINDOW);
    setBindingMock('ComputerPairingStatus', async () => READY);
    let refuse!: () => void;
    setBindingMock('ConfirmDevicePairing', () => new Promise<void>((_, reject) => {
      refuse = () => reject(new TransportError(code, 'this device was allowed, and has since lost its access'));
    }));
    const view = show();
    await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toBeTruthy());
    await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
    view.unmount();
    refuse();
    await vi.waitFor(() => expect(getToasts().map((toast) => toast.message)).toEqual(['Could not allow this computer: this device was allowed, and has since lost its access']));
  });

  it('reports a removal as a toast when the dialog already shows the access ended', async () => {
    setBindingMock('OpenComputerPairing', async () => WINDOW);
    const poll = setBindingMock('ComputerPairingStatus', async () => READY);
    let refuse!: () => void;
    setBindingMock('ConfirmDevicePairing', () => new Promise<void>((_, reject) => {
      refuse = () => reject(new TransportError('pairing_device_removed', 'this device was allowed, and its access was removed in Devices'));
    }));
    const view = show();
    await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toBeTruthy());
    await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
    poll.mockResolvedValue({ ...WAITING, state: 'expired', ended: 'accessEnded' });
    await vi.advanceTimersByTimeAsync(2_000);
    await vi.waitFor(() => expect(view.getByText(/its access has since ended or expired\. Choose Try again/)).toBeTruthy());
    refuse();
    await vi.waitFor(() => expect(getToasts().map((toast) => toast.message)).toEqual(['Could not allow this computer: this device was allowed, and its access was removed in Devices']));
    expect(view.queryByRole('alert')).toBeNull();
  });

  it('shows a computer another owner view already approved as paired', async () => {
    setBindingMock('OpenComputerPairing', async () => WINDOW);
    setBindingMock('ComputerPairingStatus', async () => READY);
    setBindingMock('ConfirmDevicePairing', async () => { throw new TransportError('pairing_already_allowed', 'this device is already allowed'); });
    const view = show();
    await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toBeTruthy());
    await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
    await vi.waitFor(() => expect(view.getByText('Computer paired')).toBeTruthy());
    expect(view.queryByRole('alert')).toBeNull();
  });

  // Two reads of one window can answer in either order. Once the dialog
  // shows the window ended, an older read's "ready" changes nothing.
  describe('a stale read after the window ended', () => {
    const ENDED = { ...WAITING, state: 'expired', ended: 'networkChanged' };
    function held() {
      let answer!: (value: unknown) => void;
      const reply = new Promise((done) => { answer = done; });
      return { reply, answer };
    }

    it('keeps the ending when the poll in flight answers after the approval\'s follow-up read', async () => {
      setBindingMock('OpenComputerPairing', async () => WINDOW);
      const poll = held();
      const answers: Array<() => Promise<unknown>> = [async () => READY, () => poll.reply, async () => ENDED];
      const status = setBindingMock('ComputerPairingStatus', () => (answers.shift() ?? (async () => ENDED))());
      setBindingMock('ConfirmDevicePairing', async () => { throw new Error('this pairing is no longer open; start pairing again'); });
      const view = show();
      await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toBeTruthy());
      await vi.advanceTimersByTimeAsync(2_000);
      expect(status).toHaveBeenCalledTimes(2);
      await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
      await vi.waitFor(() => expect(view.getByText(/The network of this computer changed/)).toBeTruthy());
      poll.answer(READY);
      await vi.advanceTimersByTimeAsync(10_000);
      expect(view.getByText(/The network of this computer changed/)).toBeTruthy();
      expect(view.queryByLabelText('Verification number')).toBeNull();
      expect(view.queryByRole('button', { name: 'It matches — allow' })).toBeNull();
      expect(view.getByRole('alert')).toHaveTextContent('Could not allow this computer');
      expect(status).toHaveBeenCalledTimes(3);
    });

    it('keeps the ending when the approval\'s follow-up read answers after the poll', async () => {
      setBindingMock('OpenComputerPairing', async () => WINDOW);
      const followUp = held();
      const answers: Array<() => Promise<unknown>> = [async () => READY, () => followUp.reply, async () => ENDED];
      const status = setBindingMock('ComputerPairingStatus', () => (answers.shift() ?? (async () => ENDED))());
      setBindingMock('ConfirmDevicePairing', async () => { throw new Error('this pairing is no longer open; start pairing again'); });
      const view = show();
      await vi.waitFor(() => expect(view.getByLabelText('Verification number')).toBeTruthy());
      await fireEvent.click(view.getByRole('button', { name: 'It matches — allow' }));
      await vi.waitFor(() => expect(status).toHaveBeenCalledTimes(2));
      await vi.advanceTimersByTimeAsync(2_000);
      await vi.waitFor(() => expect(view.getByText(/The network of this computer changed/)).toBeTruthy());
      followUp.answer(READY);
      await vi.advanceTimersByTimeAsync(10_000);
      expect(view.getByText(/The network of this computer changed/)).toBeTruthy();
      expect(view.queryByLabelText('Verification number')).toBeNull();
      expect(view.getByRole('alert')).toHaveTextContent('Could not allow this computer');
      expect(status).toHaveBeenCalledTimes(3);
    });
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
