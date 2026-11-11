import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { __setTransportHelloForTest } from './transportStatus.svelte';
import {
  THREAD_FIND_DEBOUNCE_MS,
  THREAD_FIND_LIMIT,
  createThreadFind,
  currentHitIndex,
  type ThreadFindHost,
} from './threadFind.svelte';

interface Hit { itemId: string; turnIndex: number; itemIndex: number }

function hit(itemId: string, turnIndex: number, itemIndex = 0): Hit {
  return { itemId, turnIndex, itemIndex };
}

function wireHit(h: Hit) {
  return {
    ownershipEpoch: 0, threadId: 't1', threadTitle: 'T', provider: 'claude',
    itemKind: 'assistant_text', itemRole: 'assistant', summary: 's', matchType: 'item', ...h,
  };
}

function host(overrides: Partial<ThreadFindHost> = {}) {
  const requestScrollToItem = vi.fn();
  const paint = vi.fn();
  const value: ThreadFindHost = {
    scope: () => ({ threadId: 't1' }),
    pane: () => ({ requestScrollToItem }),
    readingPosition: () => null,
    isOnScreen: () => false,
    paint,
    ...overrides,
  };
  return { value, requestScrollToItem, paint };
}

async function settle(): Promise<void> {
  await vi.advanceTimersByTimeAsync(THREAD_FIND_DEBOUNCE_MS + 1);
}

beforeEach(() => {
  vi.useFakeTimers();
  __setTransportHelloForTest({
    protocolVersion: 1, capabilities: ['timeline.scopes.v1'], backendId: '', backendName: '',
    serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0,
  });
});

afterEach(() => {
  vi.useRealTimers();
  __setTransportHelloForTest(null);
});

describe('currentHitIndex', () => {
  const hits = [hit('a', 1), hit('b', 3, 2), hit('c', 7)];

  it('picks the first hit at or below the reading position', () => {
    expect(currentHitIndex(hits, { turnIndex: 3, itemIndex: 2 })).toBe(1);
    expect(currentHitIndex(hits, { turnIndex: 3, itemIndex: 3 })).toBe(2);
    expect(currentHitIndex(hits, { turnIndex: 0, itemIndex: 0 })).toBe(0);
  });

  it('picks the last hit when none is below, and the first with no position', () => {
    expect(currentHitIndex(hits, { turnIndex: 9, itemIndex: 0 })).toBe(2);
    expect(currentHitIndex(hits, null)).toBe(0);
    expect(currentHitIndex([], null)).toBe(-1);
  });
});

describe('thread find', () => {
  it('debounces the read, scopes it to the pane, and reports pending until it answers', async () => {
    const search = setBindingMock('SearchThreadItems', async () => [wireHit(hit('a', 1))]);
    const h = host();
    const find = createThreadFind(h.value);
    find.search('nee');
    find.search('needle');
    expect(find.result.pending).toBe(true);
    await settle();
    expect(search).toHaveBeenCalledTimes(1);
    expect(search.mock.calls[0]).toEqual(['t1', 'needle', THREAD_FIND_LIMIT]);
    expect(find.result).toMatchObject({ count: 1, current: 0, pending: false });
    find.clear();
  });

  it('reads an agent transcript through the scoped search', async () => {
    const scoped = setBindingMock('SearchTimelineItems', async () => [wireHit(hit('child', 2))]);
    const h = host({ scope: () => ({ threadId: 't1', scopeRootId: 'root' }) });
    const find = createThreadFind(h.value);
    find.search('needle');
    await settle();
    expect(scoped.mock.calls[0]).toEqual(['t1', 'needle', { scopeRootId: 'root' }, THREAD_FIND_LIMIT]);
    expect(find.result.count).toBe(1);
    find.clear();
  });

  it('drops a late answer for an older query', async () => {
    const answers: Array<(hits: unknown[]) => void> = [];
    setBindingMock('SearchThreadItems', () => new Promise((resolve) => answers.push(resolve)));
    const h = host();
    const find = createThreadFind(h.value);
    find.search('first');
    await settle();
    find.search('second');
    await settle();
    answers[1]([wireHit(hit('b', 2)), wireHit(hit('c', 3))]);
    await vi.advanceTimersByTimeAsync(0);
    answers[0]([wireHit(hit('a', 1))]);
    await vi.advanceTimersByTimeAsync(0);
    expect(find.result).toMatchObject({ count: 2, pending: false });
    find.clear();
  });

  it('starts at the hit nearest the reading position and jumps only when it is off screen', async () => {
    setBindingMock('SearchThreadItems', async () => [wireHit(hit('a', 1)), wireHit(hit('b', 5)), wireHit(hit('c', 9))]);
    let onScreen = false;
    const h = host({ readingPosition: () => ({ turnIndex: 4, itemIndex: 0 }), isOnScreen: () => onScreen });
    const find = createThreadFind(h.value);
    find.search('needle');
    await settle();
    expect(find.result.current).toBe(1);
    expect(h.requestScrollToItem).toHaveBeenCalledWith('b', { mark: true });
    expect(h.paint).toHaveBeenLastCalledWith('needle', 'b');

    h.requestScrollToItem.mockClear();
    onScreen = true;
    find.search('needles');
    await settle();
    expect(h.requestScrollToItem).not.toHaveBeenCalled();
    find.clear();
  });

  it('steps with wrap in both directions and always reveals the step', async () => {
    setBindingMock('SearchThreadItems', async () => [wireHit(hit('a', 1)), wireHit(hit('b', 2))]);
    const h = host({ isOnScreen: () => true });
    const find = createThreadFind(h.value);
    find.search('needle');
    await settle();
    expect(find.result.current).toBe(0);
    find.next();
    expect(find.result.current).toBe(1);
    find.next();
    expect(find.result.current).toBe(0);
    find.previous();
    expect(find.result.current).toBe(1);
    expect(h.requestScrollToItem.mock.calls.map((c) => c[0])).toEqual(['b', 'a', 'b']);
    find.clear();
  });

  it('a refresh keeps the current hit and does not move the view', async () => {
    let answer = [wireHit(hit('a', 1)), wireHit(hit('b', 2))];
    setBindingMock('SearchThreadItems', async () => answer);
    const h = host({ isOnScreen: () => true });
    const find = createThreadFind(h.value);
    find.search('needle');
    await settle();
    find.next();
    h.requestScrollToItem.mockClear();
    answer = [wireHit(hit('z', 0)), wireHit(hit('a', 1)), wireHit(hit('b', 2))];
    find.refresh();
    await vi.advanceTimersByTimeAsync(2000);
    expect(find.result).toMatchObject({ count: 3, current: 2 });
    expect(h.requestScrollToItem).not.toHaveBeenCalled();
    find.clear();
  });

  it('an empty query and clear drop hits and paint nothing', async () => {
    const search = setBindingMock('SearchThreadItems', async () => [wireHit(hit('a', 1))]);
    const h = host();
    const find = createThreadFind(h.value);
    find.search('needle');
    await settle();
    find.search('');
    expect(find.result).toMatchObject({ count: 0, current: -1, pending: false });
    expect(h.paint).toHaveBeenLastCalledWith('', null);

    find.search('needle');
    find.clear();
    await settle();
    expect(search).toHaveBeenCalledTimes(1);
    expect(find.result).toMatchObject({ count: 0, current: -1, pending: false });
    expect(h.paint).toHaveBeenLastCalledWith('', null);
  });

  it('a failed read ends pending with no hits', async () => {
    setBindingMock('SearchThreadItems', async () => { throw new Error('offline'); });
    const find = createThreadFind(host().value);
    find.search('needle');
    await settle();
    expect(find.result).toMatchObject({ count: 0, current: -1, pending: false });
    find.clear();
  });

  it('reports a capped count at the limit', async () => {
    setBindingMock('SearchThreadItems', async () =>
      Array.from({ length: THREAD_FIND_LIMIT }, (_, i) => wireHit(hit(`i${i}`, i))));
    const find = createThreadFind(host().value);
    find.search('e');
    await settle();
    expect(find.result).toMatchObject({ count: THREAD_FIND_LIMIT, capped: true });
    find.clear();
  });
});
