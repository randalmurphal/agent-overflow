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
import { cleanup } from '@testing-library/svelte';
import { resetWailsMocks } from './mocks/wailsio-runtime';
import { resetBindingMocks, setBindingMock } from './mocks/bindings-app';
import { resetForTest as resetThreadStatusesForTest } from '../lib/stores/threadStatuses.svelte';
import { clearThreadItemCacheForTest } from '../lib/stores/threadItemCache';
import { clearThreadScrollSnapshotsForTest } from '../lib/utils/threadScrollSnapshots';
import { clearThreadVirtuaSizeCacheForTest } from '../lib/utils/threadVirtuaSizeCache';

// Chromium emits this when a ResizeObserver callback itself changes layout so
// notifications remain for the next frame — the scroll controller's sync-pin
// does exactly that by design, and the browser simply re-delivers next frame.
// It is a warning-grade engine notice (observer loop LIMIT errors are the
// real bug) whose ErrorEvent carries `.error === null`, so vitest's browser
// error-catcher never fails a test on it — but the catcher DOES re-log every
// occurrence via console.error, which buries a streaming test's output under
// dozens of identical lines. Drop exactly that message; everything else
// passes through untouched.
//
// Deliberately NOT a window 'error' listener: vitest counts user-registered
// error listeners and, when any exist, downgrades ALL unhandled window
// errors to console noise instead of failing the test — a listener here
// would weaken the safety net for the whole browser suite.
const RO_LOOP_NOTICE = 'ResizeObserver loop completed with undelivered notifications';
const originalConsoleError = console.error.bind(console);
console.error = (...args: unknown[]) => {
  const [first] = args;
  const message = first instanceof Error ? first.message : typeof first === 'string' ? first : '';
  if (message.includes(RO_LOOP_NOTICE)) return;
  originalConsoleError(...args);
};

beforeEach(() => {
  // uiRenderTrace is compiled in under MODE==='test' and flushes on a real
  // 500ms timer, which browser tests actually reach (unit tests finish before
  // it fires). Default the sink bindings to no-ops so any traced code path
  // doesn't spam "called without a mock" warnings; tests can override.
  setBindingMock('AppendUIRenderTraceBatch', async () => '');
  setBindingMock('BookmarkUIRenderTrace', async () => '');
});

afterEach(() => {
  // Unmounts components rendered through @testing-library. Tests that
  // mount() manually (the prevailing browser-suite pattern) keep owning
  // their own unmount in a local afterEach.
  cleanup();
  resetWailsMocks();
  resetBindingMocks();
  // Global active-turn registry backing getActiveTurn(); a leaked live
  // turn makes the next test's timeline believe a stream is in flight.
  resetThreadStatusesForTest();
  // Per-thread caches that survive switchThread by design: items snapshot
  // LRU, scroll position snapshots, and virtua's measured-size replay
  // cache. Stale entries let a later test skip its mocked load or replay
  // another test's row geometry.
  clearThreadItemCacheForTest();
  clearThreadScrollSnapshotsForTest();
  clearThreadVirtuaSizeCacheForTest();
});
