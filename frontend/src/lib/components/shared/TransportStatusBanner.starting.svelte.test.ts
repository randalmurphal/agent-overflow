// A starting backend in the connection strip. Its own `.svelte.ts` file
// because the cases need the snapshot to change while the component is
// mounted (see TransportStatusBanner.reboot.svelte.test.ts).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import { tick } from 'svelte';
import type { TransportStatusSnapshot } from '../../transport/wsClient';

let snapshot = $state<TransportStatusSnapshot>({ status: 'connected', nextAttemptAt: null });

vi.mock('../../stores/transportStatus.svelte', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../stores/transportStatus.svelte')>()),
  getTransportStatus: () => snapshot,
}));

import TransportStatusBanner from './TransportStatusBanner.svelte';

const starting: TransportStatusSnapshot = {
  status: 'starting',
  nextAttemptAt: null,
  startup: {
    phase: 'store.migrate',
    detail: 'Applying migration 3 of 7 add_index',
    step: 3,
    steps: 7,
    elapsedMs: 72_000,
    updatingTo: '',
  },
};

describe('<TransportStatusBanner> while the backend is starting', () => {
  let reload: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.useFakeTimers();
    reload = vi.fn();
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: { ...window.location, reload },
    });
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it('leaves a first boot to the startup screen and the sidebar', async () => {
    snapshot = starting;
    const view = render(TransportStatusBanner);
    // Past the boot grace that reveals any other unconnected state.
    await vi.advanceTimersByTimeAsync(1100);
    await tick();
    expect(view.queryByTestId('transport-status-banner')).toBeNull();

    snapshot = { status: 'connected', nextAttemptAt: null };
    await tick();
    expect(view.queryByTestId('transport-status-banner')).toBeNull();
    expect(reload).not.toHaveBeenCalled();
  });

  it('shows a restart’s boot phase in a neutral strip with no Retry', async () => {
    snapshot = { status: 'connected', nextAttemptAt: null };
    const view = render(TransportStatusBanner);
    await tick();

    snapshot = starting;
    await tick();
    const banner = view.getByTestId('transport-status-banner');
    expect(banner.dataset.status).toBe('starting');
    expect(banner).toHaveTextContent('Applying migration 3 of 7 add_index (Step 3 of 7 · 1:12 elapsed)');
    expect(banner.className).toContain('text-fg-muted');
    expect(view.queryByTestId('transport-status-retry')).toBeNull();

    snapshot = { status: 'connected', nextAttemptAt: null };
    await tick();
    await vi.advanceTimersByTimeAsync(500);
    expect(view.queryByTestId('transport-status-banner')).toBeNull();
    expect(reload).not.toHaveBeenCalled();
  });
});
