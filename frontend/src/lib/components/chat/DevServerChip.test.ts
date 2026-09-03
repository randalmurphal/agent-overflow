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

  // With a `preview` the port is on another machine, so `localhost` here is
  // not it: the click mints a URL on that machine's port gateway and opens
  // THAT, and the button says where it is going.
  it('mints a preview and names the machine when the port is on another one', async () => {
    const external = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));
    const mint = setBindingMock(
      'MintPreviewURL',
      vi.fn(async () => 'https://laptop.tail.ts.net/preview/5173/app?t=1'),
    );

    const { getByTestId } = render(DevServerChip, {
      props: {
        url: 'http://localhost:5173/app',
        preview: {
          url: 'http://localhost:5173/app',
          threadId: 'thread-1',
          port: 5173,
          path: '/app',
          machine: 'Laptop',
        },
      },
    });

    const chip = getByTestId('dev-server-chip');
    expect(chip.getAttribute('aria-label')).toBe('Open localhost:5173 on Laptop');
    expect(chip.dataset.machine).toBe('Laptop');

    await fireEvent.click(chip);
    expect(mint).toHaveBeenCalledWith('thread-1', 5173, '/app');
    await vi.waitFor(() =>
      expect(external).toHaveBeenCalledWith('https://laptop.tail.ts.net/preview/5173/app?t=1'),
    );
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
