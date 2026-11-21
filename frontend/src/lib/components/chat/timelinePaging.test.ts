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
  offset?: number;
  newerStatuses?: LoadOlderResult['status'][];
}) {
  const loadOlder = vi.fn(async () => ({ status: 'loaded' as const, insertedBeforeWindow: true, insertedRows: true }));
  const statuses = [...(over.newerStatuses ?? [])];
  const loadNewer = vi.fn(async (): Promise<LoadOlderResult> => (
    { status: statuses.shift() ?? 'loaded', insertedBeforeWindow: false, insertedRows: true }));
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
    scrollHeight: over.scrollHeight,
    clientHeight: over.clientHeight ?? 600,
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

// A refused page (its edge moved while it was out) applied nothing and is no
// attempt. The reader is still at the edge they asked from, so the edge is
// probed again without waiting for a scroll; a re-probe refused at a floor
// that has not moved waits for the next scroll.
describe('a refused auto-load', () => {
  // Resting on the newest edge of a tall window.
  const atNewestEdge = { scrollHeight: 4000, clientHeight: 600, offset: 3400, revealed: 40, hasMoreNewer: true };
  const settle = async () => { for (let i = 0; i < 20; i += 1) await Promise.resolve(); };

  it('probes the same edge again without a scroll', async () => {
    const { paging, loadNewer } = fixture({ ...atNewestEdge, newerStatuses: ['stale', 'loaded'] });
    expect(paging.maybeAutoLoadNewer(3400)).toBe(true);
    await vi.waitFor(() => expect(loadNewer).toHaveBeenCalledTimes(2));
    await settle();
    expect(loadNewer).toHaveBeenCalledTimes(2);
  });

  it('re-probes once at an unmoved floor, then waits for a new scroll', async () => {
    const { paging, loadNewer } = fixture({ ...atNewestEdge, newerStatuses: ['stale', 'stale', 'loaded'] });
    expect(paging.maybeAutoLoadNewer(3400)).toBe(true);
    await vi.waitFor(() => expect(loadNewer).toHaveBeenCalledTimes(2));
    await settle();
    expect(loadNewer).toHaveBeenCalledTimes(2);

    // The refused attempts are forgotten: the next scroll at the same floor loads.
    expect(paging.maybeAutoLoadNewer(3400)).toBe(true);
    await vi.waitFor(() => expect(loadNewer).toHaveBeenCalledTimes(3));
  });

  it('re-probes again when the edge moved under the refused re-probe', async () => {
    const { paging, pane, loadNewer } = fixture({ ...atNewestEdge, newerStatuses: ['loaded'] });
    loadNewer.mockImplementationOnce(async () => ({ status: 'stale', insertedBeforeWindow: false, insertedRows: false }));
    loadNewer.mockImplementationOnce(async () => {
      (pane as { newestLoadedCursor: TimelineCursorLike }).newestLoadedCursor = { turnIndex: 7, itemIndex: 0 };
      return { status: 'stale', insertedBeforeWindow: false, insertedRows: false };
    });
    expect(paging.maybeAutoLoadNewer(3400)).toBe(true);
    await vi.waitFor(() => expect(loadNewer).toHaveBeenCalledTimes(3));
    await settle();
    expect(loadNewer).toHaveBeenCalledTimes(3);
  });

  it('a page that landed disarms the gate as before', async () => {
    const { paging, pane, loadNewer } = fixture({ ...atNewestEdge, newerStatuses: ['loaded'] });
    expect(paging.maybeAutoLoadNewer(3400)).toBe(true);
    await vi.waitFor(() => expect(loadNewer).toHaveBeenCalledOnce());
    await settle();
    // The page moved the ceiling; only the disarm holds the next probe back.
    (pane as { newestLoadedCursor: TimelineCursorLike }).newestLoadedCursor = { turnIndex: 11, itemIndex: 0 };
    expect(paging.maybeAutoLoadNewer(3400)).toBe(false);
    paging.armGatesOnUserGesture();
    expect(paging.maybeAutoLoadNewer(3400)).toBe(true);
  });
});

// The fill pages one edge per pass: older first, then newer while the older
// page is still out. Both pages extend the window they were asked from.
describe('the viewport fill at both edges', () => {
  beforeEach(installThreadPaneTestEnv);

  it('lands an older and a newer page that are out together', async () => {
    const row = (turnIndex: number) => makeItem({ id: `t${turnIndex}`, threadId: 't', turnIndex, itemIndex: 0 });
    const pane = createThreadPane();
    setBindingMock('ListThreadSliceAround', async () => ({
      items: [row(10), row(11)], hasMore: true, hasMoreOlder: true, hasMoreNewer: true,
    }));
    await pane.switchThread(makeThread({ id: 't' }));
    type Page = { items: Item[]; hasMore: boolean; hasMoreOlder: boolean; hasMoreNewer: boolean };
    let answerOlder!: (page: Page) => void;
    let answerNewer!: (page: Page) => void;
    setBindingMock('ListItemsBeforeCursor', () => new Promise<Page>((resolve) => { answerOlder = resolve; }));
    setBindingMock('ListItemsAfterCursor', () => new Promise<Page>((resolve) => { answerNewer = resolve; }));
    const paging = createTimelinePaging({
      getPane: () => pane,
      stick: { pauseAutoScroll: () => () => {}, markEscaped: () => {}, forceStick: () => {} } as unknown as UseStickToBottomController,
      getListRef: () => ({ scrollToIndex: () => {}, getScrollOffset: () => 0, findItemIndex: () => 0 }) as unknown as TimelineVirtualizerHandle,
      getScrollEl: () => ({ scrollHeight: 300, clientHeight: 600 }) as HTMLDivElement,
      getRevealedNodes: () => [],
      isWindowMeasured: () => true,
      getRestoredThreadId: () => null,
      beginNavigation: () => 1,
      claimNavigation: () => true,
      saveScrollSnapshot: () => {},
    });

    expect(paging.maybeFillViewport()).toBe(true);
    expect(pane.loadingOlder).toBe(true);
    expect(paging.maybeFillViewport()).toBe(true);
    expect(pane.loadingNewer).toBe(true);

    answerOlder({ items: [row(9)], hasMore: true, hasMoreOlder: true, hasMoreNewer: false });
    answerNewer({ items: [row(12)], hasMore: true, hasMoreOlder: true, hasMoreNewer: true });
    await vi.waitFor(() => expect(pane.loadingOlder || pane.loadingNewer).toBe(false));
    expect(pane.items.map((item) => item.id)).toEqual(['t9', 't10', 't11', 't12']);
  });
});
