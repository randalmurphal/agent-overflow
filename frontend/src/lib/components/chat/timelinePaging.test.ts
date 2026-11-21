// The height-driven fill (docs/architecture/frontend-scroll.md, Live Window
// Bounds): a window whose rows collapse into a few activity runs can be
// shorter than the viewport plus both auto-load zones, where no scroll
// offset ever reaches a trigger. The quiet scheduler asks this once per
// pass and it pages one section at a time until the window is tall enough.
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { createTimelinePaging, type TimelinePaging, type TimelinePagingOptions } from './timelinePaging';
import {
  ACTIVE_TIMELINE_WINDOW_MAX_ITEMS,
  ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
  type LoadOlderResult,
} from '../../stores/threadPaneShared';
import type { TimelineCursorLike } from '../../stores/threadItems';
import type { UseStickToBottomController } from '../../utils/scroll/index.svelte';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import type { Item } from '../../types/models';
import { createThreadPane } from '../../stores/thread.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { makeItem, makeThread, stubScrollController } from '../../../test/helpers/chat';
import { installThreadPaneTestEnv } from '../../../test/helpers/threadPane';

type Pane = ReturnType<TimelinePagingOptions['getPane']>;

function fixture(over: {
  scrollHeight: number;
  clientHeight?: number;
  items?: number;
  hasMoreHistory?: boolean;
  hasMoreNewer?: boolean;
  loadingOlder?: boolean;
  loadingNewer?: boolean;
  olderStalled?: boolean;
  newerStalled?: boolean;
  listRef?: boolean;
  revealed?: number;
  measured?: boolean;
  /** Where the reader is scrolled. */
  offset?: number;
}) {
  const loadOlder = vi.fn(async (): Promise<LoadOlderResult> => (
    { status: 'loaded', insertedBeforeWindow: true, insertedRows: true }));
  const loadNewer = vi.fn(async (): Promise<LoadOlderResult> => (
    { status: 'loaded', insertedBeforeWindow: false, insertedRows: true }));
  const markEscaped = vi.fn();
  const pane = {
    items: Array.from({ length: over.items ?? 10 }, () => ({})),
    hasMoreHistory: over.hasMoreHistory ?? true,
    hasMoreNewer: over.hasMoreNewer ?? false,
    loadingOlder: over.loadingOlder ?? false,
    loadingNewer: over.loadingNewer ?? false,
    olderStalled: over.olderStalled ?? false,
    newerStalled: over.newerStalled ?? false,
    oldestLoadedCursor: { turnIndex: 1, itemIndex: 0 } as TimelineCursorLike,
    newestLoadedCursor: { turnIndex: 9, itemIndex: 0 } as TimelineCursorLike,
    scrollStateKey: 'pane-key',
    loadOlder,
    loadNewer,
    switchGeneration: 1,
  } as unknown as Pane;
  const stick = {
    pauseAutoScroll: () => () => {},
    markEscaped,
    forceStick: () => {},
  } as unknown as UseStickToBottomController;
  const viewport = {
    get scrollHeight() { return over.scrollHeight; },
    get clientHeight() { return over.clientHeight ?? 600; },
  } as HTMLDivElement;
  const paging = createTimelinePaging({
    getPane: () => pane,
    stick,
    getListRef: () =>
      over.listRef === false ? undefined : ({
        scrollToIndex: () => {},
        getScrollOffset: () => over.offset ?? 0,
        findItemIndex: () => (over.revealed ?? 0) - 1,
      } as unknown as TimelineVirtualizerHandle),
    getScrollEl: () => viewport,
    getRevealedNodes: () => Array.from({ length: over.revealed ?? 0 }, () => ({})) as never[],
    isWindowMeasured: () => over.measured ?? true,
    getRestoredThreadId: () => 'pane-key',
    beginNavigation: () => 1,
    claimNavigation: () => true,
    saveScrollSnapshot: () => {},
  });
  return { paging, pane, loadOlder, loadNewer, markEscaped, over };
}

describe('maybeFillViewport', () => {
  it('pages older when the window is shorter than the viewport plus both zones', () => {
    const { paging, loadOlder } = fixture({ scrollHeight: 900 });
    expect(paging.maybeFillViewport()).toBe(true);
    expect(loadOlder).toHaveBeenCalledOnce();
  });

  it('stands down once the window is tall enough to scroll into a trigger zone', () => {
    // 600 + 2 × 800: the older zone and the newer zone can both exist with
    // the viewport between them, so the scroll path takes over from here.
    const { paging, loadOlder } = fixture({ scrollHeight: 2200 });
    expect(paging.maybeFillViewport()).toBe(false);
    expect(loadOlder).not.toHaveBeenCalled();
  });

  it('pages newer only when nothing older is left', () => {
    const { paging, loadOlder, loadNewer } = fixture({
      scrollHeight: 900, hasMoreHistory: false, hasMoreNewer: true,
    });
    expect(paging.maybeFillViewport()).toBe(true);
    expect(loadOlder).not.toHaveBeenCalled();
    expect(loadNewer).toHaveBeenCalledOnce();
  });

  it('does nothing with nowhere to page', () => {
    const { paging } = fixture({ scrollHeight: 900, hasMoreHistory: false });
    expect(paging.maybeFillViewport()).toBe(false);
  });

  it('waits for a load already in flight', () => {
    const { paging, loadOlder } = fixture({ scrollHeight: 900, loadingOlder: true });
    expect(paging.maybeFillViewport()).toBe(false);
    expect(loadOlder).not.toHaveBeenCalled();
  });

  it('stops at the retention target: a page it would cut is not worth fetching', () => {
    const { paging, loadOlder } = fixture({
      scrollHeight: 900, items: ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
    });
    expect(paging.maybeFillViewport()).toBe(false);
    expect(loadOlder).not.toHaveBeenCalled();
  });

  it('refuses a hidden pane: zero height is not a short window', () => {
    const { paging, loadOlder } = fixture({ scrollHeight: 0, clientHeight: 0 });
    expect(paging.maybeFillViewport()).toBe(false);
    expect(loadOlder).not.toHaveBeenCalled();
  });

  // Estimates are not the window's height. A fill started on them pauses
  // auto-scroll while the rows measure taller, and a reader pinned to the
  // bottom is left where the estimates put it until the page lands.
  it('waits for the mounted rows to measure, then asks for one more pass', () => {
    const { paging, loadOlder, over } = fixture({ scrollHeight: 900, revealed: 3, measured: false });
    expect(paging.maybeFillViewport()).toBe(false);
    expect(loadOlder).not.toHaveBeenCalled();
    over.measured = true;
    expect(paging.noteWindowMeasured()).toBe(true);
    expect(paging.noteWindowMeasured()).toBe(false);
    expect(paging.maybeFillViewport()).toBe(true);
    expect(loadOlder).toHaveBeenCalledOnce();
  });

  it('asks for no pass on a measurement it did not wait for', () => {
    const { paging } = fixture({ scrollHeight: 900, revealed: 3 });
    expect(paging.maybeFillViewport()).toBe(true);
    expect(paging.noteWindowMeasured()).toBe(false);
  });

  it('fills an empty window, which has nothing to measure', () => {
    const { paging, loadOlder } = fixture({ scrollHeight: 0, revealed: 0, measured: false });
    expect(paging.maybeFillViewport()).toBe(true);
    expect(loadOlder).toHaveBeenCalledOnce();
  });

  // The quiet scheduler reruns the fill on every lap; an edge whose last
  // page went nowhere waits for the edge to move, and the other edge may
  // still fill.
  it.each(['older', 'newer'] as const)('skips a stalled %s edge', (edge) => {
    const stalled = fixture({
      scrollHeight: 900, hasMoreHistory: edge === 'older', hasMoreNewer: edge === 'newer',
      olderStalled: edge === 'older', newerStalled: edge === 'newer',
    });
    expect(stalled.paging.maybeFillViewport()).toBe(false);
    expect(stalled.loadOlder).not.toHaveBeenCalled();
    expect(stalled.loadNewer).not.toHaveBeenCalled();
  });

  it('fills newer while the older edge is stalled', () => {
    const { paging, loadOlder, loadNewer } = fixture({ scrollHeight: 900, hasMoreNewer: true, olderStalled: true });
    expect(paging.maybeFillViewport()).toBe(true);
    expect(loadOlder).not.toHaveBeenCalled();
    expect(loadNewer).toHaveBeenCalledOnce();
  });

  it('refuses before the virtualizer exists', () => {
    const { paging, loadOlder } = fixture({ scrollHeight: 900, listRef: false });
    expect(paging.maybeFillViewport()).toBe(false);
    expect(loadOlder).not.toHaveBeenCalled();
  });
});

// Intent is a reader fact. A load that fires without a gesture (the fill,
// the button under a reader resting on the bottom) must leave the follow
// state alone: the prepend lands above the reader and they are still at
// the bottom. Only reader-asked navigation escapes.
describe('loads and intent', () => {
  it('the viewport fill never writes intent', async () => {
    const { paging, loadOlder, markEscaped } = fixture({ scrollHeight: 900 });
    expect(paging.maybeFillViewport()).toBe(true);
    await vi.waitFor(() => expect(loadOlder).toHaveBeenCalledOnce());
    await Promise.resolve();
    expect(markEscaped).not.toHaveBeenCalled();
  });

  it('load-older never writes intent, whoever asked', async () => {
    const { paging, loadOlder, markEscaped } = fixture({ scrollHeight: 3000 });
    await paging.handleLoadOlder();
    expect(loadOlder).toHaveBeenCalledOnce();
    expect(markEscaped).not.toHaveBeenCalled();
  });

  it('auto load-newer never writes intent', async () => {
    const { paging, loadNewer, markEscaped } = fixture({ scrollHeight: 3000, hasMoreNewer: true });
    await paging.handleLoadNewerAuto();
    expect(loadNewer).toHaveBeenCalledOnce();
    expect(markEscaped).not.toHaveBeenCalled();
  });

  it('the manual load-newer jump is reader navigation and escapes', async () => {
    const { paging, loadNewer, markEscaped } = fixture({ scrollHeight: 3000, hasMoreNewer: true, revealed: 3 });
    await paging.handleLoadNewer();
    expect(loadNewer).toHaveBeenCalledOnce();
    expect(markEscaped).toHaveBeenCalledOnce();
  });
});

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

// Every microtask a settled load queues runs before the next task.
const flush = () => new Promise<void>((resolve) => { setTimeout(resolve, 0); });
const result = (status: LoadOlderResult['status']): LoadOlderResult =>
  ({ status, insertedBeforeWindow: false, insertedRows: status === 'loaded' });

// After a page ends the gate is disarmed, unless the page was refused (its
// edge moved while it was out): a refused page applied nothing, leaves the
// gate armed and starts nothing until the reader scrolls or gestures again.
// Whether a page went anywhere is the window's (`olderStalled` /
// `newerStalled`, tested against the real window below); the gate refuses
// a stalled edge.
describe('auto-load after a page ends', () => {
  // A tall window: the top zone at offset 100, the bottom zone at 3400.
  const tall = { scrollHeight: 4000, clientHeight: 600, revealed: 3, hasMoreNewer: true };
  const edges = {
    older: {
      load: 'loadOlder', stalled: 'olderStalled',
      offset: 100, probe: (paging: ReturnType<typeof fixture>['paging']) => paging.maybeAutoLoadOlder(100),
    },
    newer: {
      load: 'loadNewer', stalled: 'newerStalled',
      offset: 3400, probe: (paging: ReturnType<typeof fixture>['paging']) => paging.maybeAutoLoadNewer(3400),
    },
  } as const;

  // The reader rests in the edge's zone, at the offset it probes from.
  function edgeFixture(name: keyof typeof edges) {
    const edge = edges[name];
    const f = fixture({ ...tall, offset: edge.offset });
    const pane = f.pane as unknown as Record<string, unknown>;
    const load = f[edge.load];
    const setStalled = (stalled: boolean) => { pane[edge.stalled] = stalled; };
    return { ...f, load, setStalled, probe: () => edge.probe(f.paging) };
  }

  it.each(['older', 'newer'] as const)('a refused %s page starts no load without a new scroll or gesture', async (name) => {
    const { load, probe } = edgeFixture(name);
    load.mockImplementationOnce(async () => result('stale'));
    expect(probe()).toBe(true);
    await flush();
    expect(load).toHaveBeenCalledOnce();

    // The gate is still armed: the next scroll probes.
    expect(probe()).toBe(true);
    expect(load).toHaveBeenCalledTimes(2);
  });

  it.each(['older', 'newer'] as const)('skips a stalled %s edge', (name) => {
    const { probe, setStalled, load } = edgeFixture(name);
    setStalled(true);
    expect(probe()).toBe(false);
    setStalled(false);
    expect(probe()).toBe(true);
    expect(load).toHaveBeenCalledOnce();
  });

  it('a page that landed disarms the gate as before', async () => {
    const { paging, load, probe } = edgeFixture('newer');
    load.mockImplementationOnce(async () => result('loaded'));
    expect(probe()).toBe(true);
    await flush();
    expect(probe()).toBe(false);
    paging.armGatesOnUserGesture();
    expect(probe()).toBe(true);
  });

  // The switch reset the gates for the next thread; a load from the thread
  // before it must not disarm them.
  it('a load that ends after a thread switch leaves the reset gate alone', async () => {
    const { paging, pane, load, probe } = edgeFixture('older');
    const out = deferred<LoadOlderResult>();
    load.mockImplementationOnce(() => out.promise);
    expect(probe()).toBe(true);
    (pane as { switchGeneration: number }).switchGeneration += 1;
    paging.resetGates();
    out.resolve(result('error'));
    await flush();
    expect(probe()).toBe(true);
  });
});

// Against the real window, whose edge claims decide which page lands and
// whose stalled flags decide which edge automatic paging skips.
describe('paging the real window', () => {
  beforeEach(installThreadPaneTestEnv);

  type Page = { items: Item[]; hasMore: boolean; hasMoreOlder: boolean; hasMoreNewer: boolean };
  type Held = { resolve: (answer: Page) => void; reject: (error: Error) => void };
  const row = (turnIndex: number) => makeItem({ id: `t${turnIndex}`, threadId: 't', turnIndex, itemIndex: 0 });
  const turns = (from: number, count: number) => Array.from({ length: count }, (_, index) => from + index);
  const page = (rows: number[], more: Partial<Page> = {}): Page =>
    ({ items: rows.map(row), hasMore: true, hasMoreOlder: true, hasMoreNewer: false, ...more });
  const ids = (pane: ReturnType<typeof createThreadPane>) => pane.items.map((item) => item.id);
  const tall = () => ({ scrollHeight: 4000, clientHeight: 600 });

  // Every page held until the test answers it. The window opens on
  // `initial`: rows t10 and t11 with more history on both sides unless the
  // test says otherwise. `geometry` is live: a test may change its height.
  async function openWindow(
    geometry: { scrollHeight: number; clientHeight: number },
    initial: Page = page([10, 11], { hasMoreNewer: true }),
  ) {
    const pane = createThreadPane();
    setBindingMock('ListThreadSliceAround', async () => initial);
    await pane.switchThread(makeThread({ id: 't' }));
    const olderPages: Held[] = [];
    const newerPages: Held[] = [];
    const older = setBindingMock('ListItemsBeforeCursor', () => new Promise<Page>((resolve, reject) => { olderPages.push({ resolve, reject }); }));
    setBindingMock('ListItemsAfterCursor', () => new Promise<Page>((resolve, reject) => { newerPages.push({ resolve, reject }); }));
    const paging = createTimelinePaging({
      getPane: () => pane,
      stick: { pauseAutoScroll: () => () => {}, markEscaped: () => {}, forceStick: () => {} } as unknown as UseStickToBottomController,
      getListRef: () => ({ scrollToIndex: () => {}, getScrollOffset: () => 0, findItemIndex: () => 0 }) as unknown as TimelineVirtualizerHandle,
      getScrollEl: () => geometry as HTMLDivElement,
      getRevealedNodes: () => [],
      isWindowMeasured: () => true,
      getRestoredThreadId: () => pane.scrollStateKey,
      beginNavigation: () => 1,
      claimNavigation: () => true,
      saveScrollSnapshot: () => {},
    });
    // A navigation that replaces the window with `slice` around row `id`.
    async function navigate(id: number, slice: Page) {
      setBindingMock('GetThreadItem', async () => row(id));
      setBindingMock('ListThreadSliceAround', async () => slice);
      expect(await pane.loadUntilItem(`t${id}`)).toBe('loaded');
    }
    return { pane, paging, geometry, older, olderPages, newerPages, navigate };
  }

  const probe = {
    older: (paging: TimelinePaging) => paging.maybeAutoLoadOlder(100),
    newer: (paging: TimelinePaging) => paging.maybeAutoLoadNewer(3400),
  };

  // The fill pages one edge per pass: older first, then newer while the
  // older page is still out. Both pages extend the window they were asked
  // from.
  it('the viewport fill lands an older and a newer page that are out together', async () => {
    const { pane, paging, olderPages, newerPages } = await openWindow({ scrollHeight: 300, clientHeight: 600 });
    expect(paging.maybeFillViewport()).toBe(true);
    expect(pane.loadingOlder).toBe(true);
    expect(paging.maybeFillViewport()).toBe(true);
    expect(pane.loadingNewer).toBe(true);

    olderPages[0].resolve(page([9]));
    newerPages[0].resolve(page([12], { hasMoreNewer: true }));
    await vi.waitFor(() => expect(pane.loadingOlder || pane.loadingNewer).toBe(false));
    expect(ids(pane)).toEqual(['t9', 't10', 't11', 't12']);
  });

  // A navigation holds both edges and releases them when it ends, while an
  // older page it refused is still out. The next older page may go out at
  // the same floor: its claim is the live one, and the refused answer
  // applies nothing whenever it arrives.
  it.each(['first', 'last'])('a page sent while a refused page is still out lands when the refused one answers %s', async (when) => {
    const { pane, paging, older, olderPages } = await openWindow(tall());
    expect(paging.maybeAutoLoadOlder(100)).toBe(true);
    setBindingMock('GetThreadItem', async () => null);
    expect(await pane.loadUntilItem('gone')).toBe('missing');
    expect(pane.loadingOlder).toBe(false);

    expect(paging.maybeAutoLoadOlder(100)).toBe(true);
    expect(older).toHaveBeenCalledTimes(2);
    const [refused, live] = olderPages;
    const answers = [() => refused.resolve(page([8])), () => live.resolve(page([9]))];
    for (const answer of when === 'first' ? answers : answers.reverse()) {
      answer();
      await flush();
    }
    expect(pane.loadingOlder).toBe(false);
    expect(ids(pane)).toEqual(['t9', 't10', 't11']);
  });

  // In a window too short for disjoint zones only an upward move pages
  // older. Jump to latest replaces the window under the page an upward
  // gesture sent; the reader is at the tail now, so nothing pages older
  // until they move up again.
  it('Jump to latest refuses an older page an upward gesture sent, and nothing pages older until the next upward move', async () => {
    const { pane, paging, older, olderPages } = await openWindow({ scrollHeight: 900, clientHeight: 600 });
    expect(paging.probeOlderOnUpwardGesture()).toBe(true);
    setBindingMock('ListThreadSliceAround', async () => page([20, 21]));
    await paging.jumpToLatest();
    expect(ids(pane)).toEqual(['t20', 't21']);

    olderPages[0].resolve(page([9]));
    await flush();
    expect(older).toHaveBeenCalledOnce();
    expect(paging.maybeAutoLoadOlder(0)).toBe(false);
    expect(older).toHaveBeenCalledOnce();
    expect(ids(pane)).toEqual(['t20', 't21']);

    expect(paging.probeOlderOnUpwardGesture()).toBe(true);
    expect(older).toHaveBeenCalledTimes(2);
  });

  // Whether a page went anywhere is decided from what it committed. Pages
  // at both edges are out over a window the reader holds at one edge. This
  // edge's page lands and moves it; before that load has finished, the
  // other edge's page lands and its cut, which keeps the row the reader is
  // on, puts this edge back where it started. The page loaded: the edge
  // stays open.
  it.each(['older', 'newer'] as const)("a page that loaded leaves the %s edge open after the other edge's cut puts it back", async (edge) => {
    const max = ACTIVE_TIMELINE_WINDOW_MAX_ITEMS;
    const { pane, paging, olderPages, newerPages } = await openWindow(
      tall(), page(turns(1000, max - 200), { hasMoreNewer: true }),
    );
    const start = edge === 'older' ? 1000 : 1000 + max - 201;
    pane.attachScrollController(stubScrollController({ visibleTimelineItemIds: () => new Set([`t${start}`]) }));
    const loads = [paging.handleLoadOlder(), paging.handleLoadNewerAuto()];
    // This edge's page fills the window to the cap; the other edge's page
    // pushes it past, and its cut drops what this edge's page added.
    const answerOlder = () => olderPages[0].resolve(page(turns(800, 200)));
    const answerNewer = () => newerPages[0].resolve(page(turns(1000 + max - 200, 200), { hasMoreNewer: true }));
    if (edge === 'older') {
      answerOlder();
      answerNewer();
    } else {
      answerNewer();
      answerOlder();
    }
    await Promise.all(loads);
    expect((edge === 'older' ? pane.oldestLoadedCursor : pane.newestLoadedCursor)?.turnIndex).toBe(start);

    paging.armGatesOnUserGesture();
    expect(probe[edge](paging)).toBe(true);
  });

  // A failed page stalls its edge until the edge moves. Two replacements
  // move it away and back to the same cursor: it moved, so it is open.
  it.each(['older', 'newer'] as const)('a failed %s page stops blocking once replacements move the edge away and back', async (edge) => {
    const more = { hasMoreOlder: edge === 'older', hasMoreNewer: edge === 'newer' };
    const { pane, paging, olderPages, newerPages, navigate } = await openWindow(tall(), page([10, 11, 12], more));
    const pages = edge === 'older' ? olderPages : newerPages;
    const failed = edge === 'older' ? paging.handleLoadOlder() : paging.handleLoadNewerAuto();
    pages[0].reject(new Error(`${edge} page failed`));
    await failed;
    paging.armGatesOnUserGesture();
    expect(probe[edge](paging)).toBe(false);

    await navigate(50, page([50, 51, 52], more));
    await navigate(10, page([10, 11, 12], more));
    expect(ids(pane)).toEqual(['t10', 't11', 't12']);
    expect(probe[edge](paging)).toBe(true);
    expect(pages).toHaveLength(2);
  });

  // Every page at an edge stalls it when it fails, whoever sent it, and the
  // stall keeps auto-load and the viewport fill off that edge until a page
  // moves it. The explicit buttons still load it.
  it.each([
    ['the Load older button', 'older', 900, (paging: TimelinePaging) => { void paging.handleLoadOlder(); }],
    ['the viewport fill', 'older', 900, (paging: TimelinePaging) => { expect(paging.maybeFillViewport()).toBe(true); }],
    ['auto-load', 'older', 4000, (paging: TimelinePaging) => { expect(paging.maybeAutoLoadOlder(100)).toBe(true); }],
    ['the Load newer button', 'newer', 900, (paging: TimelinePaging) => { void paging.handleLoadNewer(); }],
    ['the viewport fill', 'newer', 900, (paging: TimelinePaging) => { expect(paging.maybeFillViewport()).toBe(true); }],
    ['auto-load', 'newer', 4000, (paging: TimelinePaging) => { expect(paging.maybeAutoLoadNewer(3400)).toBe(true); }],
  ] as const)('a failed page from %s keeps automatic paging off the %s edge until a page moves it', async (_, edge, height, send) => {
    const more = { hasMoreOlder: edge === 'older', hasMoreNewer: edge === 'newer' };
    const { pane, paging, geometry, olderPages, newerPages } = await openWindow(
      { scrollHeight: height, clientHeight: 600 }, page([10, 11, 12], more),
    );
    const pages = edge === 'older' ? olderPages : newerPages;
    send(paging);
    pages[0].reject(new Error(`${edge} page failed`));
    await flush();

    geometry.scrollHeight = 900;
    expect(paging.maybeFillViewport()).toBe(false);
    geometry.scrollHeight = 4000;
    paging.armGatesOnUserGesture();
    expect(probe[edge](paging)).toBe(false);
    expect(pages).toHaveLength(1);

    const click = edge === 'older' ? paging.handleLoadOlder() : paging.handleLoadNewer();
    pages[1].resolve(edge === 'older' ? page([9]) : page([13], { hasMoreNewer: true }));
    await click;
    expect(ids(pane)).toEqual(edge === 'older' ? ['t9', 't10', 't11', 't12'] : ['t10', 't11', 't12', 't13']);
    paging.armGatesOnUserGesture();
    expect(probe[edge](paging)).toBe(true);
  });

  // A navigation refused page A and released the older edge while A was
  // still out; page B goes out at the new floor and fails. A's refusal, in
  // either order, leaves B's stall in place, or the next gesture repeats
  // B's failed query and its toast.
  it.each(['the refused page', 'the failed page'])('a refused page leaves the stall of the page after it when %s answers first', async (first) => {
    const { paging, older, olderPages, navigate } = await openWindow(tall());
    expect(paging.maybeAutoLoadOlder(100)).toBe(true);
    await navigate(50, page([50, 51, 52]));
    paging.armGatesOnUserGesture();
    expect(paging.maybeAutoLoadOlder(100)).toBe(true);
    const [a, b] = olderPages;
    const answers = [() => a.resolve(page([8])), () => b.reject(new Error('older page failed'))];
    for (const answer of first === 'the refused page' ? answers : answers.reverse()) {
      answer();
      await flush();
    }

    paging.armGatesOnUserGesture();
    expect(paging.maybeAutoLoadOlder(100)).toBe(false);
    expect(older).toHaveBeenCalledTimes(2);
  });
});
