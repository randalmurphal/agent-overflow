import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import WebviewGPUVsyncField from './WebviewGPUVsyncField.svelte';
import { setBindingMock, resetBindingMocks } from '../../../test/mocks/bindings-app';
import { resetRunMode } from '../../../test/runMode';
import { getToasts, removeToast } from '../../stores/toast.svelte';

const FIELD = '[data-settings-field="performance.disable-gpu-vsync"]';

/** The backend's answer: the saved preference and the session's launch value. */
function vsync(disabled: boolean, launched = disabled) {
  return { value: disabled, launched };
}

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
    setBindingMock('GetWebviewGPUVsyncDisabled', vi.fn(async () => vsync(true)));
    const { container, getByRole, queryByRole } = render(WebviewGPUVsyncField);
    await waitFor(() => expect(container.querySelector(FIELD)).not.toBeNull());
    expect(getByRole('switch').getAttribute('aria-checked')).toBe('true');
    expect(queryByRole('status')).toBeNull();
  });

  it('saves through SetWebviewGPUVsyncDisabled, reconciles from the answer, and shows the restart notice until the value is back', async () => {
    setBindingMock('IsWSL', vi.fn(async () => true));
    setBindingMock('GetWebviewGPUVsyncDisabled', vi.fn(async () => vsync(false)));
    const setMock = setBindingMock('SetWebviewGPUVsyncDisabled', vi.fn(async (value: unknown) => vsync(value as boolean, false)));
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

  // The page compares against the value this session started with, which
  // the backend reports, never the value it found on mount.
  it('keeps the restart notice when Settings is closed and reopened before a restart', async () => {
    setBindingMock('IsWSL', vi.fn(async () => true));
    let saved = false;
    setBindingMock('GetWebviewGPUVsyncDisabled', vi.fn(async () => vsync(saved, false)));
    setBindingMock('SetWebviewGPUVsyncDisabled', vi.fn(async (value: unknown) => {
      saved = value as boolean;
      return vsync(saved, false);
    }));
    const first = render(WebviewGPUVsyncField);
    await waitFor(() => expect(first.container.querySelector(FIELD)).not.toBeNull());
    await fireEvent.click(first.getByRole('switch'));
    await waitFor(() => expect(first.getByRole('status').textContent).toMatch(/next time you open/i));
    first.unmount();

    const reopened = render(WebviewGPUVsyncField);
    await waitFor(() => expect(reopened.getByRole('switch').getAttribute('aria-checked')).toBe('true'));
    expect(reopened.getByRole('status').textContent).toMatch(/next time you open/i);
  });

  it('keeps the saved value and toasts when the write fails', async () => {
    setBindingMock('IsWSL', vi.fn(async () => true));
    setBindingMock('GetWebviewGPUVsyncDisabled', vi.fn(async () => vsync(false)));
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
