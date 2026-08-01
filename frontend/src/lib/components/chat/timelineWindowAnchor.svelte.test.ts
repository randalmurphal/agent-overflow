// Unit coverage for the anchored timeline-height transactions. The
// physics-visible outcome (glide vs snap) lives in the real-Chromium
// suites, and the takeover arbitration itself is the controller's
// (utils/scroll/index.svelte.test.ts). These lock the DECISIONS this
// module makes: which takeover priority each transaction's bottom
// restore requests — the unasked prune always yields
// (bug-report-20260801T214455Z — a claimed bottom landing mid-chase
// collapsed the spring's remaining distance into an instant one-line
// hop), viewport-bottom transactions carry their caller's priority —
// and that the virtualized placement rides the `write` callback.

import { describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import { createTimelineWindowAnchor } from './timelineWindowAnchor.svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import type {
  RequestBottomOptions,
  UseStickToBottomController,
} from '../../utils/scroll/index.svelte';
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
  // Faithful to the controller contract this module programs against:
  // a 'yield' while the program is engaged hands off to the
  // live-content path; every other resolution places via the caller's
  // `write`. The arbitration itself is controller-owned and tested
  // there — this stub only mirrors its observable shape.
  const requestBottom = vi.fn((opts: RequestBottomOptions) => {
    if (opts.takeover === 'yield' && autoScrollInFlight) {
      observe('live-content');
      return;
    }
    opts.write?.();
  });
  const stick = {
    isSticky: true,
    escapedFromLock: false,
    isAtBottom: true,
    pauseAutoScroll: vi.fn(() => release),
    observe,
    markAtBottom,
    requestBottom,
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
  return {
    anchor,
    scrollToIndex,
    observe,
    markAtBottom,
    requestBottom,
    saveScrollSnapshot,
    release,
  };
}

async function settleRestore(): Promise<void> {
  await tick();
  await Promise.resolve();
}

describe('preserveTimelineWindowAnchor — sticking-to-bottom restore', () => {
  it('requests the bottom as a yield: an in-flight auto-scroll keeps the trip', async () => {
    const h = makeHarness({ autoScrollInFlight: true });
    const applied = h.anchor.preserveTimelineWindowAnchor({
      run: () => {},
      keepsItem: () => true,
    });
    expect(applied).toBe(true);
    await settleRestore();

    // The prune is unasked, so its restore must never claim the bottom
    // over a mid-glide spring — the yield hands the fresh geometry to
    // the live-content path and writes nothing.
    expect(h.requestBottom).toHaveBeenCalledWith(
      expect.objectContaining({ takeover: 'yield' }),
    );
    expect(h.scrollToIndex).not.toHaveBeenCalled();
    expect(h.markAtBottom).not.toHaveBeenCalled();
    expect(h.observe).toHaveBeenCalledWith('live-content');
    expect(h.saveScrollSnapshot).toHaveBeenCalled();
    expect(h.release).toHaveBeenCalled();
  });

  it('places the bottom edge through the write callback when no auto-scroll is in flight', async () => {
    const h = makeHarness({ autoScrollInFlight: false });
    const applied = h.anchor.preserveTimelineWindowAnchor({
      run: () => {},
      keepsItem: () => true,
    });
    expect(applied).toBe(true);
    await settleRestore();

    expect(h.requestBottom).toHaveBeenCalledWith(
      expect.objectContaining({ takeover: 'yield' }),
    );
    expect(h.scrollToIndex).toHaveBeenCalledWith(2, { align: 'end' });
    expect(h.markAtBottom).toHaveBeenCalled();
    expect(h.saveScrollSnapshot).toHaveBeenCalled();
    expect(h.observe).not.toHaveBeenCalled();
    expect(h.release).toHaveBeenCalled();
  });
});

describe('preserveViewportBottom — takeover priority', () => {
  it("an unasked transaction's yield stands down for the engaged program", async () => {
    const h = makeHarness({ autoScrollInFlight: true });
    h.anchor.preserveViewportBottom(() => {}, { takeover: 'yield' });
    await settleRestore();

    expect(h.requestBottom).toHaveBeenCalledWith(
      expect.objectContaining({ takeover: 'yield' }),
    );
    expect(h.scrollToIndex).not.toHaveBeenCalled();
    expect(h.observe).toHaveBeenCalledWith('live-content');
    expect(h.saveScrollSnapshot).toHaveBeenCalled();
    expect(h.release).toHaveBeenCalled();
  });

  it('a reader-asked transaction claims the bottom even mid-program', async () => {
    const h = makeHarness({ autoScrollInFlight: true });
    h.anchor.preserveViewportBottom(() => {});
    await settleRestore();

    // Default takeover is 'claim': the reader clicked this height
    // change, and user intent always may retarget — the clicked delta
    // never animates.
    expect(h.requestBottom).toHaveBeenCalledWith(
      expect.objectContaining({ takeover: 'claim' }),
    );
    expect(h.scrollToIndex).toHaveBeenCalledWith(2, { align: 'end' });
    expect(h.markAtBottom).toHaveBeenCalled();
    expect(h.saveScrollSnapshot).toHaveBeenCalled();
    expect(h.release).toHaveBeenCalled();
  });
});
