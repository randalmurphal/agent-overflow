import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import Toast from './Toast.svelte';
import { addErrorToast, addToast, getToasts, removeToast, resetToastsForTest } from '../../stores/toast.svelte';
import { resetErrorReportsForTest } from '../../stores/errorReports.svelte';
import { TransportError, transportErrorFromFrame } from '../../transport/wsClient';
import { HOME_BACKEND } from '../../transport/backendKey';
import forwardedFrames from '../../../test/fixtures/forwardedRefusalFrames.json';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { installAnimateShim } from '../../../test/integration/_helpers';

beforeAll(installAnimateShim);

const writeText = vi.fn(async (_text: string) => {});

function revertFailure(): TransportError {
  return new TransportError('method_error', 'failed', {
    detail: {
      ref: 'rKVz',
      method: 'RevertConversationAndResendMessage',
      at: 1,
      chain: ['revert and resend: claude rollback: uuid missing', 'claude rollback: uuid missing', 'uuid missing'],
    },
  });
}

describe('<Toast> error reports', () => {
  beforeEach(() => {
    resetToastsForTest();
    resetBindingMocks();
    resetErrorReportsForTest();
    setBindingMock('Version', async () => '1.2.3');
    setBindingMock('GetErrorLogLines', async () => ({ lines: ['transport: failed (id: rKVz)'], found: true }));
    writeText.mockReset();
    writeText.mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true, writable: true });
  });

  afterEach(() => {
    vi.useRealTimers();
    delete (navigator as { clipboard?: unknown }).clipboard;
  });

  // A computer's refusal of the call: the caller's headline cannot say
  // which computer to update, so the reason shows whole beside it, and the
  // toast stays until dismissed (v0.0.15 refusing "Failed to load older
  // messages" used to leave after five seconds, its remedy under Details).
  it('shows a computer\'s refusal beside the headline and keeps it up', async () => {
    vi.useFakeTimers();
    const refusal = new TransportError('bad_params', 'bad parameter 3');
    addErrorToast('Failed to load older messages', refusal);
    const { getByTestId } = render(Toast);
    expect(getByTestId('toast-message').textContent).toBe('Failed to load older messages');
    expect(getByTestId('toast-refusal').textContent).toBe(refusal.message);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(getToasts()).toHaveLength(1);
  });

  it('shows a refusal passed as its own headline once, whole', () => {
    const refusal = new TransportError('method_not_found', 'method not registered');
    addErrorToast(refusal.message, refusal);
    const { getByTestId, queryByTestId } = render(Toast);
    expect(getByTestId('toast-message').textContent).toBe(refusal.message);
    expect(getByTestId('toast-message').className).not.toContain('line-clamp');
    expect(queryByTestId('toast-refusal')).toBeNull();
  });

  // A current computer passing on the refusal of the computer it called,
  // as the Go forwarder sends it (internal/app's
  // TestAForwardedRefusalIsNotThisComputersOwn writes these frames).
  it.each(forwardedFrames.map((frame) => [frame.name, frame] as const))(
    'shows a refusal forwarded by a current computer (%s) beside the headline and keeps it up',
    async (_name, frame) => {
      vi.useFakeTimers();
      const forwarded = transportErrorFromFrame({ ...frame, detail: { ref: 'ref-1', at: 1, ...frame.detail } }, HOME_BACKEND);
      addErrorToast('Could not read remote log', forwarded);
      const { getByTestId } = render(Toast);
      expect(getByTestId('toast-message').textContent).toBe('Could not read remote log');
      expect(getByTestId('toast-refusal').textContent).toBe(frame.message);
      await vi.advanceTimersByTimeAsync(60_000);
      expect(getToasts()).toHaveLength(1);
    },
  );

  // It stays until dismissed, so a caller that reports the same refusal
  // again (a retry, a refresh that fails each time) must not stack it.
  it('shows the same refusal under the same headline once, until it is dismissed', () => {
    const refusal = () => new TransportError('bad_params', 'bad parameter 3');
    const first = addErrorToast('Failed to load older messages', refusal());
    expect(addErrorToast('Failed to load older messages', refusal())).toBe(first);
    expect(getToasts()).toHaveLength(1);
    addErrorToast('Failed to load newer messages', refusal());
    addErrorToast('Failed to load older messages', new TransportError('method_not_found', 'method not registered'));
    expect(getToasts()).toHaveLength(3);
    removeToast(first);
    const again = addErrorToast('Failed to load older messages', refusal());
    expect(again).not.toBe(first);
    expect(getToasts()).toHaveLength(3);
  });

  it('shows every other failure each time it is reported', () => {
    const failure = () => new TransportError('bad_params', 'Choose at least one project for the assistant, or all projects.');
    addErrorToast('Could not save', failure());
    addErrorToast('Could not save', failure());
    expect(getToasts()).toHaveLength(2);
  });

  it('keeps its usual delay and adds nothing for any other failure', async () => {
    vi.useFakeTimers();
    addErrorToast('Could not save', new TransportError('bad_params', 'Choose at least one project for the assistant, or all projects.'));
    const { queryByTestId } = render(Toast);
    expect(queryByTestId('toast-refusal')).toBeNull();
    await vi.advanceTimersByTimeAsync(6_000);
    expect(getToasts()).toHaveLength(0);
  });

  it('expands the error chain and stays up while expanded', async () => {
    vi.useFakeTimers();
    addErrorToast('Could not resend the edited message', revertFailure(), 1000);
    const { getByTestId, queryByTestId, getByLabelText } = render(Toast);
    expect(queryByTestId('error-details')).toBeNull();

    await fireEvent.click(getByTestId('error-details-toggle'));
    const details = getByTestId('error-details');
    expect(details.textContent).toContain('RevertConversationAndResendMessage');
    expect(details.textContent).toContain('ref rKVz');
    expect(details.querySelectorAll('li')).toHaveLength(3);
    expect(getByLabelText('Hide details').getAttribute('aria-expanded')).toBe('true');

    vi.advanceTimersByTime(10_000);
    expect(getToasts()).toHaveLength(1);

    await fireEvent.click(getByTestId('error-details-toggle'));
    vi.advanceTimersByTime(1001);
    expect(getToasts()).toHaveLength(0);
  });

  // An action closes its toast, unless it keeps it (a copy, which the
  // reader may want to repeat).
  it('closes on its action unless the action keeps it', async () => {
    const closes = vi.fn();
    const keeps = vi.fn();
    addToast('info', 'Undo the move', 0, { label: 'Undo', run: closes });
    addErrorToast('Your comment was not sent.', new Error('refused'), 0, { label: 'Copy text', run: keeps, keep: true });
    const { getByText } = render(Toast);
    await fireEvent.click(getByText('Undo'));
    await fireEvent.click(getByText('Copy text'));
    await fireEvent.click(getByText('Copy text'));
    expect(closes).toHaveBeenCalledTimes(1);
    expect(keeps).toHaveBeenCalledTimes(2);
    expect(getToasts().map((toast) => toast.message)).toEqual(['Your comment was not sent.']);
  });

  it('waits while the pointer is on it', async () => {
    vi.useFakeTimers();
    addErrorToast('Could not save', new Error('disk full'), 1000);
    const { getByTestId } = render(Toast);
    await fireEvent.pointerEnter(getByTestId('toast'));
    vi.advanceTimersByTime(5000);
    expect(getToasts()).toHaveLength(1);
    await fireEvent.pointerLeave(getByTestId('toast'));
    vi.advanceTimersByTime(1001);
    expect(getToasts()).toHaveLength(0);
  });

  it('copies the report with the backend log for an agent', async () => {
    addErrorToast('Could not resend the edited message', revertFailure());
    const { getByLabelText } = render(Toast);
    // The log is read at capture, before the copy click.
    await waitFor(() => expect(getToasts()[0].captured?.backendLog).toBeDefined());
    await fireEvent.click(getByLabelText('Copy error for an agent'));
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    const text = writeText.mock.calls[0][0];
    expect(text).toContain('Agent Overflow error: Could not resend the edited message');
    expect(text).toContain('- ref: rKVz');
    expect(text).toContain('3. uuid missing');
    expect(text).toContain('transport: failed (id: rKVz)');
  });

  it('offers no details toggle when the message already says everything', () => {
    addErrorToast('Could not save: disk full', new Error('disk full'));
    const { queryByTestId, getByLabelText } = render(Toast);
    expect(queryByTestId('error-details-toggle')).toBeNull();
    expect(getByLabelText('Copy error for an agent')).toBeTruthy();
  });

  it('gives toasts without an error no report controls', () => {
    addToast('error', 'Failed to copy');
    addToast('success', 'Saved');
    const { queryByLabelText } = render(Toast);
    expect(queryByLabelText('Copy error for an agent')).toBeNull();
  });
});
