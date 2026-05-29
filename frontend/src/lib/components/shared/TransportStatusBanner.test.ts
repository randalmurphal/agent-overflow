import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import type { TransportStatusSnapshot } from '../../transport/wsClient';

// The component reads the transport snapshot through the store; drive it
// from a hoisted holder so each test can pick the connection state before
// rendering. (vi.hoisted avoids the TDZ footgun of referencing a plain
// top-level `let` from inside the hoisted vi.mock factory.)
const h = vi.hoisted(() => ({
  snapshot: { status: 'connected', nextAttemptAt: null } as TransportStatusSnapshot,
  retry: vi.fn(),
}));

vi.mock('../../stores/transportStatus.svelte', () => ({
  getTransportStatus: () => h.snapshot,
  retryTransport: () => h.retry(),
  resetTransportStatusForTest: () => {},
}));

import TransportStatusBanner from './TransportStatusBanner.svelte';

// The banner is gated behind a 1s boot grace (so a momentary pre-handshake
// disconnect doesn't flash on mount). Trip it with fake timers.
async function settleBootGrace(): Promise<void> {
  await tick();
  await vi.advanceTimersByTimeAsync(1100);
  await tick();
}

describe('<TransportStatusBanner>', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    h.snapshot = { status: 'connected', nextAttemptAt: null };
    h.retry.mockClear();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('reserves no layout height on the happy path (connected)', async () => {
    h.snapshot = { status: 'connected', nextAttemptAt: null };
    const { container, queryByTestId } = render(TransportStatusBanner);
    await tick();

    // The regression this overlay change fixed: there must be no
    // always-present wrapper holding vertical space above the chat header.
    expect(queryByTestId('transport-status-banner')).toBeNull();
    expect(container.querySelector('[data-testid="transport-status-slot"]')).toBeNull();
    expect(container.querySelector('.min-h-7')).toBeNull();
  });

  it('renders as an absolute overlay (no layout shift) when disconnected', async () => {
    h.snapshot = { status: 'disconnected', nextAttemptAt: null };
    const { getByTestId } = render(TransportStatusBanner);
    await settleBootGrace();

    const banner = getByTestId('transport-status-banner');
    // Overlay, not a flow element: absolute + pinned to the top, and NOT a
    // height-reserving slot. This is what keeps the panes from shifting
    // down when the transport drops.
    expect(banner.className).toContain('absolute');
    expect(banner.className).toContain('top-0');
    expect(banner.className).toContain('inset-x-0');
    expect(banner.className).not.toContain('min-h-7');
    expect(banner.textContent).toContain('Disconnected from the agent backend.');
  });

  it('forces a reconnect when Retry is clicked', async () => {
    h.snapshot = { status: 'disconnected', nextAttemptAt: null };
    const { getByTestId } = render(TransportStatusBanner);
    await settleBootGrace();

    await fireEvent.click(getByTestId('transport-status-retry'));
    expect(h.retry).toHaveBeenCalledOnce();
  });
});
