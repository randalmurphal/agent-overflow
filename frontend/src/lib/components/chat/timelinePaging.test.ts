// The height-driven fill (docs/architecture/frontend-scroll.md, Live Window
// Bounds): a window whose rows collapse into a few activity runs can be
// shorter than the viewport plus both auto-load zones, where no scroll
// offset ever reaches a trigger. The quiet scheduler asks this once per
// pass and it pages one section at a time until the window is tall enough.
import { describe, expect, it, vi } from 'vitest';
import { createTimelinePaging, type TimelinePagingOptions } from './timelinePaging';
import { ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS } from '../../stores/threadPaneShared';
import type { UseStickToBottomController } from '../../utils/scroll/index.svelte';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';

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
}) {
  const loadOlder = vi.fn(async () => ({ status: 'loaded' as const, insertedBeforeWindow: true, insertedRows: true }));
  const loadNewer = vi.fn(async () => ({ status: 'loaded' as const, insertedBeforeWindow: false, insertedRows: true }));
  const markEscaped = vi.fn();
  const pane = {
    items: Array.from({ length: over.items ?? 10 }, () => ({})),
    hasMoreHistory: over.hasMoreHistory ?? true,
    hasMoreNewer: over.hasMoreNewer ?? false,
    loadingOlder: over.loadingOlder ?? false,
    loadingNewer: over.loadingNewer ?? false,
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
      over.listRef === false ? undefined : ({ scrollToIndex: () => {} } as unknown as TimelineVirtualizerHandle),
    getScrollEl: () => viewport,
    getRevealedNodes: () => Array.from({ length: over.revealed ?? 0 }, () => ({})) as never[],
    getRestoredThreadId: () => null,
    nextRestoreToken: () => 1,
    isRestoreTokenCurrent: () => true,
    saveScrollSnapshot: () => {},
  });
  return { paging, loadOlder, loadNewer, markEscaped };
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
