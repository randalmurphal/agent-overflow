// Direct unit coverage for createTimelineSizePriors, driven with a fake
// TimelineVirtualizerHandle instead of a real component mount: happy-dom's
// ResizeObserver is stubbed to a no-op (setup.ts), so a real
// <MessageTimeline> render never delivers a measurement and can't prove
// row-level replay (scroll.test.ts's priors block covers wiring only).
// Faking the handle lets these tests assert the actual per-row resolution
// the redesign exists for.
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { ThreadPane } from '../../stores/thread.svelte';
import { makeItem } from '../../../test/helpers/chat';
import { clearAllThreadSizePriorsForTest, setSizePriorsStorageAdapter } from '../../utils/virtual/priors';
import {
  __resetSizePriorsStorageForTest,
  installSizePriorsPersistence,
} from '../../utils/virtual/priorsStorage';
import type { TimelineNode } from '../../utils/subagentGrouping';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import { createTimelineSizePriors } from './timelineSizePriors.svelte';

function leaf(id: string, overrides: Partial<Parameters<typeof makeItem>[0]> = {}): TimelineNode {
  return { kind: 'leaf', item: makeItem({ id, ...overrides }) };
}

function fakePane(threadId: string, expansionSig = ''): ThreadPane {
  // Only `.threadId` and `.expansionSignature()` are read by
  // timelineSizePriors.svelte.ts — a full ThreadPane is not needed.
  return {
    threadId,
    expansionSignature: () => expansionSig,
  } as unknown as ThreadPane;
}

function fakeListRef(sizes: number[]): TimelineVirtualizerHandle {
  return {
    scrollToIndex: () => {},
    revalidate: () => {},
    getScrollOffset: () => 0,
    getViewportSize: () => 0,
    getScrollSize: () => 0,
    getTotalSize: () => sizes.reduce((sum, size) => sum + Math.max(size, 0), 0),
    findItemIndex: () => -1,
    getItemOffset: () => 0,
    sizeAt: (index) => sizes[index],
    isMeasuredAt: (index) => sizes[index] >= 0,
    takeSnapshot: () => sizes.slice(),
  };
}

beforeEach(() => {
  clearAllThreadSizePriorsForTest();
  setSizePriorsStorageAdapter(undefined);
});

describe('createTimelineSizePriors', () => {
  // THE HEADLINE TEST. Fails against the pre-change (positional,
  // whole-window-structureSig-keyed) implementation: that design's
  // `getReplayableSizePriors` required the mount's ENTIRE joined
  // structure signature to equal the captured one, and a 12-row suffix
  // of a 30-row capture produces a completely different joined string
  // (different length, different leading rows) — an unconditional key
  // miss, so every row degraded to the kind/flat estimate. Verified by
  // reasoning about `timelineStructureSignature`'s whole-window join
  // (utils/timelineStructureSignature.ts, pre-change) rather than a
  // stash-run: the old API this test exercises (`getReplayableSizePriors`,
  // positional `sizes: number[]`) no longer exists, so the test itself
  // cannot compile against the prior module shape.
  it('resolves a suffix window against a larger captured window (window-composition fix)', () => {
    const threadId = 'thread-suffix';
    const capturedNodes: TimelineNode[] = Array.from({ length: 30 }, (_, i) =>
      leaf(`item-${i}`, { summary: `body ${i}`, status: 'completed', updatedAt: i }),
    );
    const capturedSizes = capturedNodes.map((_, i) => 50 + i);

    let nodes = capturedNodes;
    let listRef: TimelineVirtualizerHandle = fakeListRef(capturedSizes);
    const pane = fakePane(threadId);
    const priors = createTimelineSizePriors({
      getPane: () => pane,
      getListRef: () => listRef,
      getRevealedNodes: () => nodes,
      getScrollSurfaceContentWidth: () => 800,
      getRestoredThreadId: () => threadId,
    });

    // A large in-session window (30 rows) settles and captures.
    priors.maybePersistSizePriors();

    // A fresh app boot instead loads only a SUFFIX window: the last 12
    // of those 30 rows (a boot always starts from a small initial slice,
    // never the full session window).
    const suffixNodes = capturedNodes.slice(18);
    nodes = suffixNodes;
    listRef = fakeListRef(suffixNodes.map(() => -1)); // nothing measured yet this mount

    priors.resolveRowEstimateOnThreadEdge(threadId);
    const estimate = priors.rowEstimate;
    expect(estimate).toBeDefined();

    // Every suffix row resolves to its ORIGINAL captured height, not a
    // kind/flat estimate.
    for (let i = 0; i < suffixNodes.length; i++) {
      expect(estimate!.at(i)).toBe(capturedSizes[18 + i]);
    }
  });

  it('defers the width/expansion validity check to the first at() call (lazy-once)', () => {
    const threadId = 'thread-lazy';
    const nodes: TimelineNode[] = [leaf('a', { summary: 'hi' })]; // default kind: assistant_text
    let currentWidth = 800;
    const listRef = fakeListRef([120]);
    const pane = fakePane(threadId);
    const priors = createTimelineSizePriors({
      getPane: () => pane,
      getListRef: () => listRef,
      getRevealedNodes: () => nodes,
      getScrollSurfaceContentWidth: () => currentWidth,
      getRestoredThreadId: () => threadId,
    });
    priors.maybePersistSizePriors(); // captured at width 800

    // Mirrors the real app-boot ordering: resolveRowEstimateOnThreadEdge
    // runs in $effect.pre before the scroll surface is laid out, so width
    // reads 0 here. Must not eagerly refuse the entry.
    currentWidth = 0;
    priors.resolveRowEstimateOnThreadEdge(threadId);

    // Layout has now happened, but the real width (640) mismatches the
    // captured width (800) — the FIRST at() call detects this and
    // refuses the whole entry, falling back to the kind estimate.
    currentWidth = 640;
    expect(priors.rowEstimate!.at(0)).toBe(44); // ROW_KIND_ESTIMATE_PX.assistant_text

    // The check is memoized after the first call: even if width now
    // coincidentally matches the capture, the row stays refused for the
    // rest of this mount.
    currentWidth = 800;
    expect(priors.rowEstimate!.at(0)).toBe(44);
  });

  it('trusts the captured width when the surface has not reported one yet (width 0 at first at())', () => {
    // The engine's first at() calls run synchronously when the virtualizer
    // mounts with data, and the width signal is RO-only (async) — on boot,
    // whichever lands first is a machine-speed race. Width 0 means "layout
    // hasn't reported yet", not a real wrap point, so the entry must be
    // trusted, not refused. Fails against a memo that latches
    // `0 !== capturedWidth` as a mismatch.
    const threadId = 'thread-boot-race';
    const nodes: TimelineNode[] = [leaf('a', { summary: 'hi' })];
    let currentWidth = 800;
    const listRef = fakeListRef([120]);
    const pane = fakePane(threadId);
    const priors = createTimelineSizePriors({
      getPane: () => pane,
      getListRef: () => listRef,
      getRevealedNodes: () => nodes,
      getScrollSurfaceContentWidth: () => currentWidth,
      getRestoredThreadId: () => threadId,
    });
    priors.maybePersistSizePriors(); // captured at width 800

    currentWidth = 0;
    priors.resolveRowEstimateOnThreadEdge(threadId);

    // First at() still sees width 0 (RO hasn't delivered) — the prior
    // replays on trust instead of degrading to the kind estimate.
    expect(priors.rowEstimate!.at(0)).toBe(120);
    expect(priors.replayStats().validity).toBe('replayed-trusted-width');

    // The trust decision is latched for the mount: a later at() call
    // seeing a genuinely mismatched width does not retroactively refuse
    // (a mid-mount flip would resolve rows inconsistently; a real
    // mismatch self-corrects via per-row RO under the warm gate).
    currentWidth = 640;
    expect(priors.rowEstimate!.at(0)).toBe(120);
  });

  it('reports replayStats for the trace: source, validity, and resolved-row volume', () => {
    const threadId = 'thread-stats';
    const nodes: TimelineNode[] = [leaf('a', { summary: 'hi' }), leaf('b', { summary: 'yo' })];
    let currentWidth = 800;
    const listRef = fakeListRef([120, 90]);
    const pane = fakePane(threadId);
    const priors = createTimelineSizePriors({
      getPane: () => pane,
      getListRef: () => listRef,
      getRevealedNodes: () => nodes,
      getScrollSurfaceContentWidth: () => currentWidth,
      getRestoredThreadId: () => threadId,
    });

    // No entry captured yet → no-entry.
    priors.resolveRowEstimateOnThreadEdge(threadId);
    expect(priors.replayStats()).toEqual({
      source: 'none',
      validity: 'no-entry',
      rowsResolved: 0,
    });

    priors.maybePersistSizePriors();

    // Re-resolve (new mount of the same thread): entry found in memory,
    // validity pending until the first at() call runs the lazy-once check.
    priors.resolveRowEstimateOnThreadEdge(null);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    expect(priors.replayStats().source).toBe('memory');
    expect(priors.replayStats().validity).toBe('pending');

    priors.rowEstimate!.at(0);
    priors.rowEstimate!.at(1);
    expect(priors.replayStats().validity).toBe('replayed');
    expect(priors.replayStats().rowsResolved).toBe(2);

    // Width mismatch on yet another mount → refused, zero rows resolved.
    priors.resolveRowEstimateOnThreadEdge(null);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    currentWidth = 640;
    priors.rowEstimate!.at(0);
    expect(priors.replayStats().validity).toBe('width-mismatch');
    expect(priors.replayStats().rowsResolved).toBe(0);
  });

  it('resolves from localStorage after an in-memory clear (restart simulation)', () => {
    vi.useFakeTimers();
    __resetSizePriorsStorageForTest();
    installSizePriorsPersistence();

    const threadId = 'thread-restart';
    const nodes: TimelineNode[] = [leaf('a', { summary: 'hi' })];
    const listRef = fakeListRef([120]);
    const pane = fakePane(threadId);
    const priors = createTimelineSizePriors({
      getPane: () => pane,
      getListRef: () => listRef,
      getRevealedNodes: () => nodes,
      getScrollSurfaceContentWidth: () => 800,
      getRestoredThreadId: () => threadId,
    });

    priors.maybePersistSizePriors();
    vi.advanceTimersByTime(1000); // flush the debounced write to localStorage

    // Simulate an app restart: wipe the in-memory LRU only, keep localStorage.
    clearAllThreadSizePriorsForTest();

    priors.resolveRowEstimateOnThreadEdge(threadId);
    expect(priors.rowEstimate!.at(0)).toBe(120);

    __resetSizePriorsStorageForTest();
    vi.useRealTimers();
  });
});
