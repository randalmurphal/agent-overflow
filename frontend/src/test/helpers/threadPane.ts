// test/helpers/threadPane.ts
//
// The fixtures the ThreadPane store suites share. `thread.svelte.test.ts`
// was one 11k-line file whose helpers and binding-mock `beforeEach` were
// scoped to its single outer `describe`; splitting it by behavior area
// made those shared, so they live here rather than being copied into each
// sibling file.
//
// `installThreadPaneTestEnv` is the whole environment one pane suite needs:
// a deterministic viewport width, every cross-suite store reset, and the
// empty-thread defaults for the RPCs a pane touches on switch. Tests that
// need specific data override individual mocks AFTER it runs.
//
// MUST NOT grow pane-construction helpers. `buildPane` / `makeThread` /
// `makeItem` live in `chat.ts` and are shared with the component suites;
// this module is only the store suites' environment and their smoothing /
// design-fence fixtures.

import { getQueueForThread, resetForTest as resetSendQueueForTest } from '../../lib/stores/sendQueue.svelte';
import { resetForTest as resetThreadStatuses } from '../../lib/stores/threadStatuses.svelte';
import { resetLayoutMetricsForTest } from '../../lib/stores/layoutMetrics.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../lib/stores/paneLayout.svelte';
import { resetCompanionPanesForTest } from '../../lib/stores/companionPanes.svelte';
import { resetThreadTerminalStatesForTest } from '../../lib/components/terminal/terminalStore.svelte';
import type { SmoothingClock } from '../../lib/markdown/smoothing/PerItemSmoother';
import type { Item } from '../../lib/types/models';
import { resetBindingMocks, setBindingMock } from '../mocks/bindings-app';
import { makeThread } from './chat';

export function nextFrame(): Promise<void> {
  return new Promise((resolve) => {
    requestAnimationFrame(() => resolve());
  });
}

// Drain the microtask queue. Used where a test has to let an in-flight
// switch reach a deliberately-hanging binding mock: the cold-open item
// leg consults the durable replica before it issues its RPC, so the
// call no longer lands on the switch's own synchronous tick.
export function flushMicrotasks(): Promise<void> {
  return new Promise((resolve) => {
    setTimeout(resolve, 0);
  });
}

// FakeClock for smoothing reveal tests. Mirrors the same shape as
// PerItemSmoother.test.ts so per-tick assertions are deterministic.
export class FakeSmoothingClock implements SmoothingClock {
  private current = 0;
  private nextHandle = 1;
  private pending = new Map<number, () => void>();
  now(): number {
    return this.current;
  }
  schedule(cb: () => void): number {
    const h = this.nextHandle++;
    this.pending.set(h, cb);
    return h;
  }
  cancel(h: number): void {
    this.pending.delete(h);
  }
  tickFrame(ms: number): void {
    this.current += ms;
    const toFire = [...this.pending.values()];
    this.pending.clear();
    for (const cb of toFire) cb();
  }
  pendingCount(): number {
    return this.pending.size;
  }
}

// Helper: how much *new content* appeared at the end of `cur` that
// wasn't already at the end of `prev`. Computed by finding the longest
// suffix of `prev` that's also a prefix of `cur`, and returning the
// length of `cur` past that match. Used by smoothing tests to verify
// per-tick reveal granularity once the trim engages.
export function smoothingNewTailChars(prev: string, cur: string): number {
  const max = Math.min(prev.length, cur.length);
  for (let overlap = max; overlap > 0; overlap--) {
    if (prev.endsWith(cur.slice(0, overlap))) {
      return cur.length - overlap;
    }
  }
  return cur.length;
}

export function designFence(payload: unknown): string {
  return ['```aoflow-design', JSON.stringify(payload), '```'].join('\n');
}

export function seedThreadPaneLayout(paneId: string): void {
  setPaneLayoutItemsForTest([{ id: paneId, paneId, kind: 'thread', widthPx: 1 }]);
}

/**
 * The `beforeEach` every ThreadPane store suite installs. Resets the
 * cross-suite stores a pane reads through, then defaults every RPC a
 * `switchThread` touches to the empty thread so a suite only plumbs the
 * mocks it actually asserts on.
 */
export function installThreadPaneTestEnv(): void {
  Object.defineProperty(window, 'innerWidth', {
    configurable: true,
    writable: true,
    value: 1400,
  });
  resetBindingMocks();
  resetLayoutMetricsForTest();
  resetPaneLayoutForTest();
  resetCompanionPanesForTest();
  resetThreadTerminalStatesForTest();
  resetThreadStatuses();
  resetSendQueueForTest();
  setBindingMock('SwitchThread', async (threadId: unknown) =>
    makeThread({ id: typeof threadId === 'string' ? threadId : 'thread-1' }),
  );
  // switchThread loads the initial slice via ListThreadSliceAround
  // (works for both bottom-snapshot and saved-anchor cases — empty
  // anchor id resolves to the tail at the backend). Tests override
  // the mock to supply specific items; the default is an empty thread
  // so unrelated tests don't have to plumb it.
  setBindingMock('ListThreadSliceAround', async () => ({
    items: [] as Item[],
    oldestTurnIndex: -1,
    hasMore: false,
  }));
  // Legacy/broad tail loader. Active panes should use ListThreadSliceAround;
  // tests that intentionally touch this older RPC override the mock.
  setBindingMock('ListItemsBeforeCursor', async () => ({
    items: [] as Item[],
    oldestTurnIndex: -1,
    newestTurnIndex: -1,
    hasMore: false,
    hasMoreOlder: false,
    hasMoreNewer: false,
  }));
  setBindingMock('ListItemsAfterCursor', async () => ({
    items: [] as Item[],
    oldestTurnIndex: -1,
    newestTurnIndex: -1,
    hasMore: false,
    hasMoreOlder: false,
    hasMoreNewer: false,
  }));
  setBindingMock('ListPendingInteractiveRequests', async () => ({
    approvals: [],
    userInputs: [],
  }));
  setBindingMock('GetThreadLiveState', async (threadId: string) => ({
    threadId,
    activeTurn: null,
    queueItems: [...getQueueForThread(threadId)],
    interactive: { approvals: [], userInputs: [] },
    todo: null,
  }));
  setBindingMock('ListItems', async () => [] as Item[]);
  // switchThread calls ListRecentTurns as part of rehydration. Default
  // to an empty list so tests that don't care about turn rehydration
  // don't need to plumb the mock themselves.
  setBindingMock('ListRecentTurns', async () => []);
}
