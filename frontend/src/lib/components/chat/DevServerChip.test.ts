import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import DevServerChip from './DevServerChip.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetRunMode, setRunMode } from '../../../test/runMode';

describe('<DevServerChip>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetRunMode();
  });

  it('labels the chip with host:port and keeps the full URL accessible', () => {
    const { getByTestId } = render(DevServerChip, { props: { url: 'http://localhost:5173/' } });

    const chip = getByTestId('dev-server-chip');
    expect(chip.textContent?.trim()).toBe('localhost:5173');
    expect(chip.getAttribute('aria-label')).toBe('Open http://localhost:5173/ in browser');
    expect(chip.dataset.url).toBe('http://localhost:5173/');
  });

  it('routes the click through the OpenExternalURL binding', async () => {
    const open = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));

    const { getByTestId } = render(DevServerChip, { props: { url: 'http://127.0.0.1:3000/' } });
    await fireEvent.click(getByTestId('dev-server-chip'));

    expect(open).toHaveBeenCalledWith('http://127.0.0.1:3000/');
  });

  it('falls back to window.open for a remote client session', async () => {
    setRunMode('client');
    const open = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));
    const windowOpen = vi.fn();
    const originalOpen = window.open;
    window.open = windowOpen as typeof window.open;

    try {
      const { getByTestId } = render(DevServerChip, { props: { url: 'http://localhost:8080/' } });
      await fireEvent.click(getByTestId('dev-server-chip'));

      expect(open).not.toHaveBeenCalled();
      expect(windowOpen).toHaveBeenCalledWith('http://localhost:8080/', '_blank', 'noopener,noreferrer');
    } finally {
      window.open = originalOpen;
      resetRunMode();
    }
  });
});
