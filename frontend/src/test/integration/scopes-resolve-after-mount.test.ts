import { describe, expect, it, beforeAll, beforeEach, afterEach, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import App from '../../App.svelte';
import {
  flush,
  installAnimateShim,
  installAppDefaults,
  installComposerDefaults,
  installThreadViewDefaults,
  makeThread,
  resetAppState,
  seedSidebarProject,
} from './_helpers';
import { getBindingMock, setBindingMock } from '../mocks/bindings-app';
import { __resetScopesForTest, setPageGrantsFromBootstrap } from '../../lib/transport/scopes';
import { IDLE_TRIM_CHECK_MS, IDLE_TRIM_THRESHOLD_MS } from '../../lib/utils/idleMemoryTrim';
import { __setTransportStatusForTest } from '../../lib/stores/transportStatus.svelte';
import { getAllPanes, mountThreadInPane } from '../../lib/stores/panes.svelte';
import { getThreads } from '../../lib/stores/threads.svelte';
import { clearPairedSession } from '../../lib/transport/deviceSession';
import { pairViewOnly } from '../helpers/scopes';

beforeAll(installAnimateShim);

/**
 * The real page order: App mounts, THEN the WS client fetches the bootstrap
 * manifest that answers host presence. Everything armed from mount that
 * keys on `hasScope` must either read it reactively or wait for the answer;
 * a plain read at mount keeps the placeholder forever. The test suite
 * otherwise pre-resolves grants before every test (test/setup.ts), which is
 * how the idle memory trim shipped as a permanent no-op (2026-09-03).
 *
 * An unresolved non-reactive read throws in test mode (scopes.ts), so
 * mounting cleanly IS the first assertion.
 */
describe('App mounted before the bootstrap manifest resolves', () => {
  const trim = vi.fn(async () => 'requested');

  beforeEach(() => {
    const thread = makeThread({ id: 'thread-1' });
    installAppDefaults();
    setBindingMock('ListThreads', async () => [thread]);
    seedSidebarProject([thread]);
    installThreadViewDefaults();
    installComposerDefaults(thread.id);
    trim.mockClear();
    setBindingMock('RequestWebviewMemoryTrim', trim);
    __resetScopesForTest();
  });

  afterEach(() => {
    vi.useRealTimers();
    setPageGrantsFromBootstrap(false);
  });

  it('reads no scope at mount, and installs the idle trim once the manifest lands', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'Date'] });
    render(App);
    await flush();

    await vi.advanceTimersByTimeAsync(IDLE_TRIM_THRESHOLD_MS + 2 * IDLE_TRIM_CHECK_MS);
    expect(trim).not.toHaveBeenCalled();

    setPageGrantsFromBootstrap(false);
    await flush();
    await vi.advanceTimersByTimeAsync(IDLE_TRIM_THRESHOLD_MS + 2 * IDLE_TRIM_CHECK_MS);
    expect(trim).toHaveBeenCalledTimes(1);
  });
});

/**
 * A saved pane can restore before the manifest: the startup catalog read
 * falls back to the replica rows when the computer misses its deadline, and
 * the layout restore switches each pane against those rows. The switch's
 * grant-gated legs (SwitchThread, AutoResumeThread, the live-state snapshot)
 * must not decide on the placeholder, and must run once the manifest lands.
 */
describe('a saved pane restored before the bootstrap manifest resolves', () => {
  const operateCalls = ['SwitchThread', 'AutoResumeThread', 'GetThreadLiveState', 'ListPendingInteractiveRequests'];

  beforeEach(() => {
    resetAppState();
    const thread = makeThread({ id: 'thread-1' });
    const other = makeThread({ id: 'thread-2' });
    installAppDefaults();
    setBindingMock('ListThreads', async () => [thread, other]);
    seedSidebarProject([thread, other]);
    installThreadViewDefaults();
    installComposerDefaults(thread.id);
    installComposerDefaults(other.id);
    setBindingMock('GetUIState', async () => ({
      paneLayout: JSON.stringify({
        version: 3,
        panes: [
          { paneId: 'main', threadId: thread.id, widthPx: 800, kind: 'thread' },
          { paneId: 'review-main', kind: 'review', sourcePaneId: 'main', widthPx: 600 },
        ],
        focusedPaneId: 'main',
      }),
    }));
    __resetScopesForTest();
    // The socket opens after the manifest answers, so home is not connected
    // yet either; connection-gated stores wait for that edge.
    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
  });

  afterEach(() => {
    vi.restoreAllMocks();
    clearPairedSession();
    setPageGrantsFromBootstrap(false);
  });

  async function renderRestored() {
    const rendered = render(App);
    await waitFor(() => expect(rendered.getByTestId('pane-host')).toBeInTheDocument());
    await flush();
    return rendered;
  }

  it('restores the pane, and runs its operate legs once the manifest grants them', async () => {
    // A leg that read the placeholder throws in test mode; the switch
    // catches that and logs it, so a clean console is the read check.
    const consoleError = vi.spyOn(console, 'error');
    const rendered = await renderRestored();
    const paneIds = () => Array.from(
      rendered.container.querySelectorAll<HTMLElement>('[data-pane-kind="thread"]'),
    ).map((section) => section.dataset.paneId);
    expect(paneIds()).toEqual(['main']);
    expect(rendered.container.querySelectorAll('[data-pane-kind="review"]').length).toBe(1);
    for (const name of operateCalls) {
      expect(getBindingMock(name), `${name} must wait for the manifest`).not.toHaveBeenCalled();
    }

    setPageGrantsFromBootstrap(false);
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await waitFor(() => {
      for (const name of ['SwitchThread', 'AutoResumeThread', 'GetThreadLiveState']) {
        expect(getBindingMock(name), `${name} once granted`).toHaveBeenCalledWith('thread-1');
      }
    });
    expect(getBindingMock('SwitchThread')).toHaveBeenCalledTimes(1);
    expect(getBindingMock('AutoResumeThread')).toHaveBeenCalledTimes(1);
    expect(paneIds()).toEqual(['main']);
    expect(consoleError).not.toHaveBeenCalled();
  });

  it('runs only the latest switch once granted, however many came first', async () => {
    await renderRestored();
    const pane = getAllPanes().get('main');
    expect(pane?.threadId).toBe('thread-1');
    await mountThreadInPane(getThreads().find((thread) => thread.id === 'thread-2')!, pane);
    await flush();
    expect(getBindingMock('SwitchThread')).not.toHaveBeenCalled();

    setPageGrantsFromBootstrap(false);
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await waitFor(() => expect(getBindingMock('GetThreadLiveState')).toHaveBeenCalledWith('thread-2'));
    expect(getBindingMock('SwitchThread')?.mock.calls).toEqual([['thread-2']]);
    expect(getBindingMock('AutoResumeThread')?.mock.calls).toEqual([['thread-2']]);
    expect(getBindingMock('GetThreadLiveState')).not.toHaveBeenCalledWith('thread-1');
  });

  it('spends no refusal when the manifest grants a view-only session', async () => {
    await renderRestored();
    const windowReads = getBindingMock('ListThreadSliceAround')?.mock.calls.length ?? 0;

    await pairViewOnly();
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();
    await new Promise((settle) => setTimeout(settle, 300));
    for (const name of operateCalls) {
      expect(getBindingMock(name), `${name} is not granted`).not.toHaveBeenCalled();
    }
    // Nothing gated was skipped that this session may read, so no refresh.
    expect(getBindingMock('ListThreadSliceAround')?.mock.calls.length ?? 0).toBe(windowReads);
  });
});
