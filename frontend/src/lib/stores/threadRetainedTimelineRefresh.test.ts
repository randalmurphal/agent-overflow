import { afterEach, beforeEach, expect, it } from 'vitest';
import { captureRetainedTimelineWindow, refreshRetainedTimelineWindow } from './threadRetainedTimelineRefresh';
import { buildPane, installTimelineScopeCapability, makeItem, makeThread } from '../../test/helpers/chat';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { PagedItems } from '../../../bindings/agent-overflow/internal/store/models';
import type { ThreadPane } from './thread.svelte';
import type { Item } from '../types/models';

const shape = { runWindowRows: 30, inlinePreviews: false, maxBytes: 160 << 10 };
const row = (i: number, overrides: Partial<Item> = {}) => makeItem({ id: `r${i}`, threadId: 't',
  turnIndex: 0, itemIndex: i, kind: 'assistant_text', ...overrides });
const cursor = (i: number) => ({ turnIndex: 0, itemIndex: i, itemId: `r${i}` });
const rows = (first: number, count: number) => Array.from({ length: count }, (_, i) => row(first + i));
function page(items: Item[], hasMoreOlder = false, hasMoreNewer = false) {
  return new PagedItems({ items, runs: [], oldestCursor: cursor(items[0]?.itemIndex ?? 0),
    newestCursor: cursor(items.at(-1)?.itemIndex ?? 0), hasMore: hasMoreOlder, hasMoreOlder, hasMoreNewer });
}
function held(first = 0, last = 99, followingTail = true) {
  return { oldest: cursor(first), newest: cursor(last), count: last - first + 1, runs: [], followingTail };
}
function refresh(fresh: PagedItems, retained = held(), isCurrent = () => true) {
  return refreshRetainedTimelineWindow({ threadId: 't', page: fresh, retained, shape, isCurrent });
}
let pane: ThreadPane;
beforeEach(async () => { installThreadPaneTestEnv(); pane = await buildPane(makeThread({ id: 't' })); });
afterEach(() => pane.clear());

it('keeps an already covered window unchanged without extra reads', async () => {
  const read = setBindingMock('ListItemsBeforeCursor', async () => { throw new Error('unexpected read'); });
  const fresh = page(rows(0, 100), true);
  expect(await refresh(fresh)).toBe(fresh);
  expect(read).not.toHaveBeenCalled();
});

it('rebuilds the retained extent across byte-limited pages using only fresh rows', async () => {
  const fresh = page(rows(90, 10), true);
  const read = setBindingMock('ListItemsBeforeCursor', async (_id, before: { itemIndex: number }, budget, requestedShape) => {
    expect(budget).toBe(200);
    expect(requestedShape).toEqual({ ...shape, selection: {} });
    const start = Math.max(0, before.itemIndex - 7);
    return page(rows(start, before.itemIndex - start).filter(item => item.itemIndex !== 50), start > 0);
  });
  const result = await refresh(fresh);
  expect(result.items.map(item => item.id)).toEqual(rows(0, 100).filter(item => item.itemIndex !== 50).map(item => item.id));
  expect(result.oldestCursor).toEqual(cursor(0));
  expect(result.hasMoreOlder).toBe(false);
  expect(read).toHaveBeenCalledTimes(13);
  expect(fresh.items).toHaveLength(10);
});

it('refreshes both retained edges when reading an older window', async () => {
  setBindingMock('ListItemsBeforeCursor', async () => page(rows(0, 40)));
  const after = setBindingMock('ListItemsAfterCursor', async () => page(rows(60, 40), true, true));
  const result = await refresh(page(rows(40, 20), true, true), held(0, 99, false));
  expect(result.items.map(item => item.id)).toEqual(rows(0, 100).map(item => item.id));
  expect(result.newestCursor).toEqual(cursor(99));
  expect(result.hasMoreNewer).toBe(true);
  expect(after).toHaveBeenCalledOnce();
});

it('bounds tail catch-up when the retained window is far behind', async () => {
  const read = setBindingMock('ListItemsBeforeCursor', async (_id, before: { itemIndex: number }) => page(rows(before.itemIndex - 50, 50), true));
  const result = await refresh(page(rows(990, 10), true));
  expect(result.items).toHaveLength(210);
  expect(read).toHaveBeenCalledTimes(4);
  expect(result.hasMoreOlder).toBe(true);
});

it('captures coordinates by value and counts only direct selected rows', () => {
  const window = { oldestLoadedCursor: cursor(0), newestLoadedCursor: cursor(99) };
  const selected = [row(0, { parentId: 'agent', kind: 'tool_call' }), row(1, { parentId: 'agent' }), row(2)];
  const captured = captureRetainedTimelineWindow(selected, window,
    { loadedItems: items => [...items], isLoadedMember: () => false, readerPinnedSpan: () => false }, true, { scopeRootId: 'agent', tools: true });
  window.oldestLoadedCursor.itemIndex = 50;
  expect(captured.oldest).toEqual(cursor(0));
  expect(captured.count).toBe(1);
});

it('carries a reader pin into the retained run decision', () => {
  const captured = captureRetainedTimelineWindow(rows(0, 3).map(item => ({ ...item, kind: 'tool_call' as const })),
    { oldestLoadedCursor: cursor(0), newestLoadedCursor: cursor(2) }, {
      loadedItems: items => [...items],
      isLoadedMember: () => true,
      readerPinnedSpan: (first, last) => first === 'r0' && last === 'r2',
    }, true);
  expect(captured.runs).toMatchObject([{ firstItemId: 'r0', lastItemId: 'r2', readerPinned: true }]);
});

it('discards cancelled reads without issuing another request', async () => {
  let current = true;
  const read = setBindingMock('ListItemsBeforeCursor', async () => { current = false; return page(rows(80, 10), true); });
  const fresh = page(rows(90, 10), true);
  expect(await refresh(fresh, held(), () => current)).toBe(fresh);
  expect(read).toHaveBeenCalledOnce();
});

for (const direction of ['older', 'newer'] as const) {
  const older = direction === 'older';
  const method = older ? 'ListItemsBeforeCursor' : 'ListItemsAfterCursor';
  it(`rejects non-advancing ${direction} reads`, async () => {
    const fresh = page(rows(40, 20), older, !older);
    const read = setBindingMock(method, async () => fresh);
    await expect(refresh(fresh, held(0, 99, false))).rejects.toThrow('did not advance');
    expect(read).toHaveBeenCalledOnce();
  });
  it(`accepts an exhausted ${direction} edge but rejects an empty page claiming more rows`, async () => {
    setBindingMock(method, async () => page([]));
    const fresh = page(rows(40, 20), older, !older);
    const result = await refresh(fresh, held(0, 99, false));
    expect(result.items).toEqual(fresh.items);
    expect(older ? result.hasMoreOlder : result.hasMoreNewer).toBe(false);
    setBindingMock(method, async () => page([], older, !older));
    await expect(refresh(fresh, held(0, 99, false))).rejects.toThrow('with more history remaining');
  });
}

it('merges once when the byte limit produces many small pages', async () => {
  let reads = 0;
  const all = rows(0, 120).map((item, i) => ({ ...item, get id() { reads += 1; return `r${i}`; } }));
  setBindingMock('ListItemsBeforeCursor', async (_id, before: { itemIndex: number }) => page([all[before.itemIndex - 1]], before.itemIndex > 1));
  const result = await refresh(page(all.slice(-10), true), held(0, 119));
  expect(result.items).toHaveLength(120);
  expect(reads).toBeLessThan(3000);
});

it('does not publish partial assembly when a page read fails', async () => {
  const fresh = page(rows(90, 10), true);
  setBindingMock('ListItemsBeforeCursor', async (_id, before: { itemIndex: number }) => {
    if (before.itemIndex === 90) return page(rows(80, 10), true);
    throw new Error('connection lost');
  });
  await expect(refresh(fresh)).rejects.toThrow('connection lost');
  expect(fresh.items).toHaveLength(10);
  expect(fresh.oldestCursor).toEqual(cursor(90));
});

it('counts only selected direct rows when bounding tail catch-up', async () => {
  installTimelineScopeCapability();
  const selected = (first: number, count: number) => rows(first, count).map(item => ({ ...item, parentId: 'agent', kind: 'tool_call' as const }));
  const read = setBindingMock('ListItemsBeforeCursor', async (_id, before: { itemIndex: number }, _budget, requestedShape) => {
    expect(requestedShape).toMatchObject({ selection: { scopeRootId: 'agent', tools: true } });
    return page(selected(before.itemIndex - 50, 50), true);
  });
  const fresh = page([...rows(1000, 300), ...selected(990, 10)], true);
  fresh.newestCursor = cursor(999);
  fresh.oldestCursor = cursor(990);
  const result = await refreshRetainedTimelineWindow({ threadId: 't', page: fresh, retained: held(), shape,
    isCurrent: () => true, selection: { scopeRootId: 'agent', tools: true } });
  expect(result.items.filter(item => item.parentId === 'agent')).toHaveLength(210);
  expect(read).toHaveBeenCalledTimes(4);
});
