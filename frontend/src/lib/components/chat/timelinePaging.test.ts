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
}) {
  const loadOlder = vi.fn(async () => ({ status: 'loaded' as const, insertedBeforeWindow: true, insertedRows: true }));
  const loadNewer = vi.fn(async () => ({ status: 'loaded' as const, insertedBeforeWindow: false, insertedRows: true }));
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
    setEscapedFromLock: () => {},
    forceStick: () => {},
  } as unknown as UseStickToBottomController;
  const viewport = {
    scrollHeight: over.scrollHeight,
    clientHeight: over.clientHeight ?? 600,
  } as HTMLDivElement;
  const paging = createTimelinePaging({
    getPane: () => pane,
    stick,
    getListRef: () => (over.listRef === false ? undefined : ({} as TimelineVirtualizerHandle)),
    getScrollEl: () => viewport,
    getRevealedNodes: () => [],
    getRestoredThreadId: () => null,
    nextRestoreToken: () => 1,
    isRestoreTokenCurrent: () => true,
    saveScrollSnapshot: () => {},
  });
  return { paging, loadOlder, loadNewer };
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
