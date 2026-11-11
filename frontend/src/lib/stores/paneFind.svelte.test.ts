import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  PANE_FIND_DETAIL_REGION,
  PANE_FIND_LIST_REGION,
  activatePaneFindRegion,
  closePaneFind,
  declarePaneFindRegions,
  focusFirstPaneFindResult,
  getPaneFind,
  hasPaneFindProvider,
  isPaneFindOpen,
  isSteppingFind,
  openPaneFind,
  paneFindDetailKey,
  paneFindListKey,
  paneFindReads,
  paneFindRegionElement,
  paneFindRegionKey,
  paneFindTarget,
  paneOfFindKey,
  registerPaneFindProvider,
  resetPaneFindForTest,
  setPaneFindQuery,
  setPaneFindRegionElement,
  stepPaneFind,
  takePaneFindReturnFocus,
  type PaneFindProvider,
} from './paneFind.svelte';
import { addPaneLayoutItem, removePaneLayoutItem, resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';

function provider(overrides: Partial<PaneFindProvider> = {}): PaneFindProvider & {
  search: ReturnType<typeof vi.fn>;
  clear: ReturnType<typeof vi.fn>;
} {
  return {
    noun: 'thread',
    result: { count: 0, current: -1, pending: false },
    search: vi.fn(),
    clear: vi.fn(),
    next: vi.fn(),
    previous: vi.fn(),
    ...overrides,
  } as PaneFindProvider & { search: ReturnType<typeof vi.fn>; clear: ReturnType<typeof vi.fn> };
}

function layout(...paneIds: string[]): void {
  setPaneLayoutItemsForTest(paneIds.map((paneId) => ({ id: paneId, paneId, kind: 'thread', widthPx: 600 })));
}

beforeEach(() => {
  resetPaneFindForTest();
  layout('a', 'b');
});

afterEach(() => {
  resetPaneFindForTest();
  resetPaneLayoutForTest();
});

describe('pane find registry', () => {
  it('opens only a pane with a provider', () => {
    expect(openPaneFind('a')).toBe(false);
    expect(getPaneFind('a')).toBeNull();
    const release = registerPaneFindProvider('a', provider());
    expect(hasPaneFindProvider('a')).toBe(true);
    expect(openPaneFind('a')).toBe(true);
    expect(getPaneFind('a')?.open).toBe(true);
    release();
    expect(hasPaneFindProvider('a')).toBe(false);
  });

  it('runs the query through the provider and keeps it across a close and reopen', () => {
    const p = provider();
    registerPaneFindProvider('a', p);
    openPaneFind('a');
    setPaneFindQuery('a', 'needle');
    expect(p.search).toHaveBeenLastCalledWith('needle');

    closePaneFind('a');
    expect(getPaneFind('a')?.open).toBe(false);
    expect(getPaneFind('a')?.query).toBe('needle');
    expect(p.clear).toHaveBeenCalledTimes(1);

    p.search.mockClear();
    openPaneFind('a');
    expect(getPaneFind('a')?.query).toBe('needle');
    expect(p.search).toHaveBeenCalledWith('needle');
  });

  it('asks the bar to select its input on every open, including one while open', () => {
    registerPaneFindProvider('a', provider());
    openPaneFind('a');
    const first = getPaneFind('a')!.focusRequest;
    openPaneFind('a');
    expect(getPaneFind('a')!.focusRequest).toBeGreaterThan(first);
  });

  it('a reopen while open does not search again or lose the focus to return to', () => {
    const p = provider();
    registerPaneFindProvider('a', p);
    const before = document.createElement('button');
    openPaneFind('a', { returnFocus: before });
    setPaneFindQuery('a', 'x');
    p.search.mockClear();
    openPaneFind('a');
    expect(p.search).not.toHaveBeenCalled();
    expect(getPaneFind('a')?.returnFocus).toBe(before);
  });

  it('seeds the query from a selection', () => {
    const p = provider();
    registerPaneFindProvider('a', p);
    openPaneFind('a', { seed: 'picked text' });
    expect(getPaneFind('a')?.query).toBe('picked text');
    expect(p.search).toHaveBeenCalledWith('picked text');
  });

  it('keeps two panes independent', () => {
    const pa = provider();
    const pb = provider();
    registerPaneFindProvider('a', pa);
    registerPaneFindProvider('b', pb);
    openPaneFind('a');
    setPaneFindQuery('a', 'alpha');
    openPaneFind('b');
    setPaneFindQuery('b', 'beta');
    closePaneFind('a');
    expect(getPaneFind('a')).toMatchObject({ open: false, query: 'alpha' });
    expect(getPaneFind('b')).toMatchObject({ open: true, query: 'beta' });
    expect(pb.clear).not.toHaveBeenCalled();
    expect(pa.search).not.toHaveBeenCalledWith('beta');
  });

  it('clears the state of a pane that left the layout', () => {
    const release = registerPaneFindProvider('a', provider());
    openPaneFind('a');
    setPaneFindQuery('a', 'gone');
    layout('b');
    release();
    expect(getPaneFind('a')).toBeNull();
  });

  it('drops the state when the pane closes with no provider mounted, so a recreated id starts closed', () => {
    const release = registerPaneFindProvider('a', provider());
    openPaneFind('a', { returnFocus: document.createElement('button') });
    setPaneFindQuery('a', 'gone');
    // The pane switched to a body with no find, then closed.
    release();
    removePaneLayoutItem('a', { persist: false });
    expect(getPaneFind('a')).toBeNull();
    addPaneLayoutItem({ id: 'a', paneId: 'a', kind: 'thread', widthPx: 600 }, undefined, { persist: false });
    const again = provider();
    registerPaneFindProvider('a', again);
    expect(getPaneFind('a')).toBeNull();
    expect(again.search).not.toHaveBeenCalled();
  });

  it('keeps the state of a pane whose body remounts', () => {
    const release = registerPaneFindProvider('a', provider());
    openPaneFind('a');
    setPaneFindQuery('a', 'kept');
    release();
    expect(getPaneFind('a')).toMatchObject({ open: true, query: 'kept' });
    const again = provider();
    registerPaneFindProvider('a', again);
    expect(again.search).toHaveBeenCalledWith('kept');
  });

  it('steps through the provider, and a provider without stepping is a no-op', () => {
    const p = provider();
    registerPaneFindProvider('a', p);
    openPaneFind('a');
    stepPaneFind('a', 1);
    stepPaneFind('a', -1);
    expect(p.next).toHaveBeenCalledTimes(1);
    expect(p.previous).toHaveBeenCalledTimes(1);
    registerPaneFindProvider('b', provider({ next: undefined, previous: undefined }));
    openPaneFind('b');
    expect(() => stepPaneFind('b', 1)).not.toThrow();
  });

  it('a stale release does not drop a newer provider', () => {
    const release = registerPaneFindProvider('a', provider());
    registerPaneFindProvider('a', provider());
    release();
    expect(hasPaneFindProvider('a')).toBe(true);
  });
});

describe('pane find reads', () => {
  function gate() {
    let open!: () => void;
    const done = new Promise<void>((resolve) => { open = resolve; });
    return { done, open };
  }

  it('runs one read per pane at a time and only the newest waiting one', async () => {
    const first = gate();
    const ran: string[] = [];
    const read = (name: string, wait?: Promise<void>) => async () => { ran.push(name); await wait; };
    paneFindReads('a').request(read('first', first.done));
    paneFindReads('a').request(read('stale'));
    // A remounted body's provider queues behind the read still out.
    paneFindReads('a').request(read('newest'));
    paneFindReads('b').request(read('other pane'));
    expect(ran).toEqual(['first', 'other pane']);
    first.open();
    await vi.waitFor(() => expect(ran).toEqual(['first', 'other pane', 'newest']));
  });

  it('releases the pane\'s reads when it leaves the layout', async () => {
    const first = gate();
    const ran: string[] = [];
    paneFindReads('a').request(async () => { ran.push('first'); await first.done; });
    paneFindReads('a').request(async () => { ran.push('waiting'); });
    removePaneLayoutItem('a', { persist: false });
    paneFindReads('a').request(async () => { ran.push('recreated'); });
    // A pane recreated under the same id does not wait on the removed pane's read.
    expect(ran).toEqual(['first', 'recreated']);
    first.open();
    await first.done;
    await Promise.resolve();
    await Promise.resolve();
    expect(ran).toEqual(['first', 'recreated']);
  });
});

describe('pane find regions', () => {
  const list = paneFindListKey('a');
  const detail = paneFindDetailKey('a');

  function regions(shown: () => string | null = () => null) {
    return declarePaneFindRegions('a', { initial: PANE_FIND_LIST_REGION, shown });
  }

  it('derives region keys from the layout pane id, distinct per pane', () => {
    expect(list).not.toBe(detail);
    expect(paneOfFindKey(list)).toBe('a');
    expect(paneOfFindKey(detail)).toBe('a');
    expect(paneOfFindKey('a')).toBe('a');
    expect(paneFindDetailKey('b')).not.toBe(detail);
    expect(paneFindRegionKey('a', PANE_FIND_DETAIL_REGION)).toBe(detail);
  });

  it('targets the pane itself until it declares regions, then the initial region', () => {
    expect(paneFindTarget('a')).toBe('a');
    expect(paneFindTarget(null)).toBeNull();
    const release = regions();
    expect(paneFindTarget('a')).toBe(list);
    release();
    expect(paneFindTarget('a')).toBe('a');
  });

  it('follows the region last activated, and ignores a key with no region', () => {
    regions();
    activatePaneFindRegion(detail);
    expect(paneFindTarget('a')).toBe(detail);
    activatePaneFindRegion(list);
    expect(paneFindTarget('a')).toBe(list);
    activatePaneFindRegion('a');
    expect(paneFindTarget('a')).toBe(list);
    // Another pane's region does not move this one.
    activatePaneFindRegion(paneFindDetailKey('b'));
    expect(paneFindTarget('a')).toBe(list);
  });

  it('keeps each region\'s provider, state and focus return apart; both may be open', () => {
    regions();
    const listProvider = provider({ next: undefined, previous: undefined });
    const detailProvider = provider();
    registerPaneFindProvider(list, listProvider);
    registerPaneFindProvider(detail, detailProvider);
    const row = document.createElement('button');
    const diff = document.createElement('button');

    openPaneFind(list, { returnFocus: row });
    setPaneFindQuery(list, 'queue');
    activatePaneFindRegion(detail);
    openPaneFind(detail, { returnFocus: diff });
    setPaneFindQuery(detail, 'retry');
    expect(listProvider.search).toHaveBeenLastCalledWith('queue');
    expect(detailProvider.search).toHaveBeenLastCalledWith('retry');
    expect(isPaneFindOpen(list)).toBe(true);
    expect(isPaneFindOpen(detail)).toBe(true);

    // Closing one leaves the other open with its own query.
    closePaneFind(detail);
    expect(detailProvider.clear).toHaveBeenCalledTimes(1);
    expect(listProvider.clear).not.toHaveBeenCalled();
    expect(getPaneFind(list)).toMatchObject({ open: true, query: 'queue', returnFocus: row });
    expect(takePaneFindReturnFocus(detail)).toBe(diff);
    expect(takePaneFindReturnFocus(list)).toBe(row);
  });

  it('a detail with no provider has no find, whatever the list holds', () => {
    regions();
    registerPaneFindProvider(list, provider());
    openPaneFind(list);
    activatePaneFindRegion(detail);
    const target = paneFindTarget('a')!;
    expect(hasPaneFindProvider(target)).toBe(false);
    expect(isPaneFindOpen(target)).toBe(false);
    expect(openPaneFind(target)).toBe(false);
    expect(getPaneFind(list)?.open).toBe(true);
  });

  it('targets the region on screen when only one shows, over the one last activated', () => {
    let shown: string | null = PANE_FIND_LIST_REGION;
    regions(() => shown);
    activatePaneFindRegion(detail);
    expect(paneFindTarget('a')).toBe(list);
    shown = PANE_FIND_DETAIL_REGION;
    expect(paneFindTarget('a')).toBe(detail);
    shown = null;
    expect(paneFindTarget('a')).toBe(detail);
  });

  it('drops every region\'s state, reads and declaration when the pane leaves the layout', () => {
    regions();
    registerPaneFindProvider(list, provider());
    registerPaneFindProvider(detail, provider());
    openPaneFind(list);
    setPaneFindQuery(list, 'gone');
    openPaneFind(detail);
    activatePaneFindRegion(detail);
    const ran: string[] = [];
    paneFindReads(detail).request(() => new Promise<void>(() => { ran.push('stuck'); }));
    // The other pane keeps its state.
    registerPaneFindProvider('b', provider());
    openPaneFind('b');
    removePaneLayoutItem('a', { persist: false });
    expect(getPaneFind(list)).toBeNull();
    expect(getPaneFind(detail)).toBeNull();
    expect(paneFindTarget('a')).toBe('a');
    expect(getPaneFind('b')?.open).toBe(true);
    paneFindReads(detail).request(async () => { ran.push('fresh'); });
    expect(ran).toEqual(['stuck', 'fresh']);
  });

  it('repeated opens and closes of one region keep the other untouched', () => {
    regions();
    const listProvider = provider();
    const detailProvider = provider();
    registerPaneFindProvider(list, listProvider);
    registerPaneFindProvider(detail, detailProvider);
    for (let i = 0; i < 3; i += 1) {
      openPaneFind(list);
      setPaneFindQuery(list, `q${i}`);
      closePaneFind(list);
    }
    expect(listProvider.clear).toHaveBeenCalledTimes(3);
    expect(detailProvider.search).not.toHaveBeenCalled();
    expect(detailProvider.clear).not.toHaveBeenCalled();
    expect(getPaneFind(detail)).toBeNull();
    expect(getPaneFind(list)).toMatchObject({ open: false, query: 'q2' });
  });

  it('a provider unregistered while open hides the region\'s find and gets its query back on return', () => {
    regions();
    const first = provider({ next: undefined, previous: undefined });
    const release = registerPaneFindProvider(list, first);
    openPaneFind(list);
    setPaneFindQuery(list, 'kept');
    release();
    expect(first.clear).toHaveBeenCalledTimes(1);
    expect(isPaneFindOpen(list)).toBe(false);
    expect(getPaneFind(list)).toMatchObject({ open: true, query: 'kept' });
    const second = provider({ next: undefined, previous: undefined });
    registerPaneFindProvider(list, second);
    expect(second.search).toHaveBeenCalledWith('kept');
    expect(isPaneFindOpen(list)).toBe(true);
  });

  it('a stale region release does not drop a newer declaration', () => {
    const stale = regions();
    declarePaneFindRegions('a', { initial: PANE_FIND_DETAIL_REGION });
    stale();
    expect(paneFindTarget('a')).toBe(detail);
  });

  it('records each region\'s element for its seed until released', () => {
    const element = document.createElement('div');
    const release = setPaneFindRegionElement(list, element);
    expect(paneFindRegionElement(list)).toBe(element);
    expect(paneFindRegionElement(detail)).toBeNull();
    release();
    expect(paneFindRegionElement(list)).toBeNull();
  });
});

describe('a filter provider', () => {
  it('is not a stepper, and Enter focuses its first result only while open', async () => {
    const focusFirst = vi.fn(() => true);
    const filter = provider({ next: undefined, previous: undefined, focusFirst });
    expect(isSteppingFind(filter)).toBe(false);
    expect(isSteppingFind(provider())).toBe(true);
    registerPaneFindProvider('a', filter);
    expect(await focusFirstPaneFindResult('a')).toBe(false);
    openPaneFind('a');
    expect(await focusFirstPaneFindResult('a')).toBe(true);
    expect(focusFirst).toHaveBeenCalledTimes(1);
    registerPaneFindProvider('b', provider());
    openPaneFind('b');
    expect(await focusFirstPaneFindResult('b')).toBe(false);
  });
});
