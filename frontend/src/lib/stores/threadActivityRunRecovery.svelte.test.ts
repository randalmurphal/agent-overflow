import { expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { activityRunRow, activityRunProse, activityRunStub } from '../../test/helpers/activityRuns';
import type { Item } from '../types/models';
import type { ActivityRunStub } from '../../../bindings/agent-overflow/internal/store/models';

it('recovers a stale run through an authoritative pane refresh even when its anchor is already loaded', async () => {
  const { installThreadPaneTestEnv } = await import('../../test/helpers/threadPane');
  const { createThreadPane } = await import('./thread.svelte');
  const { makeThread } = await import('../../test/helpers/chat');
  installThreadPaneTestEnv();
  vi.useFakeTimers();
  const pane = createThreadPane();
  try {
    let refreshed = false;
    const page = vi.fn(async () => ({
      items: [activityRunRow('b', 2, { summary: refreshed ? 'fresh' : 'cached' }), activityRunRow('c', 3), activityRunRow('d', 4), activityRunProse('end', 6)],
      oldestTurnIndex: 0, newestTurnIndex: 0, hasMoreOlder: false, hasMoreNewer: false, runs: [activityRunStub()],
      oldestCursor: { turnIndex: 0, itemIndex: 1, itemId: 'a' },
      newestCursor: { turnIndex: 0, itemIndex: 6, itemId: 'end' },
    }));
    setBindingMock('ListThreadSliceAround', page);
    await pane.switchThread(makeThread({ id: 't' }));
    expect(pane.getItemById('b')?.summary).toBe('cached');
    refreshed = true;
    const members = vi.fn(async () => { throw Object.assign(new Error('stale'), { code: 'activity_run_stale' }); });
    setBindingMock('ListActivityRunMembers', members);
    pane.activityRuns.markRunDirty('a');
    await vi.advanceTimersByTimeAsync(5000);
    expect(page).toHaveBeenCalledTimes(2);
    expect(pane.getItemById('b')?.summary).toBe('fresh');
    expect(members).toHaveBeenCalledOnce();
    expect(pane.activityRuns.heldRunFold()).not.toBeNull();
  } finally {
    pane.clear();
    vi.useRealTimers();
  }
});

it.each(['recovery', 'cache'] as const)('retains explicitly loaded activity history during %s validation', async (mode) => {
  const { installThreadPaneTestEnv } = await import('../../test/helpers/threadPane');
  const { createThreadPane } = await import('./thread.svelte');
  const { makeThread } = await import('../../test/helpers/chat');
  installThreadPaneTestEnv();
  const pane = createThreadPane();
  const all = Array.from({ length: 45 }, (_, i) => activityRunRow(`tool-${i}`, i + 1));
  const describeRun = (first: number) => activityRunStub({ firstItemId: all[0].id, lastItemId: all[44].id,
    firstItemIndex: 1, lastItemIndex: 45, memberCount: 45,
    loadedFirstItemId: all[first].id, loadedLastItemId: all[44].id,
    unshippedBefore: first, unshippedAfter: 0 });
  const page = (first: number) => ({ items: all.slice(first), runs: [describeRun(first)],
    oldestCursor: { turnIndex: 0, itemIndex: 1, itemId: all[0].id },
    newestCursor: { turnIndex: 0, itemIndex: 45, itemId: all[44].id },
    oldestTurnIndex: 0, newestTurnIndex: 0, hasMoreOlder: false, hasMoreNewer: false });
  try {
    setBindingMock('ListThreadSliceAround', async () => page(0));
    await pane.switchThread(makeThread({ id: 't' }));
    if (mode === 'cache') await pane.switchThread(makeThread({ id: 'other' }));
    setBindingMock('ListThreadSliceAround', async () => page(15));
    setBindingMock('ListItemsBeforeCursor', async () => ({ ...page(0), items: all.map(item => ({ ...item, summary: 'revalidated' })) }));
    setBindingMock('ListActivityRunMembers', async () => ({ items: [], stub: describeRun(0) }));
    if (mode === 'cache') await pane.switchThread(makeThread({ id: 't' }));
    else await pane.refreshFromBackend(true);
    expect(pane.items.map(item => item.id)).toEqual(all.map(item => item.id));
    expect(pane.getItemById(all[0].id)?.summary).toBe('revalidated');
    expect(pane.activityRuns.snapshotStubs()?.[0]).toMatchObject({ unshippedBefore: 0, loadedFirstItemId: all[0].id });
    if (mode === 'cache') {
      const { threadItemCache } = await import('./threadItemCache');
      await pane.switchThread(makeThread({ id: 'other' }));
      expect(threadItemCache.get('t')?.historyStamp).toBeNull();
      setBindingMock('ListThreadSliceAround', async () => page(0));
      await pane.switchThread(makeThread({ id: 't' }));
      await pane.switchThread(makeThread({ id: 'other' }));
      expect(threadItemCache.get('t')?.historyStamp?.attested).toBe(true);
    }
  } finally { pane.clear(); }
});

// The thread holds p0 and the run a..d at 1..4. A page read before the live
// member e ships b alone of the run, and lands after the pane admitted e.
it.each(['sync', 'gap refresh'] as const)('refuses a live member the %s page leaves past a gap in its run', async (path) => {
  const { installThreadPaneTestEnv } = await import('../../test/helpers/threadPane');
  const { createThreadPane } = await import('./thread.svelte');
  const { makeThread } = await import('../../test/helpers/chat');
  installThreadPaneTestEnv();
  vi.useFakeTimers();
  const pane = createThreadPane();
  try {
    const p0 = activityRunProse('p0', 0);
    const e = activityRunRow('e', 5);
    const described = (lastItemId: string, lastItemIndex: number, unshippedAfter: number) => activityRunStub({
      lastItemId, lastItemIndex, memberCount: 2 + unshippedAfter,
      loadedFirstItemId: 'b', loadedLastItemId: 'b', unshippedBefore: 1, unshippedAfter,
    });
    const window = (items: Item[], runs: ActivityRunStub[]) => ({
      items, runs, oldestTurnIndex: 0, newestTurnIndex: 0, hasMoreOlder: false, hasMoreNewer: false,
      oldestCursor: { turnIndex: 0, itemIndex: 0, itemId: 'p0' },
      newestCursor: { turnIndex: 0, itemIndex: 4, itemId: 'd' },
    });
    const members = vi.fn(async () => ({ items: [], stub: described('e', 5, 3) }));
    setBindingMock('ListActivityRunMembers', members);
    if (path === 'gap refresh') {
      setBindingMock('ListThreadSliceAround', async () => window([p0], []));
      await pane.switchThread(makeThread({ id: 't' }));
    }
    let answer!: () => void;
    const slice = vi.fn(() => new Promise(resolve => {
      answer = () => resolve(window([p0, activityRunRow('b', 2)], [described('d', 4, 2)]));
    }));
    setBindingMock('ListThreadSliceAround', slice);
    const read = path === 'sync' ? pane.switchThread(makeThread({ id: 't' })) : pane.refreshFromBackend(true);
    await vi.waitFor(() => expect(slice).toHaveBeenCalledOnce());
    pane.applyProviderItemUpserts([e]);
    expect(pane.items.at(-1)).toBe(e);
    answer();
    await read;

    expect(pane.items.map(item => item.id)).toEqual(['p0', 'b']);
    await vi.advanceTimersByTimeAsync(2000);
    expect(members).toHaveBeenCalledOnce();
    expect(members.mock.calls[0]).toMatchObject([
      't', { limit: 0, loadedFirstItemId: 'b', loadedLastItemId: 'b' },
    ]);
    expect(slice).toHaveBeenCalledOnce();
    expect(pane.activityRuns.heldRunFold()?.count).toBe(4);
  } finally {
    pane.clear();
    vi.useRealTimers();
  }
});

// The pane holds a of the run a..d at 1..4. The row after the prose that
// ends the run arrives first, and is refused as a member.
it('reloads the window when a refused row turns out to follow its run\'s end', async () => {
  const { installThreadPaneTestEnv } = await import('../../test/helpers/threadPane');
  const { createThreadPane } = await import('./thread.svelte');
  const { makeThread } = await import('../../test/helpers/chat');
  installThreadPaneTestEnv();
  vi.useFakeTimers();
  const pane = createThreadPane();
  try {
    const p0 = activityRunProse('p0', 0);
    const a = activityRunRow('a', 1);
    const reply = activityRunProse('reply', 5);
    const next = activityRunRow('next', 6);
    const headOnly = activityRunStub({
      lastItemId: 'd', lastItemIndex: 4, memberCount: 4,
      loadedFirstItemId: 'a', loadedLastItemId: 'a', unshippedBefore: 0, unshippedAfter: 3,
    });
    let rows = [p0, a];
    const slice = vi.fn(async () => ({
      items: rows, runs: [headOnly], oldestTurnIndex: 0, newestTurnIndex: 0, hasMoreOlder: false, hasMoreNewer: false,
      oldestCursor: { turnIndex: 0, itemIndex: 0, itemId: 'p0' },
      newestCursor: { turnIndex: 0, itemIndex: rows.at(-1)!.itemIndex, itemId: rows.at(-1)!.id },
    }));
    setBindingMock('ListThreadSliceAround', slice);
    const members = vi.fn(async () => ({ items: [], stub: headOnly }));
    setBindingMock('ListActivityRunMembers', members);
    await pane.switchThread(makeThread({ id: 't' }));
    expect(pane.items.map(item => item.id)).toEqual(['p0', 'a']);

    rows = [p0, a, reply, next];
    pane.applyProviderItemUpserts([next]);
    pane.applyProviderItemUpserts([reply]);
    expect(pane.items.map(item => item.id)).toEqual(['p0', 'a', 'reply']);
    await vi.advanceTimersByTimeAsync(2000);

    expect(members.mock.calls[0]).toMatchObject(['t', { limit: 0, loadedFirstItemId: 'a', loadedLastItemId: 'a' }]);
    expect(slice).toHaveBeenCalledTimes(2);
    expect(pane.items.map(item => item.id)).toEqual(['p0', 'a', 'reply', 'next']);
    const reads = members.mock.calls.length;
    await vi.advanceTimersByTimeAsync(5000);
    expect(members).toHaveBeenCalledTimes(reads);
    expect(slice).toHaveBeenCalledTimes(2);
    expect(pane.activityRuns.heldRunFold()?.count).toBe(3);
  } finally {
    pane.clear();
    vi.useRealTimers();
  }
});
