import { describe, expect, it, beforeAll, beforeEach, afterEach, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import App from '../../App.svelte';
import {
  flush,
  installAnimateShim,
  installAppDefaults,
  installComposerDefaults,
  installThreadViewDefaults,
  makeThread,
  seedSidebarProject,
} from './_helpers';
import { setBindingMock } from '../mocks/bindings-app';
import { __resetScopesForTest, setPageGrantsFromBootstrap } from '../../lib/transport/scopes';
import { IDLE_TRIM_CHECK_MS, IDLE_TRIM_THRESHOLD_MS } from '../../lib/utils/idleMemoryTrim';

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
