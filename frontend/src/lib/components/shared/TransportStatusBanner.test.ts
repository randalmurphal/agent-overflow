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

// Partial mock: only the two exports this component drives are replaced. A
// whole-module factory would have to re-declare every export, so any new one
// silently becomes undefined here.
vi.mock('../../stores/transportStatus.svelte', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../stores/transportStatus.svelte')>()),
  getTransportStatus: () => h.snapshot,
  retryTransport: () => h.retry(),
}));

import TransportStatusBanner from './TransportStatusBanner.svelte';
import {
  __resetBundleNoticeForTest,
  noteBundleReady,
  noteBundleTooOld,
} from '../../stores/bundleNotice.svelte';
import { hasPairedSession } from '../../transport/deviceSession';
import {
  __resetHomeEndpointForTest,
  setHomeEndpoint,
  storeBackendEndpoint,
  storedBackendEndpoint,
} from '../../transport/homeEndpoint';

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

  // A refused credential (backend restarted, tokens are per-launch) is
  // terminal: the wsClient has stopped retrying, so this banner is the
  // entire recovery story and must name the action that works.
  it('names the recovery action when the backend refuses the credential', async () => {
    // The client publishes this state with nextAttemptAt: null (nothing
    // is scheduled). Feed a stale timestamp anyway — no countdown may
    // leak out of this state, whatever the snapshot carries, because it
    // would promise a recovery that is not coming.
    h.snapshot = { status: 'unauthorized', nextAttemptAt: Date.now() + 5_000 };
    const { getByTestId } = render(TransportStatusBanner);
    await settleBootGrace();

    const banner = getByTestId('transport-status-banner');
    expect(banner.dataset.status).toBe('unauthorized');
    expect(banner.textContent).toContain('The backend restarted. Reopen the share link to reconnect.');
    expect(banner.textContent).not.toContain('Reconnecting');
    // Retry stays available — it un-latches the client for one
    // user-initiated attempt (wsClient.triggerReconnect).
    expect(getByTestId('transport-status-retry')).not.toBeNull();
  });

  // The other terminal state, and the one whose remedy is different in
  // kind: this page loads fine, the backend simply will not open a
  // socket for it until the device is paired. Telling this person to
  // reopen a share link would send them around the loop they are
  // already in.
  it('names the pairing action when the backend admits paired devices only', async () => {
    h.snapshot = { status: 'pairing-required', nextAttemptAt: Date.now() + 5_000 };
    const { getByTestId } = render(TransportStatusBanner);
    await settleBootGrace();

    const banner = getByTestId('transport-status-banner');
    expect(banner.dataset.status).toBe('pairing-required');
    expect(banner.textContent).toContain('Pair this device to use this backend.');
    expect(banner.textContent).not.toContain('share link');
    expect(banner.textContent).not.toContain('Reconnecting');
    expect(getByTestId('transport-status-retry')).not.toBeNull();
  });

  it('forces a reconnect when Retry is clicked', async () => {
    h.snapshot = { status: 'disconnected', nextAttemptAt: null };
    const { getByTestId } = render(TransportStatusBanner);
    await settleBootGrace();

    await fireEvent.click(getByTestId('transport-status-retry'));
    expect(h.retry).toHaveBeenCalledOnce();
  });

  // A page whose origin is not its backend's (the phone shell) cannot be
  // navigated to a new pairing link and cannot run a passkey ceremony
  // bound to the backend's domain. Its recovery is to forget home and
  // boot into "scan a code" again.
  it('offers Pair again, and no passkey, when the page is served from its own origin', async () => {
    setHomeEndpoint('https://desk.tail-scale.ts.net:7777');
    storeBackendEndpoint('', 'https://desk.tail-scale.ts.net:7777');
    try {
      h.snapshot = { status: 'unauthorized', nextAttemptAt: null };
      const { getByTestId, queryByTestId } = render(TransportStatusBanner);
      await settleBootGrace();

      expect(queryByTestId('transport-status-passkey')).toBeNull();
      const again = getByTestId('transport-status-pair-again');
      expect(storedBackendEndpoint('')).toBe('https://desk.tail-scale.ts.net:7777');

      await fireEvent.click(again);
      expect(storedBackendEndpoint('')).toBe('');
      expect(hasPairedSession()).toBe(false);
    } finally {
      __resetHomeEndpointForTest();
      localStorage.clear();
    }
  });

  it('offers no Pair again on a page that is its backend\'s own origin', async () => {
    h.snapshot = { status: 'unauthorized', nextAttemptAt: null };
    const { queryByTestId } = render(TransportStatusBanner);
    await settleBootGrace();
    expect(queryByTestId('transport-status-pair-again')).toBeNull();
  });
});

// The phone shell's update channel says at most two things, and this is
// the strip they are said in. Everything else bundle sync does — picking
// a backend, downloading, verifying, staging, rolling back — is silent
// (stores/bundleNotice.svelte.ts).
describe('<TransportStatusBanner> and the bundle notice', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    h.snapshot = { status: 'connected', nextAttemptAt: null };
    __resetBundleNoticeForTest();
  });

  afterEach(() => {
    __resetBundleNoticeForTest();
    vi.useRealTimers();
  });

  it('says nothing on a connected client with no bundle news', async () => {
    const { queryByTestId } = render(TransportStatusBanner);
    await tick();
    expect(queryByTestId('transport-status-banner')).toBeNull();
  });

  it('shows a staged bundle as a restart, with no button to press', async () => {
    noteBundleReady();
    const { getByTestId, queryByTestId } = render(TransportStatusBanner);
    await tick();

    const banner = getByTestId('transport-status-banner');
    expect(banner.textContent).toContain(
      'A newer Agent Overflow is ready. It loads the next time the app starts.',
    );
    // Nothing to retry: the transport is fine, and the swap happens on
    // the next cold start whether or not anybody acknowledges it.
    expect(queryByTestId('transport-status-retry')).toBeNull();
  });

  it('names the machine a phone is too old for', async () => {
    noteBundleTooOld('desk');
    const { getByTestId } = render(TransportStatusBanner);
    await tick();
    expect(getByTestId('transport-status-banner').textContent).toContain(
      "This phone's app is too old for desk. Update Agent Overflow on the phone.",
    );
  });

  it('lets a staged bundle outrank a floor this phone is under', async () => {
    // A restart is an action; "update from the app store" is not one
    // this app can perform, and replacing the actionable sentence with
    // the unactionable one would be the wrong trade.
    noteBundleReady();
    noteBundleTooOld('laptop');
    const { getByTestId } = render(TransportStatusBanner);
    await tick();
    expect(getByTestId('transport-status-banner').textContent).toContain(
      'A newer Agent Overflow is ready.',
    );
  });
});
