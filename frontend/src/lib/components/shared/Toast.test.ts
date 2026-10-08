import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import Toast from './Toast.svelte';
import { addErrorToast, addToast, getToasts, resetToastsForTest } from '../../stores/toast.svelte';
import { resetErrorReportsForTest } from '../../stores/errorReports.svelte';
import { TransportError } from '../../transport/wsClient';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { installAnimateShim } from '../../../test/integration/_helpers';

beforeAll(installAnimateShim);

const writeText = vi.fn(async (_text: string) => {});

function revertFailure(): TransportError {
  return new TransportError('method_error', 'failed', {
    detail: {
      ref: 'rKVz',
      method: 'InterruptAndRevertIfClean',
      at: 1,
      chain: ['interrupt-and-revert: claude rollback: uuid missing', 'claude rollback: uuid missing', 'uuid missing'],
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
    resetToastsForTest();
    delete (navigator as { clipboard?: unknown }).clipboard;
  });

  it('expands the error chain and stays up while expanded', async () => {
    vi.useFakeTimers();
    addErrorToast('Could not undo the message', revertFailure(), 1000);
    const { getByTestId, queryByTestId, getByLabelText } = render(Toast);
    expect(queryByTestId('error-details')).toBeNull();

    await fireEvent.click(getByTestId('error-details-toggle'));
    const details = getByTestId('error-details');
    expect(details.textContent).toContain('InterruptAndRevertIfClean');
    expect(details.textContent).toContain('ref rKVz');
    expect(details.querySelectorAll('li')).toHaveLength(3);
    expect(getByLabelText('Hide details').getAttribute('aria-expanded')).toBe('true');

    vi.advanceTimersByTime(10_000);
    expect(getToasts()).toHaveLength(1);

    await fireEvent.click(getByTestId('error-details-toggle'));
    vi.advanceTimersByTime(1001);
    expect(getToasts()).toHaveLength(0);
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
    addErrorToast('Could not undo the message', revertFailure());
    const { getByLabelText } = render(Toast);
    // The log is read at capture, before the copy click.
    await waitFor(() => expect(getToasts()[0].captured?.backendLog).toBeDefined());
    await fireEvent.click(getByLabelText('Copy error for an agent'));
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    const text = writeText.mock.calls[0][0];
    expect(text).toContain('Agent Overflow error: Could not undo the message');
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
