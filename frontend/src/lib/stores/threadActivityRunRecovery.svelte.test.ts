import { expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { activityRunRow, activityRunProse, activityRunStub } from '../../test/helpers/activityRuns';

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
