import { afterEach, describe, expect, it, vi } from 'vitest';
import { createReviewFind, REVIEW_FIND_REMOTE_DEBOUNCE_MS, type ReviewFindHost, type ReviewFindSource } from './reviewFind.svelte';
import { closePaneFind, openPaneFind, registerPaneFindProvider, resetPaneFindForTest, setPaneFindQuery } from './paneFind.svelte';

interface Hit { identity: string; at: number }

// A source over `items` (identity strings) that pauses after every
// `slice` matches until the test releases it.
function source(items: () => string[], options: { slice?: number; remote?: boolean; fail?: () => Error | null } = {}) {
  const gates: (() => void)[] = [];
  let runs = 0;
  const src: ReviewFindSource<Hit> = {
    remote: options.remote ?? false,
    run: async (query, context) => {
      runs += 1;
      const failure = options.fail?.();
      if (failure) throw failure;
      const all = items().filter((item) => item.includes(query));
      for (let i = 0; i < all.length; i += 1) {
        if (options.slice && i > 0 && i % options.slice === 0) {
          if (!context.live()) return false;
          await new Promise<void>((resolve) => gates.push(resolve));
          if (!context.live()) return false;
        }
        context.emit({ identity: all[i], at: i });
      }
      return all.length >= 100;
    },
  };
  return {
    src,
    get runs() { return runs; },
    async release() {
      while (gates.length > 0) {
        gates.shift()!();
        await flush();
      }
    },
  };
}

async function flush(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await Promise.resolve();
}

function host(src: () => ReviewFindSource<Hit> | null, overrides: Partial<ReviewFindHost<Hit>> = {}) {
  const reveals: [string, boolean][] = [];
  const paints: [string, string | null][] = [];
  const h: ReviewFindHost<Hit> = {
    paneId: () => 'pane-r',
    source: src,
    readingPosition: () => () => true,
    reveal: (hit, force) => {
      reveals.push([hit.identity, force]);
      return true;
    },
    paint: (query, current) => paints.push([query, current?.identity ?? null]),
    ...overrides,
  };
  return { h, reveals, paints };
}

afterEach(() => {
  resetPaneFindForTest();
  vi.useRealTimers();
});

describe('review find', () => {
  it('counts progressively, picks the first match below the reading position, and moves only for it', async () => {
    const items = ['a1', 'a2', 'a3', 'a4', 'a5'];
    const s = source(() => items, { slice: 2 });
    const { h, reveals } = host(() => s.src, { readingPosition: () => (hit) => hit.at >= 3 });
    const find = createReviewFind(h, () => 'diff');
    find.search(' a ');
    await flush();
    expect(find.result.pending).toBe(true);
    expect(find.result.progressive).toBe(true);
    await s.release();
    expect(find.result.pending).toBe(false);
    expect(find.result.count).toBe(5);
    expect(find.result.current).toBe(3);
    expect(reveals).toEqual([['a4', false]]);
  });

  it('chooses the last match when none is below, and steps wrap and always move', async () => {
    const s = source(() => ['x1', 'x2']);
    const { h, reveals } = host(() => s.src, { readingPosition: () => () => false });
    const find = createReviewFind(h, () => 'diff');
    find.search('x');
    await flush();
    expect(find.result.current).toBe(1);
    find.next();
    expect(find.result.current).toBe(0);
    find.previous();
    expect(find.result.current).toBe(1);
    expect(reveals).toEqual([['x2', false], ['x1', true], ['x2', true]]);
  });

  it('does not wrap past a scan still running', async () => {
    const s = source(() => ['y1', 'y2', 'y3'], { slice: 2 });
    const { h } = host(() => s.src);
    const find = createReviewFind(h, () => 'diff');
    find.search('y');
    await flush();
    // The scan published its first slice at its pause.
    await s.release();
    find.search('y');
    await flush();
    expect(find.result.pending).toBe(true);
    find.previous();
    expect(find.result.current).toBe(0);
  });

  it('a new query mid-scan stops the old scan, whose late matches never land', async () => {
    const s = source(() => ['q1', 'q2', 'q3', 'q4', 'r1'], { slice: 1 });
    const { h } = host(() => s.src);
    const find = createReviewFind(h, () => 'diff');
    find.search('q');
    await flush();
    find.search('r');
    await s.release();
    expect(find.result.count).toBe(1);
    expect(find.hits.map((hit) => hit.identity)).toEqual(['r1']);
  });

  it('a content change rescans in place: the current match is kept, nothing moves', async () => {
    let items = ['c1', 'c2', 'c3'];
    const s = source(() => items);
    const { h, reveals } = host(() => s.src);
    const find = createReviewFind(h, () => 'diff');
    find.search('c');
    await flush();
    find.next();
    expect(find.result.current).toBe(1);
    reveals.length = 0;
    items = ['c0', 'c1', 'c2', 'c3'];
    find.refresh();
    expect(find.result.pending).toBe(false);
    await flush();
    expect(find.result.count).toBe(4);
    expect(find.result.current).toBe(2);
    expect(reveals).toEqual([]);
    // The current match gone: a new one is chosen and shown if off screen.
    items = ['c0', 'c3'];
    find.refresh();
    await flush();
    expect(find.hits.map((hit) => hit.identity)).toEqual(['c0', 'c3']);
    expect(reveals).toEqual([['c0', false]]);
  });

  it('a reveal that changed the content shows the match again after the rescan', async () => {
    let items = ['f1'];
    const s = source(() => items);
    let opened = false;
    const { h, reveals } = host(() => s.src, {
      reveal: (hit, force) => {
        reveals.push([hit.identity, force]);
        if (!force || opened) return true;
        opened = true;
        return false;
      },
    });
    const find = createReviewFind(h, () => 'conflicts');
    find.search('f');
    await flush();
    find.next();
    items = ['f1'];
    find.refresh();
    await flush();
    expect(reveals).toEqual([['f1', false], ['f1', true], ['f1', true]]);
  });

  it('a view change starts the query over', async () => {
    let items = ['v1', 'v2'];
    const s = source(() => items);
    const { h } = host(() => s.src);
    const find = createReviewFind(h, () => 'diff');
    find.search('v');
    await flush();
    find.next();
    items = ['v9'];
    find.rescope();
    expect(find.result.count).toBe(0);
    expect(find.result.pending).toBe(true);
    await flush();
    expect(find.hits.map((hit) => hit.identity)).toEqual(['v9']);
    expect(find.result.current).toBe(0);
  });

  it('shows a failed scan as its error and clears it on the next answer', async () => {
    let failure: Error | null = new Error('boom');
    const s = source(() => ['e1'], { fail: () => failure });
    const { h } = host(() => s.src);
    const find = createReviewFind(h, () => 'log');
    find.search('e');
    await flush();
    expect(find.result.error).toBe('boom');
    expect(find.result.count).toBe(0);
    expect(find.result.pending).toBe(false);
    failure = null;
    find.refresh();
    await flush();
    expect(find.result.error).toBeNull();
    expect(find.result.count).toBe(1);
  });

  it('debounces a remote source and runs one read at a time', async () => {
    vi.useFakeTimers();
    const s = source(() => ['l1'], { remote: true });
    const { h } = host(() => s.src);
    const find = createReviewFind(h, () => 'log');
    find.search('l');
    find.search('l1');
    expect(s.runs).toBe(0);
    await vi.advanceTimersByTimeAsync(REVIEW_FIND_REMOTE_DEBOUNCE_MS);
    expect(s.runs).toBe(1);
    expect(find.result.count).toBe(1);
  });

  it('close clears matches and paint; reopen searches again; a second close is a no-op', async () => {
    const s = source(() => ['o1']);
    const { h, paints } = host(() => s.src);
    const find = createReviewFind(h, () => 'diff');
    const release = registerPaneFindProvider('pane-r', find);
    openPaneFind('pane-r');
    setPaneFindQuery('pane-r', 'o');
    await flush();
    expect(find.result.count).toBe(1);
    closePaneFind('pane-r');
    closePaneFind('pane-r');
    expect(find.result.count).toBe(0);
    expect(paints.at(-1)).toEqual(['', null]);
    openPaneFind('pane-r');
    await flush();
    expect(find.result.count).toBe(1);
    // The pane's body goes: the session ends with it.
    release();
    expect(find.result.count).toBe(0);
    expect(paints.at(-1)).toEqual(['', null]);
  });

  it('with no source answers nothing and drops what it had', async () => {
    let src: ReviewFindSource<Hit> | null = source(() => ['n1']).src;
    const { h } = host(() => src);
    const find = createReviewFind(h, () => 'diff');
    find.search('n');
    await flush();
    expect(find.result.count).toBe(1);
    src = null;
    find.rescope();
    expect(find.result.count).toBe(0);
    expect(find.result.pending).toBe(false);
  });
});
