import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  closePaneFind,
  getPaneFind,
  hasPaneFindProvider,
  openPaneFind,
  registerPaneFindProvider,
  resetPaneFindForTest,
  setPaneFindQuery,
  stepPaneFind,
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
