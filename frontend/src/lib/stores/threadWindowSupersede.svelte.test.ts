// stores/threadWindowSupersede.svelte.test.ts
//
// A snapshot read describes the window it started from. When a jump, a
// page load or a cut replaces that window before the read lands, the
// read's page is not applied: its cursors would describe rows the pane no
// longer holds, hiding the rows it does (`itemsWithinLoadedWindow`). The
// backend refresh reruns against the window that replaced it; the switch
// sync hands over to that refresh; a page load whose edge a cut moved is
// refused. `threadTimelineWindow.observeWindow` is the one rule.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { createThreadPane } from './thread.svelte';
import type { Item } from '../types/models';
import type { PagedItems } from '../../../bindings/agent-overflow/internal/store/models';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { makeItem, makeThread } from '../../test/helpers/chat';
import { flushMicrotasks, installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import {
  ACTIVE_TIMELINE_WINDOW_MAX_ITEMS,
  ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
} from './threadPaneShared';

const THREAD = 't';
const COUNT = 1200;
type Cursor = { turnIndex: number; itemIndex: number; itemId?: string };

/** One row per turn, served the way the backend pages them. */
function history(count = COUNT) {
  const rows = Array.from({ length: count }, (_, i) =>
    makeItem({ id: `row-${i}`, threadId: THREAD, turnIndex: i, itemIndex: 0, summary: `row ${i}` }));
  const cursor = (item: Item) => ({ turnIndex: item.turnIndex, itemIndex: item.itemIndex, itemId: item.id });
  const pageOf = (from: number, to: number): PagedItems => {
    const items = rows.slice(Math.max(0, from), Math.min(count, to));
    return {
      items,
      oldestCursor: cursor(items[0]),
      newestCursor: cursor(items[items.length - 1]),
      oldestTurnIndex: items[0].turnIndex,
      newestTurnIndex: items[items.length - 1].turnIndex,
      hasMore: from > 0,
      hasMoreOlder: from > 0,
      hasMoreNewer: to < count,
      runs: [],
    } as unknown as PagedItems;
  };
  // Rows strictly before the cursor; every row here sits at item index 0.
  const before = (at: Cursor) => rows.filter(row =>
    row.turnIndex < at.turnIndex || (row.turnIndex === at.turnIndex && at.itemIndex > 0)).length;
  return {
    rows,
    page: pageOf,
    slice: (anchor: string, budget: number) => {
      if (!anchor) return pageOf(count - budget, count);
      const from = Math.max(0, rows.findIndex(row => row.id === anchor) - budget / 2);
      return pageOf(from, from + budget);
    },
    before: (at: Cursor, limit: number) => pageOf(before(at) - limit, before(at)),
    after: (at: Cursor, limit: number) => pageOf(before(at) + 1, before(at) + 1 + limit),
  };
}

/** Serve the thread through the paging bindings, and return the call spies. */
function serve(backend: ReturnType<typeof history>) {
  return {
    slice: setBindingMock('ListThreadSliceAround',
      async (_threadId: string, anchor: string, budget: number) => backend.slice(anchor, budget)),
    before: setBindingMock('ListItemsBeforeCursor',
      async (_threadId: string, at: Cursor, limit: number) => backend.before(at, limit)),
    after: setBindingMock('ListItemsAfterCursor',
      async (_threadId: string, at: Cursor, limit: number) => backend.after(at, limit)),
    item: setBindingMock('GetThreadItem',
      async (_threadId: string, id: string) => backend.rows.find(row => row.id === id)),
  };
}

/** Hold the first live-state read so the refresh around it straddles what follows. */
function holdFirstLiveStateRead() {
  let release!: () => void;
  const held = new Promise<void>((resolve) => { release = resolve; });
  let calls = 0;
  const read = setBindingMock('GetThreadLiveState', async (threadId: string) => {
    calls += 1;
    if (calls === 1) await held;
    return { threadId, activeTurn: null, queueItems: [], interactive: { approvals: [], userInputs: [] }, todo: null };
  });
  return { read, release: () => release() };
}

const idRange = (from: number, to: number) => Array.from({ length: to - from }, (_, i) => `row-${from + i}`);

describe('a window replaced during a snapshot read', () => {
  beforeEach(installThreadPaneTestEnv);

  it('does not apply a refresh page read before a jump, and reruns against the jumped window', async () => {
    const backend = history();
    const calls = serve(backend);
    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: THREAD }));
    expect(pane.items.map(item => item.id)).toEqual(idRange(1000, 1200));

    const live = holdFirstLiveStateRead();
    let answered = false;
    const refreshing = pane.refreshFromBackend(true).then(() => { answered = true; });
    await vi.waitFor(() => expect(live.read).toHaveBeenCalledOnce());

    expect(await pane.loadUntilItem('row-0')).toBe('loaded');
    expect(pane.hasMoreNewer).toBe(true);
    calls.before.mockClear();
    live.release();
    await flushMicrotasks();

    // The tail page was dropped: the jumped rows are still the window, and
    // the waiter is owed the rerun.
    expect(pane.items.map(item => item.id)).toEqual(idRange(0, 200));
    expect(pane.newestLoadedTurnIndex).toBe(199);
    expect(answered).toBe(false);

    await refreshing;
    expect(live.read).toHaveBeenCalledTimes(2);
    // The rerun read the jumped window, bounded by its ceiling.
    expect(calls.before.mock.calls[0][1]).toEqual({ turnIndex: 199, itemIndex: 1, itemId: '' });
    expect(pane.items.map(item => item.id)).toEqual(idRange(0, 200));
    expect(pane.hasMoreNewer).toBe(true);
    expect(pane.hasMoreHistory).toBe(false);

    // Every later refresh keeps the reader's position.
    await pane.refreshFromBackend(true);
    expect(pane.items.map(item => item.id)).toEqual(idRange(0, 200));
    expect(pane.hasMoreNewer).toBe(true);
    expect(pane.hasMoreHistory).toBe(false);
  });

  it('keeps rows a load-newer appended while a refresh was reading', async () => {
    const backend = history();
    serve(backend);
    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: THREAD }));
    expect(await pane.loadUntilItem('row-500')).toBe('loaded');
    expect(pane.newestLoadedTurnIndex).toBe(599);

    const live = holdFirstLiveStateRead();
    const refreshing = pane.refreshFromBackend();
    await vi.waitFor(() => expect(live.read).toHaveBeenCalledOnce());
    expect((await pane.loadNewer()).status).toBe('loaded');
    expect(pane.newestLoadedTurnIndex).toBe(799);
    live.release();
    await refreshing;

    expect(pane.newestLoadedTurnIndex).toBe(799);
    expect(pane.hasMoreNewer).toBe(true);
    expect(pane.items.map(item => item.id)).toEqual(expect.arrayContaining(idRange(600, 800)));
  });

  it('refuses an older page whose floor a streaming cut moved while it loaded', async () => {
    const max = ACTIVE_TIMELINE_WINDOW_MAX_ITEMS;
    const target = ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS;
    const backend = history(100 + max + 1);
    serve(backend);
    // Every row but the live one about to arrive.
    setBindingMock('ListThreadSliceAround', async () => ({ ...backend.page(100, 100 + max), hasMoreNewer: false }));
    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: THREAD }));
    expect(pane.oldestLoadedTurnIndex).toBe(100);

    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    setBindingMock('ListItemsBeforeCursor', async (_threadId: string, at: Cursor, limit: number) => {
      await held;
      return backend.before(at, limit);
    });
    const loading = pane.loadOlder();
    // The live tail passes the cap and the cut drops the head the load extends.
    pane.upsertItem(backend.rows[100 + max]);
    const floor = 100 + max + 1 - target;
    expect(pane.oldestLoadedTurnIndex).toBe(floor);
    release();

    expect((await loading).status).toBe('stale');
    expect(pane.oldestLoadedTurnIndex).toBe(floor);
    expect(pane.hasMoreHistory).toBe(true);
    expect(pane.items[0].id).toBe(`row-${floor}`);
    expect(pane.items).toHaveLength(target);
  });

  it('hands a switch sync answer read before a jump to a refresh of the jumped window', async () => {
    const backend = history();
    const calls = serve(backend);
    let answer!: () => void;
    const sync = setBindingMock('SyncThreadWindow', () => new Promise((resolve) => {
      answer = () => resolve({ status: 'stale', epoch: 1, rev: 1, generation: 'test-generation', page: backend.slice('', 200) });
    }));
    const pane = createThreadPane();
    const opening = pane.switchThread(makeThread({ id: THREAD }));
    await vi.waitFor(() => expect(sync).toHaveBeenCalledOnce());

    expect(await pane.loadUntilItem('row-0')).toBe('loaded');
    calls.before.mockClear();
    answer();
    await opening;

    expect(pane.items.map(item => item.id)).toEqual(idRange(0, 200));
    expect(pane.newestLoadedTurnIndex).toBe(199);
    await vi.waitFor(() => expect(calls.before).toHaveBeenCalled());
    expect(calls.before.mock.calls[0][1]).toEqual({ turnIndex: 199, itemIndex: 1, itemId: '' });
    await pane.refreshFromBackend(true);
    expect(pane.items.map(item => item.id)).toEqual(idRange(0, 200));
    expect(pane.hasMoreNewer).toBe(true);
    expect(pane.hasMoreHistory).toBe(false);
  });

  it('lets an older page in flight land before a refresh replaces the window it extends', async () => {
    // The pane holds rows 1000-1199 as its tail; the backend has 1000 newer
    // rows a transport gap kept from it, more than a refresh reads back.
    const backend = history(2200);
    serve(backend);
    setBindingMock('ListThreadSliceAround', async () => ({ ...backend.page(1000, 1200), hasMoreNewer: false }));
    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: THREAD }));
    serve(backend);

    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    setBindingMock('ListItemsBeforeCursor', async (_threadId: string, at: Cursor, limit: number) => {
      if (at.turnIndex === 1000) await held;
      return backend.before(at, limit);
    });
    const loading = pane.loadOlder();
    const refreshing = pane.refreshFromBackend(true);
    await flushMicrotasks();
    // The refresh did not install the tail over the floor the load extends.
    expect(pane.oldestLoadedTurnIndex).toBe(1000);
    expect(pane.newestLoadedTurnIndex).toBe(1199);

    release();
    expect((await loading).status).toBe('loaded');
    await refreshing;

    // One contiguous window reaching the backend's tail.
    const first = pane.items[0].turnIndex;
    expect(pane.items.map(item => item.id)).toEqual(idRange(first, 2200));
    expect(pane.oldestLoadedTurnIndex).toBe(first);
    expect(pane.newestLoadedTurnIndex).toBe(2199);
    expect(pane.hasMoreHistory).toBe(true);
  });
});
