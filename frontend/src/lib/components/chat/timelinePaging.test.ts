// The height-driven fill (docs/architecture/frontend-scroll.md, Live Window
// Bounds): a window whose rows collapse into a few activity runs can be
// shorter than the viewport plus both auto-load zones, where no scroll
// offset ever reaches a trigger. The quiet scheduler asks this once per
// pass and it pages one section at a time until the window is tall enough.
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { createTimelinePaging, type TimelinePagingOptions } from './timelinePaging';
import { ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS, type LoadOlderResult } from '../../stores/threadPaneShared';
import type { TimelineCursorLike } from '../../stores/threadItems';
import type { UseStickToBottomController } from '../../utils/scroll/index.svelte';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import type { Item } from '../../types/models';
import { createThreadPane } from '../../stores/thread.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { makeItem, makeThread } from '../../../test/helpers/chat';
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

  // The quiet scheduler reruns the fill on every lap; a page that failed
  // waits for its edge to move, and the other edge may still fill.
  it.each(['older', 'newer'] as const)('does not send a failed %s page again until that edge moves', async (edge) => {
    const f = fixture({ scrollHeight: 900, hasMoreHistory: edge === 'older', hasMoreNewer: edge === 'newer' });
    const load = edge === 'older' ? f.loadOlder : f.loadNewer;
    load.mockImplementationOnce(async () => ({ status: 'error', insertedBeforeWindow: false, insertedRows: false }));
    expect(f.paging.maybeFillViewport()).toBe(true);
    await flush();
    expect(f.paging.maybeFillViewport()).toBe(false);
    expect(load).toHaveBeenCalledOnce();

    (f.pane as unknown as Record<string, TimelineCursorLike>)[edge === 'older' ? 'oldestLoadedCursor' : 'newestLoadedCursor'] =
      { turnIndex: 5, itemIndex: 0 };
    expect(f.paging.maybeFillViewport()).toBe(true);
    expect(load).toHaveBeenCalledTimes(2);
  });

  it('fills newer while the older edge waits on a failed page', async () => {
    const { paging, loadOlder, loadNewer } = fixture({ scrollHeight: 900, hasMoreNewer: true });
    loadOlder.mockImplementationOnce(async () => ({ status: 'error', insertedBeforeWindow: false, insertedRows: false }));
    expect(paging.maybeFillViewport()).toBe(true);
    await flush();
    expect(paging.maybeFillViewport()).toBe(true);
    expect(loadOlder).toHaveBeenCalledOnce();
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

// The gate records what a load that ran learned, when it ends: a page that
// moved the floor clears the record, an error or a page that left the floor
// where it was records it, and a refused page (its edge moved while it was
// out) records nothing, leaves the gate armed and starts nothing until the
// reader scrolls or gestures again.
describe('what a settled load teaches the gate', () => {
  // A tall window: the top zone at offset 100, the bottom zone at 3400.
  const tall = { scrollHeight: 4000, clientHeight: 600, revealed: 3, hasMoreNewer: true };
  const edges = {
    older: {
      load: 'loadOlder', cursor: 'oldestLoadedCursor', start: { turnIndex: 5, itemIndex: 0 }, next: { turnIndex: 3, itemIndex: 0 },
      offset: 100, probe: (paging: ReturnType<typeof fixture>['paging']) => paging.maybeAutoLoadOlder(100),
    },
    newer: {
      load: 'loadNewer', cursor: 'newestLoadedCursor', start: { turnIndex: 9, itemIndex: 0 }, next: { turnIndex: 11, itemIndex: 0 },
      offset: 3400, probe: (paging: ReturnType<typeof fixture>['paging']) => paging.maybeAutoLoadNewer(3400),
    },
  } as const;

  // The reader rests in the edge's zone, at the offset it probes from.
  function edgeFixture(name: keyof typeof edges) {
    const edge = edges[name];
    const f = fixture({ ...tall, offset: edge.offset });
    const pane = f.pane as unknown as Record<string, TimelineCursorLike>;
    const load = f[edge.load];
    const moveFloor = (cursor: TimelineCursorLike) => { pane[edge.cursor] = cursor; };
    moveFloor(edge.start);
    return { ...f, edge, load, moveFloor, probe: () => edge.probe(f.paging) };
  }

  it.each(['older', 'newer'] as const)('a refused %s page starts no load without a new scroll or gesture', async (name) => {
    const { load, probe } = edgeFixture(name);
    load.mockImplementationOnce(async () => result('stale'));
    expect(probe()).toBe(true);
    await flush();
    expect(load).toHaveBeenCalledOnce();

    // The gate is still armed and holds no record: the next scroll probes.
    expect(probe()).toBe(true);
    expect(load).toHaveBeenCalledTimes(2);
  });

  it.each(['older', 'newer'] as const)('an errored %s load blocks its floor until the floor moves', async (name) => {
    const { paging, load, probe, moveFloor, edge } = edgeFixture(name);
    load.mockImplementationOnce(async () => result('error'));
    expect(probe()).toBe(true);
    await flush();
    paging.armGatesOnUserGesture();
    expect(probe()).toBe(false);
    moveFloor(edge.next);
    expect(probe()).toBe(true);
  });

  // A navigation moved the older edge and released its loading flag while
  // page A was still out; page B loads at the new floor and fails. A's
  // refusal, in either order, must leave B's failure on record, or the next
  // gesture repeats B's failed query and its toast.
  it.each(['the refused page', 'the failed page'])('a refused page leaves the failure of the page after it on record when %s answers first', async (first) => {
    const { paging, load, probe, moveFloor, edge } = edgeFixture('older');
    const a = deferred<LoadOlderResult>();
    const b = deferred<LoadOlderResult>();
    load.mockImplementationOnce(() => a.promise).mockImplementationOnce(() => b.promise);
    expect(probe()).toBe(true);
    moveFloor(edge.next);
    expect(probe()).toBe(true);
    const answers = [() => a.resolve(result('stale')), () => b.resolve(result('error'))];
    for (const answer of first === 'the refused page' ? answers : answers.reverse()) {
      answer();
      await flush();
    }

    paging.armGatesOnUserGesture();
    expect(probe()).toBe(false);
    expect(load).toHaveBeenCalledTimes(2);
  });

  // A page lands and moves the floor; the next page goes out at the new
  // floor; a cut at the other edge's page drops what the first page added,
  // which returns the floor to where the first page started and refuses the
  // second. That floor loaded fine, so the edge stays open.
  it.each(['older', 'newer'] as const)('a cut that returns the %s edge to a floor that loaded leaves it open', async (name) => {
    const { paging, load, probe, moveFloor, edge } = edgeFixture(name);
    const second = deferred<LoadOlderResult>();
    load
      .mockImplementationOnce(async () => { moveFloor(edge.next); return result('loaded'); })
      .mockImplementationOnce(() => second.promise);
    expect(probe()).toBe(true);
    await flush();
    paging.armGatesOnUserGesture();
    expect(probe()).toBe(true);
    moveFloor(edge.start);
    second.resolve(result('stale'));
    await flush();

    expect(probe()).toBe(true);
    expect(load).toHaveBeenCalledTimes(3);
  });

  it('a page that landed disarms the gate as before', async () => {
    const { paging, load, probe, moveFloor, edge } = edgeFixture('newer');
    load.mockImplementationOnce(async () => { moveFloor(edge.next); return result('loaded'); });
    expect(probe()).toBe(true);
    await flush();
    // The page moved the ceiling; only the disarm holds the next probe back.
    expect(probe()).toBe(false);
    paging.armGatesOnUserGesture();
    expect(probe()).toBe(true);
  });

  // Every load at an edge reports to its gate, not only auto-load: a failed
  // click or fill keeps auto-load from repeating the same failed query.
  it.each([
    ['the Load older button', 'older', (paging: ReturnType<typeof fixture>['paging']) => paging.handleLoadOlder()],
    ['the Load newer button', 'newer', (paging: ReturnType<typeof fixture>['paging']) => paging.handleLoadNewer()],
  ] as const)('a failed load from %s blocks its floor for auto-load', async (_, name, run) => {
    const { paging, load, probe } = edgeFixture(name);
    load.mockImplementationOnce(async () => result('error'));
    await run(paging);
    paging.armGatesOnUserGesture();
    expect(probe()).toBe(false);
    expect(load).toHaveBeenCalledOnce();
  });

  // The switch reset the gates for the next thread; a load from the thread
  // before it must not write into them.
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

  it('a failed viewport fill blocks its floor for auto-load', async () => {
    const { paging, load, over } = edgeFixture('older');
    over.scrollHeight = 900;
    load.mockImplementationOnce(async () => result('error'));
    expect(paging.maybeFillViewport()).toBe(true);
    await flush();
    over.scrollHeight = 4000;
    paging.armGatesOnUserGesture();
    expect(paging.maybeAutoLoadOlder(100)).toBe(false);
    expect(load).toHaveBeenCalledOnce();
  });
});

// Against the real window, whose edge claims decide which page lands.
describe('paging the real window', () => {
  beforeEach(installThreadPaneTestEnv);

  type Page = { items: Item[]; hasMore: boolean; hasMoreOlder: boolean; hasMoreNewer: boolean };
  const row = (turnIndex: number) => makeItem({ id: `t${turnIndex}`, threadId: 't', turnIndex, itemIndex: 0 });
  const page = (turns: number[], more: Partial<Page> = {}): Page =>
    ({ items: turns.map(row), hasMore: true, hasMoreOlder: true, hasMoreNewer: false, ...more });
  const ids = (pane: ReturnType<typeof createThreadPane>) => pane.items.map((item) => item.id);

  // Rows t10 and t11, with more history on both sides; every page held.
  async function openWindow(geometry: { scrollHeight: number; clientHeight: number }) {
    const pane = createThreadPane();
    setBindingMock('ListThreadSliceAround', async () => page([10, 11], { hasMoreNewer: true }));
    await pane.switchThread(makeThread({ id: 't' }));
    const olderAnswers: ((answer: Page) => void)[] = [];
    const newerAnswers: ((answer: Page) => void)[] = [];
    const older = setBindingMock('ListItemsBeforeCursor', () => new Promise<Page>((resolve) => { olderAnswers.push(resolve); }));
    setBindingMock('ListItemsAfterCursor', () => new Promise<Page>((resolve) => { newerAnswers.push(resolve); }));
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
    return { pane, paging, older, olderAnswers, newerAnswers };
  }

  // The fill pages one edge per pass: older first, then newer while the
  // older page is still out. Both pages extend the window they were asked
  // from.
  it('the viewport fill lands an older and a newer page that are out together', async () => {
    const { pane, paging, olderAnswers, newerAnswers } = await openWindow({ scrollHeight: 300, clientHeight: 600 });
    expect(paging.maybeFillViewport()).toBe(true);
    expect(pane.loadingOlder).toBe(true);
    expect(paging.maybeFillViewport()).toBe(true);
    expect(pane.loadingNewer).toBe(true);

    olderAnswers[0](page([9]));
    newerAnswers[0](page([12], { hasMoreNewer: true }));
    await vi.waitFor(() => expect(pane.loadingOlder || pane.loadingNewer).toBe(false));
    expect(ids(pane)).toEqual(['t9', 't10', 't11', 't12']);
  });

  // A navigation holds both edges and releases them when it ends, while an
  // older page it refused is still out. The next older page may go out at
  // the same floor: its claim is the live one, and the refused answer
  // applies nothing whenever it arrives.
  it.each(['first', 'last'])('a page sent while a refused page is still out lands when the refused one answers %s', async (when) => {
    const { pane, paging, older, olderAnswers } = await openWindow({ scrollHeight: 4000, clientHeight: 600 });
    expect(paging.maybeAutoLoadOlder(100)).toBe(true);
    setBindingMock('GetThreadItem', async () => null);
    expect(await pane.loadUntilItem('gone')).toBe('missing');
    expect(pane.loadingOlder).toBe(false);

    expect(paging.maybeAutoLoadOlder(100)).toBe(true);
    expect(older).toHaveBeenCalledTimes(2);
    const [refused, live] = olderAnswers;
    const answers = [() => refused(page([8])), () => live(page([9]))];
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
    const { pane, paging, older, olderAnswers } = await openWindow({ scrollHeight: 900, clientHeight: 600 });
    expect(paging.probeOlderOnUpwardGesture()).toBe(true);
    setBindingMock('ListThreadSliceAround', async () => page([20, 21]));
    await paging.jumpToLatest();
    expect(ids(pane)).toEqual(['t20', 't21']);

    olderAnswers[0](page([9]));
    await flush();
    expect(older).toHaveBeenCalledOnce();
    expect(paging.maybeAutoLoadOlder(0)).toBe(false);
    expect(older).toHaveBeenCalledOnce();
    expect(ids(pane)).toEqual(['t20', 't21']);

    expect(paging.probeOlderOnUpwardGesture()).toBe(true);
    expect(older).toHaveBeenCalledTimes(2);
  });
});
