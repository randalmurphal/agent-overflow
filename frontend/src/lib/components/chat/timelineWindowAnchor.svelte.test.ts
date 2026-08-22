// Unit coverage for the anchored timeline-height transactions. The
// physics-visible outcome (glide vs snap) lives in the real-Chromium
// suites, and the takeover arbitration itself is the controller's
// (utils/scroll/index.svelte.test.ts). These lock the DECISIONS this
// module makes: which takeover priority each transaction's bottom
// restore requests — the unasked prune always yields
// (bug-report-20260801T214455Z — a claimed bottom landing mid-chase
// collapsed the spring's remaining distance into an instant one-line
// hop), viewport-bottom transactions carry their caller's priority —
// and that the virtualized placement rides the `write` callback. They
// also lock the release timing: a viewport-bottom transaction's pause
// outlives the measurement flush (two rAFs past the restore), so the
// release repin can never hand the toggle's still-measuring height
// delta to an engaged spring (bug-report-20260802T011749Z — the
// 1px-at-a-time crawl whose glide residue resampled all pane text).

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

function makeHarness({
  autoScrollInFlight,
  holdingBottom = true,
}: {
  autoScrollInFlight: boolean;
  holdingBottom?: boolean;
}) {
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
    isSticky: holdingBottom,
    escapedFromLock: false,
    isAtBottom: holdingBottom,
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
      ({
        scrollToIndex,
        getScrollOffset: () => 0,
        findItemIndex: () => 0,
        getItemOffset: () => 0,
      }) as unknown as TimelineVirtualizerHandle,
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

// The viewport-bottom release is deferred past the measurement flush
// (two rAFs). This helper's own two rAFs are scheduled strictly after
// the module's, so by the time they fire the deferred release has run;
// the trailing microtask drains the resolve continuation.
async function settleMeasurementFlush(): Promise<void> {
  await new Promise<void>((resolve) => {
    requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
  });
  await Promise.resolve();
}

describe('canPreserveTimelineWindow', () => {
  it('accepts every retention plan while the reader holds the bottom', () => {
    const h = makeHarness({ autoScrollInFlight: false });
    const keepsItem = vi.fn(() => false);

    expect(h.anchor.canPreserveTimelineWindow(keepsItem)).toBe(true);
    expect(keepsItem).not.toHaveBeenCalled();
  });

  it('requires a scrolled-up reader\'s visible anchor to survive', () => {
    const h = makeHarness({ autoScrollInFlight: false, holdingBottom: false });
    const keepsItem = vi.fn((itemId: string) => itemId === 'a');

    expect(h.anchor.canPreserveTimelineWindow(keepsItem)).toBe(true);
    expect(keepsItem).toHaveBeenCalledWith('a');
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
    await settleMeasurementFlush();
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
    await settleMeasurementFlush();
    expect(h.release).toHaveBeenCalled();
  });
});

describe('preserveViewportBottom — release timing', () => {
  it('holds the pause past the measurement flush so the release repin cannot hand the toggle delta to an engaged spring', async () => {
    const h = makeHarness({ autoScrollInFlight: true });
    h.anchor.preserveViewportBottom(() => {});
    await settleRestore();

    // The restore has run — the claim placed the bottom edge — but the
    // toggled run's height has not measured yet: it lands one rendering
    // update later via the virtualizer's ResizeObserver, and the
    // convergence pass places the delta in that same update. Releasing
    // here would let the release repin's yield hand that delta to the
    // engaged spring, whose first tick kills the pending index scroll
    // (bug-report-20260802T011749Z: 500-2000ms crawls starting ≤6ms
    // after each toggle while a turn streamed).
    expect(h.scrollToIndex).toHaveBeenCalled();
    expect(h.release).not.toHaveBeenCalled();

    await settleMeasurementFlush();
    expect(h.release).toHaveBeenCalled();
  });

  it('a change() throw releases the pause synchronously', () => {
    const h = makeHarness({ autoScrollInFlight: false });
    expect(() =>
      h.anchor.preserveViewportBottom(() => {
        throw new Error('boom');
      }),
    ).toThrow('boom');
    expect(h.release).toHaveBeenCalled();
  });
});
