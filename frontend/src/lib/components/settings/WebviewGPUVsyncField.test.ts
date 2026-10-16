import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import WebviewGPUVsyncField from './WebviewGPUVsyncField.svelte';
import { setBindingMock, resetBindingMocks } from '../../../test/mocks/bindings-app';
import { resetRunMode } from '../../../test/runMode';
import { getToasts, removeToast } from '../../stores/toast.svelte';

const FIELD = '[data-settings-field="performance.disable-gpu-vsync"]';

function clearToasts(): void {
  for (const toast of getToasts()) removeToast(toast.id);
}

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) {
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
}

describe('<WebviewGPUVsyncField>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetRunMode();
    clearToasts();
  });

  afterEach(() => {
    resetBindingMocks();
    resetRunMode();
    clearToasts();
  });

  it('renders nothing off the Windows launcher and never reads the preference there', async () => {
    setBindingMock('IsWSL', vi.fn(async () => false));
    // GetWebviewGPUVsyncDisabled has no mock: a call would throw and toast.
    const { container } = render(WebviewGPUVsyncField);
    await settle();
    expect(container.querySelector(FIELD)).toBeNull();
    expect(getToasts()).toHaveLength(0);
  });

  it('renders the saved value with no restart notice', async () => {
    setBindingMock('IsWSL', vi.fn(async () => true));
    setBindingMock('GetWebviewGPUVsyncDisabled', vi.fn(async () => true));
    const { container, getByRole, queryByRole } = render(WebviewGPUVsyncField);
    await waitFor(() => expect(container.querySelector(FIELD)).not.toBeNull());
    expect(getByRole('switch').getAttribute('aria-checked')).toBe('true');
    expect(queryByRole('status')).toBeNull();
  });

  it('saves through SetWebviewGPUVsyncDisabled, reconciles from the answer, and shows the restart notice until the value is back', async () => {
    setBindingMock('IsWSL', vi.fn(async () => true));
    setBindingMock('GetWebviewGPUVsyncDisabled', vi.fn(async () => false));
    const setMock = setBindingMock('SetWebviewGPUVsyncDisabled', vi.fn(async (value: unknown) => value as boolean));
    const { container, getByRole, queryByRole } = render(WebviewGPUVsyncField);
    await waitFor(() => expect(container.querySelector(FIELD)).not.toBeNull());

    await fireEvent.click(getByRole('switch'));
    await waitFor(() => expect(setMock).toHaveBeenCalledWith(true));
    await waitFor(() => expect(getByRole('switch').getAttribute('aria-checked')).toBe('true'));
    expect(getByRole('status').textContent).toMatch(/next time you open/i);

    // Back to the value the launcher started with: nothing is pending.
    await fireEvent.click(getByRole('switch'));
    await waitFor(() => expect(setMock).toHaveBeenCalledWith(false));
    await waitFor(() => expect(getByRole('switch').getAttribute('aria-checked')).toBe('false'));
    expect(queryByRole('status')).toBeNull();
  });

  it('keeps the saved value and toasts when the write fails', async () => {
    setBindingMock('IsWSL', vi.fn(async () => true));
    setBindingMock('GetWebviewGPUVsyncDisabled', vi.fn(async () => false));
    setBindingMock('SetWebviewGPUVsyncDisabled', vi.fn(async () => {
      throw new Error('write failed');
    }));
    const { container, getByRole, queryByRole } = render(WebviewGPUVsyncField);
    await waitFor(() => expect(container.querySelector(FIELD)).not.toBeNull());

    await fireEvent.click(getByRole('switch'));
    await waitFor(() => {
      expect(getToasts().some((t) => t.type === 'error' && /vsync/i.test(t.message))).toBe(true);
    });
    expect(getByRole('switch').getAttribute('aria-checked')).toBe('false');
    expect(queryByRole('status')).toBeNull();
  });

  it('toasts when the preference cannot be read', async () => {
    setBindingMock('IsWSL', vi.fn(async () => true));
    setBindingMock('GetWebviewGPUVsyncDisabled', vi.fn(async () => {
      throw new Error('decode wsl.json');
    }));
    const { container } = render(WebviewGPUVsyncField);
    await waitFor(() => {
      expect(getToasts().some((t) => t.type === 'error' && /vsync/i.test(t.message))).toBe(true);
    });
    expect(container.querySelector(FIELD)).toBeNull();
  });
});
