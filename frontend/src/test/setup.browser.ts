// Per-test resets for the real-Chromium `browser` vitest project — the
// minimal subset of setup.ts that browser tests actually need. Chromium
// implements everything setup.ts polyfills for happy-dom (ResizeObserver,
// element.animate, execCommand, matchMedia, localStorage), so none of
// those shims belong here; importing them would paper over the real
// engine the project exists to exercise.
//
// What browser tests DO share with the unit suite is module-level state:
// panes built via `test/helpers/chat.ts#buildPane` register binding mocks
// and write into caches that deliberately survive thread switches. Without
// these resets, one test's thread snapshot / item cache / active turn
// leaks into the next as "already warm" or "still streaming" state.
import { afterEach, beforeEach } from 'vitest';
import './helpers/firstDivergence';
import { cleanup } from '@testing-library/svelte';
import { resetWailsMocks } from './mocks/wailsio-runtime';
import { resetBindingMocks, setBindingMock } from './mocks/bindings-app';
import { resetAttachmentTransferMocks } from './mocks/attachmentTransfer';
import { setPageGrantsFromBootstrap } from '../lib/transport/scopes';
import { resetForTest as resetThreadStatusesForTest } from '../lib/stores/threadStatuses.svelte';
import { clearThreadItemCacheForTest } from '../lib/stores/threadItemCache';
import { clearThreadScrollSnapshotsForTest } from '../lib/utils/threadScrollSnapshots';
import { clearAllThreadSizePriorsForTest } from '../lib/utils/virtual/priors';
import { __resetSizePriorsStorageForTest } from '../lib/utils/virtual/priorsStorage';

function installBrowserTraceSinks(): void {
  // uiRenderTrace is compiled in under MODE==='test' and flushes on a real
  // 500ms timer, which browser tests actually reach (unit tests finish before
  // it fires). Default the sink bindings to no-ops so any traced code path
  // doesn't spam "called without a mock" warnings; tests can override.
  setBindingMock('AppendUIRenderTraceBatch', async () => '');
  setBindingMock('BookmarkUIRenderTrace', async () => '');
}

beforeEach(() => {
  installBrowserTraceSinks();
  // Same pin as setup.ts: with no bootstrap fetch nothing resolves the
  // page's grants, and scopes.ts THROWS in test mode on a pre-resolution
  // read outside a reactive context (the 2026-09-03 idle-trim tripwire).
  // Pin the embedded webview's answer; a test exercising a narrower
  // session sets its own.
  setPageGrantsFromBootstrap(false);
});

afterEach(() => {
  // Unmounts components rendered through @testing-library. Tests that
  // mount() manually (the prevailing browser-suite pattern) keep owning
  // their own unmount in a local afterEach.
  cleanup();
  resetWailsMocks();
  resetBindingMocks();
  resetAttachmentTransferMocks();
  // A trace flush timer can outlive the component that scheduled it. Keep
  // the browser project's documented no-op sink installed across the gap
  // between this teardown and the next test's beforeEach.
  installBrowserTraceSinks();
  // Global active-turn registry backing getActiveTurn(); a leaked live
  // turn makes the next test's timeline believe a stream is in flight.
  resetThreadStatusesForTest();
  // Per-thread caches that survive switchThread by design: items snapshot
  // LRU, scroll position snapshots, and the measured-size priors. Stale
  // entries let a later test skip its mocked load or replay another
  // test's row geometry.
  clearThreadItemCacheForTest();
  clearThreadScrollSnapshotsForTest();
  clearAllThreadSizePriorsForTest();
  // Real localStorage persists across tests in the same Chromium context
  // (unlike happy-dom's per-file in-memory shim), so also cancel any
  // pending debounced flush and wipe the size-priors keys it wrote.
  __resetSizePriorsStorageForTest();
});
