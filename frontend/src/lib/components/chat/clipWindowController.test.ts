import { expect, it, vi } from 'vitest';
import { createClipWindowController } from './clipWindowController';
import type { UseStickToBottomController } from '../../utils/scroll/index.svelte';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';

it('retains the visible reader range and releases it when following resumes', () => {
  let atBottom = false;
  let escaped = true;
  let offset = 20;
  const stick = {
    get isAtBottom() { return atBottom; }, get escapedFromLock() { return escaped; },
    observe: vi.fn(), pauseAutoScroll: vi.fn(), autoScrollInFlight: vi.fn(),
    markStructuralContentPending: vi.fn(), armWarmup: vi.fn(), preserveScrollAnchor: vi.fn(),
  } as unknown as UseStickToBottomController;
  const list = { getScrollOffset: () => offset, getViewportSize: () => 30,
    findItemIndex: (position: number) => Math.floor(position / 10) } as TimelineVirtualizerHandle;
  const nodes = Array.from({ length: 100 }, (_, i) => [`item-${i}`, `child-${i}`]);
  const controller = createClipWindowController({ stick, list, nodes: () => nodes, itemIds: node => node });
  expect([...controller.visibleTimelineItemIds!()!]).toEqual(['item-2', 'child-2', 'item-3', 'child-3', 'item-4', 'child-4']);
  expect(controller.canPreserveTimelineWindow!(id => id !== 'child-3')).toBe(false);
  expect(controller.canPreserveTimelineWindow!(id => id !== 'item-90')).toBe(true);
  offset = 40;
  expect(controller.canPreserveTimelineWindow!(id => id !== 'child-3')).toBe(true);
  atBottom = true; escaped = false;
  expect(controller.visibleTimelineItemIds!()).toBeNull();
  expect(controller.canPreserveTimelineWindow!(() => false)).toBe(true);
  escaped = true;
  expect(controller.canPreserveTimelineWindow!(() => false)).toBe(false);
  controller.observe('content');
  expect(stick.observe).toHaveBeenCalledExactlyOnceWith('content');
});
