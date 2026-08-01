// Unit coverage for the anchored timeline-height transactions. The
// physics-visible outcome (glide vs snap) lives in the real-Chromium
// suites; these lock the DECISIONS: which restore path a prune takes,
// and that the sticking-to-bottom restore stands down for an in-flight
// auto-scroll instead of writing the bottom over a mid-glide spring
// (bug-report-20260801T214455Z — the recent-window prune landing
// mid-chase collapsed the spring's remaining distance into an instant
// one-line hop).

import { describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import { createTimelineWindowAnchor } from './timelineWindowAnchor.svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { UseStickToBottomController } from '../../utils/scroll/index.svelte';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import type { TimelineNode } from '../../utils/subagentGrouping';
import type { Item } from '../../types/models';

function leaf(id: string): TimelineNode {
  return { kind: 'leaf', item: { threadId: 't1', id } as Item } as TimelineNode;
}

function makeHarness({ autoScrollInFlight }: { autoScrollInFlight: boolean }) {
  const scrollToIndex = vi.fn();
  const observe = vi.fn();
  const markAtBottom = vi.fn();
  const saveScrollSnapshot = vi.fn();
  const release = vi.fn();
  const stick = {
    isSticky: true,
    escapedFromLock: false,
    isAtBottom: true,
    pauseAutoScroll: vi.fn(() => release),
    observe,
    markAtBottom,
    autoScrollInFlight: () => autoScrollInFlight,
  } as unknown as UseStickToBottomController;

  const nodes = [leaf('a'), leaf('b'), leaf('c')];
  let token = 0;
  const anchor = createTimelineWindowAnchor({
    getPane: () => ({ switchGeneration: 1 }) as ThreadPane,
    stick,
    getListRef: () =>
      ({ scrollToIndex, getScrollOffset: () => 0 }) as unknown as TimelineVirtualizerHandle,
    getScrollEl: () => document.createElement('div'),
    getRevealedNodes: () => nodes,
    findTimelineNodeIndex: () => 0,
    saveScrollSnapshot,
    nextRestoreToken: () => ++token,
    isRestoreTokenCurrent: (t) => t === token,
  });
  return { anchor, scrollToIndex, observe, markAtBottom, saveScrollSnapshot, release };
}

async function settleRestore(): Promise<void> {
  await tick();
  await Promise.resolve();
}

describe('preserveTimelineWindowAnchor — sticking-to-bottom restore', () => {
  it('yields to an in-flight auto-scroll: no bottom write, live-content observe instead', async () => {
    const h = makeHarness({ autoScrollInFlight: true });
    const applied = h.anchor.preserveTimelineWindowAnchor({
      run: () => {},
      keepsItem: () => true,
    });
    expect(applied).toBe(true);
    await settleRestore();

    // The mid-glide spring owns the trip to the bottom; the restore must
    // not collapse its remaining distance into a direct write.
    expect(h.scrollToIndex).not.toHaveBeenCalled();
    expect(h.markAtBottom).not.toHaveBeenCalled();
    expect(h.observe).toHaveBeenCalledWith('live-content');
    expect(h.saveScrollSnapshot).toHaveBeenCalled();
    expect(h.release).toHaveBeenCalled();
  });

  it('restores the bottom edge directly when no auto-scroll is in flight', async () => {
    const h = makeHarness({ autoScrollInFlight: false });
    const applied = h.anchor.preserveTimelineWindowAnchor({
      run: () => {},
      keepsItem: () => true,
    });
    expect(applied).toBe(true);
    await settleRestore();

    expect(h.scrollToIndex).toHaveBeenCalledWith(2, { align: 'end' });
    expect(h.markAtBottom).toHaveBeenCalled();
    expect(h.saveScrollSnapshot).toHaveBeenCalled();
    expect(h.observe).not.toHaveBeenCalled();
    expect(h.release).toHaveBeenCalled();
  });
});
