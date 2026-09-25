// A backend's failed boot phases in the connection strip. Its own
// `.svelte.ts` file because the cases change the snapshot while the
// component is mounted (see TransportStatusBanner.reboot.svelte.test.ts).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import type { TransportHello, TransportStatusSnapshot } from '../../transport/wsClient';
import type { BootFailure } from '../../transport/bootFailures';

let snapshot = $state<TransportStatusSnapshot>({ status: 'connected', nextAttemptAt: null });

vi.mock('../../stores/transportStatus.svelte', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../stores/transportStatus.svelte')>()),
  getTransportStatus: () => snapshot,
}));

import TransportStatusBanner from './TransportStatusBanner.svelte';
import ComputerTransportStatus from './ComputerTransportStatus.svelte';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { __setTransportHelloForTest } from '../../stores/transportStatus.svelte';
import { __resetBootFailureNoticeForTest } from '../../stores/bootFailureNotice.svelte';
import { __resetBundleNoticeForTest, noteBundleReady } from '../../stores/bundleNotice.svelte';

const crashedTurns: BootFailure = {
  phase: 'app.recover_crashed_turns',
  detail: 'Settling interrupted turns',
  error: 'triage: recover crashed turns: database is locked',
};
const worktrees: BootFailure = {
  phase: 'app.sweep_crashed_worktree_setups',
  detail: 'Settling worktree setups',
  error: 'worktree setup: sweep crashed setups: disk I/O error',
};
const bothText = 'Settling interrupted turns failed at startup: triage: recover crashed turns: database is locked. '
  + 'Settling worktree setups failed at startup: worktree setup: sweep crashed setups: disk I/O error. '
  + 'Retrying on next start.';

function hello(launchId: string, bootFailures: BootFailure[]): TransportHello {
  return {
    launchId, protocolVersion: 1, capabilities: [], backendId: 'backend-1', backendName: 'desk',
    serverTimeMs: 1, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0, bootFailures,
  };
}

describe('<TransportStatusBanner> and failed boot phases', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    snapshot = { status: 'connected', nextAttemptAt: null };
    __resetBootFailureNoticeForTest();
    __resetBundleNoticeForTest();
  });
  afterEach(() => {
    resetStagedBackends();
    __setTransportHelloForTest(null);
    __resetBootFailureNoticeForTest();
    __resetBundleNoticeForTest();
    vi.useRealTimers();
  });

  it('names each failed phase and its error in an error strip until dismissed', async () => {
    __setTransportHelloForTest(hello('launch-1', [crashedTurns, worktrees]));
    const view = render(TransportStatusBanner);
    await tick();
    const banner = view.getByTestId('transport-status-banner');
    expect(banner).toHaveTextContent(bothText);
    expect(banner.dataset.status).toBe('connected');
    expect(banner.className).toContain('text-error');
    // Nothing to retry now: the sweep runs again at the next start.
    expect(view.queryByTestId('transport-status-retry')).toBeNull();

    await fireEvent.click(view.getByTestId('transport-status-dismiss'));
    await vi.advanceTimersByTimeAsync(200);
    expect(view.queryByTestId('transport-status-banner')).toBeNull();

    // The same launch reconnecting says the same thing: still dismissed.
    __setTransportHelloForTest(hello('launch-1', [crashedTurns, worktrees]));
    await tick();
    expect(view.queryByTestId('transport-status-banner')).toBeNull();
  });

  it('shows a later launch that fails again, and clears when a launch does not', async () => {
    __setTransportHelloForTest(hello('launch-1', [crashedTurns]));
    const view = render(TransportStatusBanner);
    await tick();
    await fireEvent.click(view.getByTestId('transport-status-dismiss'));
    await vi.advanceTimersByTimeAsync(200);
    expect(view.queryByTestId('transport-status-banner')).toBeNull();

    __setTransportHelloForTest(hello('launch-2', [crashedTurns]));
    await tick();
    expect(view.getByTestId('transport-status-banner')).toHaveTextContent(
      'Settling interrupted turns failed at startup: triage: recover crashed turns: database is locked. Retrying on next start.',
    );

    __setTransportHelloForTest(hello('launch-3', []));
    await tick();
    await vi.advanceTimersByTimeAsync(200);
    expect(view.queryByTestId('transport-status-banner')).toBeNull();
  });

  it('yields to a connection problem or check and outranks a bundle notice', async () => {
    noteBundleReady();
    __setTransportHelloForTest(hello('launch-1', [worktrees]));
    const view = render(TransportStatusBanner);
    await tick();
    expect(view.getByTestId('transport-status-banner')).toHaveTextContent('Settling worktree setups failed at startup');

    snapshot = { status: 'reconnecting', nextAttemptAt: null };
    await tick();
    expect(view.getByTestId('transport-status-banner')).toHaveTextContent('Reconnecting…');
    expect(view.queryByTestId('transport-status-dismiss')).toBeNull();
    snapshot = { status: 'connected', nextAttemptAt: null, checkingConnection: true };
    await tick();
    expect(view.getByTestId('transport-status-banner')).toHaveTextContent('Checking connection…');
    expect(view.getByTestId('transport-status-banner').className).toContain('text-fg-muted');

    snapshot = { status: 'connected', nextAttemptAt: null };
    await tick();
    // Dismissing the failure leaves the bundle notice it outranked.
    await fireEvent.click(view.getByTestId('transport-status-dismiss'));
    await tick();
    const banner = view.getByTestId('transport-status-banner');
    expect(banner).toHaveTextContent('A newer Agent Overflow is ready.');
    expect(banner.className).toContain('text-fg-muted');
  });

  it('names the attached computer whose boot failed', async () => {
    const laptop = stageBackend({ hello: hello('launch-1', [crashedTurns]) });
    stageBackend({ id: 'desk', backendId: '11111111-2222-4333-8444-555555555555', name: 'Desk',
      wsUrl: 'ws://localhost:3000/ws/backend/desk', bootstrapUrl: '/bootstrap/desk.json' });
    const view = render(ComputerTransportStatus, { backend: 'laptop' });
    await tick();
    expect(view.getByTestId('transport-status-banner')).toHaveTextContent(
      'Laptop: Settling interrupted turns failed at startup: triage: recover crashed turns: database is locked.',
    );

    laptop.setHello(hello('launch-2', []));
    await tick();
    await vi.advanceTimersByTimeAsync(200);
    expect(view.queryByTestId('transport-status-banner')).toBeNull();
  });
});
