// Direct unit coverage for createTimelineSizePriors, driven with a fake
import { wholeRunNodeFields } from '../../../test/helpers/activityRuns';
// TimelineVirtualizerHandle instead of a real component mount: happy-dom's
// ResizeObserver is stubbed to a no-op (setup.ts), so a real
// <MessageTimeline> render never delivers a measurement and can't prove
// row-level replay (scroll.test.ts's priors block covers wiring only).
// Faking the handle lets these tests assert the actual per-row resolution
// the redesign exists for.
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { Item } from '../../types/models';
import { makeItem } from '../../../test/helpers/chat';
import {
  clearAllThreadSizePriorsForTest,
  getThreadSizePriors,
  setSizePriorsStorageAdapter,
  sizePriorsAtGeometry,
} from '../../utils/virtual/priors';
import {
  __resetSizePriorsStorageForTest,
  installSizePriorsPersistence,
} from '../../utils/virtual/priorsStorage';
import type { TimelineNode } from '../../utils/subagentGrouping';
import { nodeSignature } from '../../utils/timelineStructureSignature';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import { createTimelineSizePriors } from './timelineSizePriors.svelte';

// Stand-in for `typographySignature()` (stores/settings.svelte.ts). The
// module only ever compares it, so a literal is enough; the tests that
// care about a typography change reassign this between mounts.
const DEFAULT_TYPOGRAPHY = 'f15/sgeist/mgeist/c1/w1';
let typography = DEFAULT_TYPOGRAPHY;

function leaf(id: string, overrides: Partial<Parameters<typeof makeItem>[0]> = {}): TimelineNode {
  return { kind: 'leaf', item: makeItem({ id, ...overrides }) };
}

function fakePane(
  threadId: string,
  expansionSig = '',
  rowsById: ReadonlyMap<string, Item> = new Map(),
): ThreadPane {
  // Only `.threadId`, `.scrollStateKey`, `.expansionSignature()` and
  // `.getItemById()` are read by timelineSizePriors.svelte.ts — a full
  // ThreadPane is not needed. scrollStateKey mirrors the production default
  // (the stable thread id); the agent-pane facade is what diverges it.
  // `rowsById` is the store's current row per id: empty means every node's
  // `item` IS the store row, the steady state after a structural pass.
  return {
    threadId,
    scrollStateKey: threadId,
    expansionSignature: () => expansionSig,
    getItemById: (itemId: string) => rowsById.get(itemId),
  } as unknown as ThreadPane;
}

function fakeListRef(sizes: number[]): TimelineVirtualizerHandle {
  return {
    scrollToIndex: () => {},
    revalidate: () => {},
    measureMountedRows: async () => {},
    subscribeContentGeometry: () => () => {},
    noteScrollTopWritten: () => {},
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
  typography = DEFAULT_TYPOGRAPHY;
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
      getTypographySignature: () => typography,
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

  it('captures a settled row under the signature the reopen looks up, not the stale node item', () => {
    // A streaming row settling (status, summary, updatedAt) is not a
    // structural change, so the projection keeps the node minted for the
    // streaming-era Item while the store, and the row on screen, hold the
    // settled one. The switch-away capture must store the settled height
    // under the settled signature: the reopen rebuilds its nodes from the
    // store and would otherwise look up a key that was never written and
    // estimate from the kind floor.
    const threadId = 'thread-settle';
    const streaming = makeItem({ id: 'a', summary: '', status: 'streaming', updatedAt: 1 });
    const settled = makeItem({ id: 'a', summary: 'a settled answer', status: 'completed', updatedAt: 2 });
    let nodes: TimelineNode[] = [{ kind: 'leaf', item: streaming }];
    let listRef = fakeListRef([121]);
    let pane = fakePane(threadId, '', new Map([['a', settled]]));
    const priors = createTimelineSizePriors({
      getPane: () => pane,
      getListRef: () => listRef,
      getRevealedNodes: () => nodes,
      getScrollSurfaceContentWidth: () => 800,
      getTypographySignature: () => typography,
      getRestoredThreadId: () => threadId,
    });
    priors.persistSizePriorsFinal();

    // Reopen: a fresh projection from the store, nothing measured yet.
    nodes = [{ kind: 'leaf', item: settled }];
    listRef = fakeListRef([-1]);
    pane = fakePane(threadId);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    expect(priors.rowEstimate!.at(0)).toBe(121);
  });

  it('resolves a run captured open, and reopened closed, from the bucket\'s closed-run height', () => {
    // The tail run of a finished turn stays open at switch-away (its
    // `openedLive` hold) and comes back closed on the reopen, since the hold
    // dies with the registry. Every closed run in a bucket measures the same
    // px, so the run resolves from any closed run the capture measured.
    const threadId = 'thread-run-shape';
    const run = (id: string, collapsed: boolean): TimelineNode => ({
      kind: 'activity_run',
      runId: `run-${id}`,
      threadId,
      children: [leaf(id)],
      mountedFrom: 0,
      mountedRows: 1,
      membershipEpoch: 1,
      memberItemIds: [id],
      summaryItemIds: [id],
      ...wholeRunNodeFields([id]),
      collapsed,
      live: false,
      atTail: false,
    });
    let nodes: TimelineNode[] = [run('early', true), run('tail', false)];
    let listRef = fakeListRef([38, 69.5]);
    const pane = fakePane(threadId);
    const priors = createTimelineSizePriors({
      getPane: () => pane,
      getListRef: () => listRef,
      getRevealedNodes: () => nodes,
      getScrollSurfaceContentWidth: () => 800,
      getTypographySignature: () => typography,
      getRestoredThreadId: () => threadId,
    });
    priors.persistSizePriorsFinal();

    nodes = [run('early', true), run('tail', true)];
    listRef = fakeListRef([-1, -1]);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    expect(priors.rowEstimate!.at(0)).toBe(38);
    expect(priors.rowEstimate!.at(1)).toBe(38);
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
      getTypographySignature: () => typography,
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
      getTypographySignature: () => typography,
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
      getTypographySignature: () => typography,
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
    expect(priors.replayStats().validity).toBe('geometry-mismatch');
    expect(priors.replayStats().rowsResolved).toBe(0);
  });

  // Regression battery for the restore-time capture destroying settled
  // priors (the coldload trace's memory/replayed/rowsResolved:0 signature).
  // timelineRestore's restoreToBottom/restoreAnchor call saveScrollSnapshot
  // — and through it maybePersistSizePriors — synchronously at restore
  // time, when the freshly remounted engine has measured nothing and the
  // size-gate was just reset on the thread edge. Pre-fix, that persisted an
  // empty rows map over the thread's settled entry.
  it('does not destroy a settled entry when a restore-time capture has measured nothing', () => {
    const threadId = 'thread-restore-wipe';
    const nodes: TimelineNode[] = Array.from({ length: 8 }, (_, i) =>
      leaf(`item-${i}`, { summary: `body ${i}`, status: 'completed', updatedAt: i }),
    );
    const settledSizes = nodes.map((_, i) => 60 + i);
    let listRef = fakeListRef(settledSizes);
    const pane = fakePane(threadId);
    const priors = createTimelineSizePriors({
      getPane: () => pane,
      getListRef: () => listRef,
      getRevealedNodes: () => nodes,
      getScrollSurfaceContentWidth: () => 800,
      getTypographySignature: () => typography,
      getRestoredThreadId: () => threadId,
    });
    priors.maybePersistSizePriors(); // the settled capture

    // Remount of the same thread: the size-gate resets on the thread edge,
    // and restore fires a capture before any row has re-measured.
    priors.resolveRowEstimateOnThreadEdge(null);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    listRef = fakeListRef(nodes.map(() => -1));
    priors.maybePersistSizePriors(); // restore-time capture, nothing measured

    // A later mount must still resolve every settled size.
    priors.resolveRowEstimateOnThreadEdge(null);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    for (let i = 0; i < nodes.length; i++) {
      expect(priors.rowEstimate!.at(i)).toBe(settledSizes[i]);
    }
    expect(priors.replayStats().rowsResolved).toBe(nodes.length);
  });

  it('carries settled sizes through a partial mid-cascade capture, dropping changed rows', () => {
    const threadId = 'thread-partial';
    let nodes: TimelineNode[] = Array.from({ length: 6 }, (_, i) =>
      leaf(`item-${i}`, { summary: `body ${i}`, status: 'completed', updatedAt: i }),
    );
    const settledSizes = [50, 51, 52, 53, 54, 55];
    let listRef = fakeListRef(settledSizes);
    const pane = fakePane(threadId);
    const priors = createTimelineSizePriors({
      getPane: () => pane,
      getListRef: () => listRef,
      getRevealedNodes: () => nodes,
      getScrollSurfaceContentWidth: () => 800,
      getTypographySignature: () => typography,
      getRestoredThreadId: () => threadId,
    });
    priors.maybePersistSizePriors();

    // Remount: only rows 0-1 have re-measured (mid-cascade scrollend
    // capture), and item-5's content changed since the settled capture —
    // its old signature is no longer live, so its stale size must drop.
    nodes = [
      ...nodes.slice(0, 5),
      leaf('item-5', { summary: 'body 5 grew longer', status: 'completed', updatedAt: 99 }),
    ];
    priors.resolveRowEstimateOnThreadEdge(null);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    listRef = fakeListRef([70, 71, -1, -1, -1, -1]);
    priors.maybePersistSizePriors();

    priors.resolveRowEstimateOnThreadEdge(null);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    const estimate = priors.rowEstimate!;
    expect(estimate.at(0)).toBe(70); // fresh measurement wins
    expect(estimate.at(1)).toBe(71);
    expect(estimate.at(2)).toBe(52); // carried forward from the settled capture
    expect(estimate.at(4)).toBe(54);
    expect(estimate.at(5)).toBe(44); // changed signature: stale size dropped → kind estimate
  });

  it('carries forward within a width bucket and keeps the other width\'s bucket', () => {
    const threadId = 'thread-width-carry';
    const nodes: TimelineNode[] = Array.from({ length: 4 }, (_, i) =>
      leaf(`item-${i}`, { summary: `body ${i}`, status: 'completed', updatedAt: i }),
    );
    let currentWidth = 800;
    let listRef = fakeListRef([90, 91, 92, 93]);
    const pane = fakePane(threadId);
    const priors = createTimelineSizePriors({
      getPane: () => pane,
      getListRef: () => listRef,
      getRevealedNodes: () => nodes,
      getScrollSurfaceContentWidth: () => currentWidth,
      getTypographySignature: () => typography,
      getRestoredThreadId: () => threadId,
    });
    priors.maybePersistSizePriors(); // settled at width 800

    // Pane resized: a partial capture at the new width must not smuggle
    // 800px-width sizes into the 640px bucket.
    currentWidth = 640;
    priors.resolveRowEstimateOnThreadEdge(null);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    listRef = fakeListRef([110, -1, -1, -1]);
    priors.maybePersistSizePriors();

    priors.resolveRowEstimateOnThreadEdge(null);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    let estimate = priors.rowEstimate!;
    expect(estimate.at(0)).toBe(110); // measured at 640 — replays
    expect(estimate.at(1)).toBe(44); // 800px size not carried into 640 → kind estimate

    // ...and the 640px capture left the 800px bucket intact, so going back
    // to the old width still replays its settled sizes.
    currentWidth = 800;
    priors.resolveRowEstimateOnThreadEdge(null);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    estimate = priors.rowEstimate!;
    expect(estimate.at(0)).toBe(90);
    expect(estimate.at(1)).toBe(91);
    expect(priors.replayStats().validity).toBe('replayed');

    // A second capture at 640 carries the earlier 640 measurement forward
    // for rows that still have not re-measured at that width.
    currentWidth = 640;
    priors.resolveRowEstimateOnThreadEdge(null);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    listRef = fakeListRef([-1, 111, -1, -1]);
    priors.maybePersistSizePriors();

    priors.resolveRowEstimateOnThreadEdge(null);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    estimate = priors.rowEstimate!;
    expect(estimate.at(0)).toBe(110); // carried within the 640 bucket
    expect(estimate.at(1)).toBe(111);
  });

  it('replays each captured width from its own bucket', () => {
    const threadId = 'thread-two-widths';
    const nodes: TimelineNode[] = Array.from({ length: 3 }, (_, i) =>
      leaf(`item-${i}`, { summary: `body ${i}`, status: 'completed', updatedAt: i }),
    );
    let currentWidth = 800;
    let listRef = fakeListRef([90, 91, 92]);
    const pane = fakePane(threadId);
    const priors = createTimelineSizePriors({
      getPane: () => pane,
      getListRef: () => listRef,
      getRevealedNodes: () => nodes,
      getScrollSurfaceContentWidth: () => currentWidth,
      getTypographySignature: () => typography,
      getRestoredThreadId: () => threadId,
    });
    priors.maybePersistSizePriors();

    // The same thread settles in a narrower pane (a split, or the sidebar
    // opening) and captures there too.
    currentWidth = 600;
    priors.resolveRowEstimateOnThreadEdge(null);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    listRef = fakeListRef([130, 131, 132]);
    priors.maybePersistSizePriors();

    // Each width replays its own measurements, both as full replays.
    currentWidth = 800;
    priors.resolveRowEstimateOnThreadEdge(null);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    expect([0, 1, 2].map((i) => priors.rowEstimate!.at(i))).toEqual([90, 91, 92]);
    expect(priors.replayStats().validity).toBe('replayed');

    currentWidth = 600;
    priors.resolveRowEstimateOnThreadEdge(null);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    expect([0, 1, 2].map((i) => priors.rowEstimate!.at(i))).toEqual([130, 131, 132]);
    expect(priors.replayStats().validity).toBe('replayed');
  });

  it('evicts the least recently captured width past the per-thread bucket cap', () => {
    const threadId = 'thread-width-cap';
    const nodes: TimelineNode[] = [leaf('a', { summary: 'hi' })];
    let currentWidth = 800;
    let listRef = fakeListRef([120]);
    const pane = fakePane(threadId);
    const priors = createTimelineSizePriors({
      getPane: () => pane,
      getListRef: () => listRef,
      getRevealedNodes: () => nodes,
      getScrollSurfaceContentWidth: () => currentWidth,
      getTypographySignature: () => typography,
      getRestoredThreadId: () => threadId,
    });

    // Four widths, each with its own measurement. Only the newest three
    // buckets survive the cap.
    [[800, 120], [700, 121], [600, 122], [500, 123]].forEach(([width, size], i) => {
      currentWidth = width;
      if (i > 0) {
        priors.resolveRowEstimateOnThreadEdge(null);
        priors.resolveRowEstimateOnThreadEdge(threadId);
      }
      listRef = fakeListRef([size]);
      priors.maybePersistSizePriors();
    });

    currentWidth = 800;
    priors.resolveRowEstimateOnThreadEdge(null);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    expect(priors.rowEstimate!.at(0)).toBe(44); // evicted → kind estimate
    expect(priors.replayStats().validity).toBe('geometry-mismatch');

    for (const [width, size] of [[700, 121], [600, 122], [500, 123]]) {
      currentWidth = width;
      priors.resolveRowEstimateOnThreadEdge(null);
      priors.resolveRowEstimateOnThreadEdge(threadId);
      expect(priors.rowEstimate!.at(0)).toBe(size);
      expect(priors.replayStats().validity).toBe('replayed');
    }
  });

  it('refuses a same-width bucket captured under different typography, and captures its own', () => {
    // Root font scale and the UI typefaces rescale every row at an
    // unchanged wrap point. Before typography joined the bucket key this
    // replayed the old heights and relied on the warm-up gate to hide the
    // correction cascade; now it is an ordinary bucket miss.
    const threadId = 'thread-typography';
    const nodes: TimelineNode[] = [leaf('a', { summary: 'hi' })];
    const currentWidth = 800;
    let listRef = fakeListRef([120]);
    const pane = fakePane(threadId);
    const priors = createTimelineSizePriors({
      getPane: () => pane,
      getListRef: () => listRef,
      getRevealedNodes: () => nodes,
      getScrollSurfaceContentWidth: () => currentWidth,
      getTypographySignature: () => typography,
      getRestoredThreadId: () => threadId,
    });
    priors.maybePersistSizePriors(); // captured at 800px / default typography

    typography = 'f18/sgeist/mgeist/c1/w1';
    priors.resolveRowEstimateOnThreadEdge(null);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    expect(priors.rowEstimate!.at(0)).toBe(44); // kind estimate, not the 120px measured before
    expect(priors.replayStats().validity).toBe('geometry-mismatch');

    // The new typography gets its OWN bucket at the same width...
    listRef = fakeListRef([150]);
    priors.maybePersistSizePriors();
    priors.resolveRowEstimateOnThreadEdge(null);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    expect(priors.rowEstimate!.at(0)).toBe(150);
    expect(priors.replayStats().validity).toBe('replayed');

    // ...and going back replays the original bucket, not the new one.
    typography = DEFAULT_TYPOGRAPHY;
    priors.resolveRowEstimateOnThreadEdge(null);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    expect(priors.rowEstimate!.at(0)).toBe(120);
    expect(priors.replayStats().validity).toBe('replayed');
  });

  it('still trusts the latest bucket at width 0 after a typography change', () => {
    // Width 0 means the surface has reported no geometry at all, so there
    // is nothing to match against; the trusted-latest replay stays the
    // documented exception, self-corrected by the per-row observer behind
    // the warm-up gate. Pinned so a future edit cannot quietly turn boot
    // replay into a guaranteed full cascade.
    const threadId = 'thread-typography-boot';
    const nodes: TimelineNode[] = [leaf('a', { summary: 'hi' })];
    let currentWidth = 800;
    const listRef = fakeListRef([120]);
    const pane = fakePane(threadId);
    const priors = createTimelineSizePriors({
      getPane: () => pane,
      getListRef: () => listRef,
      getRevealedNodes: () => nodes,
      getScrollSurfaceContentWidth: () => currentWidth,
      getTypographySignature: () => typography,
      getRestoredThreadId: () => threadId,
    });
    priors.maybePersistSizePriors();

    typography = 'f18/sgeist/mgeist/c1/w1';
    currentWidth = 0;
    priors.resolveRowEstimateOnThreadEdge(null);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    expect(priors.rowEstimate!.at(0)).toBe(120);
    expect(priors.replayStats().validity).toBe('replayed-trusted-width');
  });

  it('uses the most recently captured bucket when the surface reports width 0', () => {
    // Width 0 at first at() is "layout hasn't reported yet". With several
    // buckets stored, the best guess is the width the reader last used,
    // not whichever bucket happens to be first in the map.
    const threadId = 'thread-width-zero';
    const nodes: TimelineNode[] = [leaf('a', { summary: 'hi' })];
    let currentWidth = 800;
    let listRef = fakeListRef([120]);
    const pane = fakePane(threadId);
    const priors = createTimelineSizePriors({
      getPane: () => pane,
      getListRef: () => listRef,
      getRevealedNodes: () => nodes,
      getScrollSurfaceContentWidth: () => currentWidth,
      getTypographySignature: () => typography,
      getRestoredThreadId: () => threadId,
    });
    priors.maybePersistSizePriors();

    currentWidth = 600;
    priors.resolveRowEstimateOnThreadEdge(null);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    listRef = fakeListRef([170]);
    priors.maybePersistSizePriors();

    currentWidth = 0;
    priors.resolveRowEstimateOnThreadEdge(null);
    priors.resolveRowEstimateOnThreadEdge(threadId);
    expect(priors.rowEstimate!.at(0)).toBe(170); // the 600px bucket, captured last
    expect(priors.replayStats().validity).toBe('replayed-trusted-width');
  });

  it('never stores an entry for a capture that resolves nothing', () => {
    const threadId = 'thread-nothing';
    const nodes: TimelineNode[] = [leaf('a', { summary: 'hi' })];
    const listRef = fakeListRef([-1]);
    const pane = fakePane(threadId);
    const priors = createTimelineSizePriors({
      getPane: () => pane,
      getListRef: () => listRef,
      getRevealedNodes: () => nodes,
      getScrollSurfaceContentWidth: () => 800,
      getTypographySignature: () => typography,
      getRestoredThreadId: () => threadId,
    });
    priors.maybePersistSizePriors();

    priors.resolveRowEstimateOnThreadEdge(threadId);
    expect(priors.replayStats().source).toBe('none');
    expect(priors.replayStats().validity).toBe('no-entry');
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
      getTypographySignature: () => typography,
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
  describe('scroll-driven capture rate bound', () => {
    // `maybePersistSizePriorsInterim` is what the scroll snapshot path
    // calls, and that path fires per scroll frame. The bound has to hold
    // in BOTH directions: it must swallow a burst, and it must let the
    // next real change through once the interval has passed — a bound
    // that latched would freeze the thread's priors at the first frame of
    // its cascade, which is exactly the stale-estimate replay this module
    // exists to prevent.
    function harness(threadId: string) {
      let sizes = [100];
      let node = leaf('a', { summary: 'hi', updatedAt: 1 });
      const pane = fakePane(threadId);
      const priors = createTimelineSizePriors({
        getPane: () => pane,
        getListRef: () => fakeListRef(sizes),
        getRevealedNodes: () => [node],
        getScrollSurfaceContentWidth: () => 800,
        getTypographySignature: () => typography,
        getRestoredThreadId: () => threadId,
      });
      return {
        priors,
        /** Change the geometry so the O(1) size gate cannot absorb the call. */
        grow(px: number) {
          sizes = [sizes[0] + px];
        },
        /** Change the row's signature without moving its geometry. */
        touch(updatedAt: number) {
          node = leaf('a', { summary: 'hi', updatedAt });
        },
        signature(): string {
          return nodeSignature(node, () => undefined);
        },
        /** The one row's stored height at width 800, or null when nothing is stored. */
        stored(): number | null {
          const entry = getThreadSizePriors(threadId);
          if (!entry) return null;
          const [size] = [...(sizePriorsAtGeometry(entry, { width: 800, typography })?.rows.values() ?? [])];
          return size ?? null;
        },
        /** The signatures stored at width 800. */
        storedSignatures(): string[] {
          const entry = getThreadSizePriors(threadId);
          if (!entry) return [];
          return [...(sizePriorsAtGeometry(entry, { width: 800, typography })?.rows.keys() ?? [])];
        },
      };
    }

    it('captures the first frame and swallows the burst behind it', () => {
      vi.useFakeTimers();
      const h = harness('thread-bound-burst');
      h.priors.maybePersistSizePriorsInterim();
      expect(h.stored()).toBe(100);

      for (let frame = 0; frame < 15; frame += 1) {
        h.grow(10);
        vi.advanceTimersByTime(16);
        h.priors.maybePersistSizePriorsInterim();
      }

      // 15 frames x 16ms = 240ms, inside one interval: the stored value is
      // still the one the first call captured.
      expect(h.stored()).toBe(100);
      vi.useRealTimers();
    });

    it('lets the next change through once the interval has passed', () => {
      vi.useFakeTimers();
      const h = harness('thread-bound-release');
      h.priors.maybePersistSizePriorsInterim();
      h.grow(50);
      h.priors.maybePersistSizePriorsInterim();
      expect(h.stored()).toBe(100);

      vi.advanceTimersByTime(250);
      h.priors.maybePersistSizePriorsInterim();

      expect(h.stored()).toBe(150);
      vi.useRealTimers();
    });

    it('does not make an incoming thread wait out the outgoing cooldown', () => {
      vi.useFakeTimers();
      const h = harness('thread-bound-switch');
      h.priors.maybePersistSizePriorsInterim();
      h.grow(70);

      // The threadId edge is the switch. It resets the cooldown with the
      // rest of the per-thread capture state.
      h.priors.resolveRowEstimateOnThreadEdge('other');
      h.priors.resolveRowEstimateOnThreadEdge('thread-bound-switch');
      h.priors.maybePersistSizePriorsInterim();

      expect(h.stored()).toBe(170);
      vi.useRealTimers();
    });

    it('never bounds the settle-edge capture', () => {
      vi.useFakeTimers();
      const h = harness('thread-bound-warm');
      h.priors.maybePersistSizePriorsInterim();
      h.grow(30);

      h.priors.captureOnWarmRisingEdge(true);

      expect(h.stored()).toBe(130);
      vi.useRealTimers();
    });

    it('never bounds the final-edge capture', () => {
      // The switch-away edge (`switchThread` → the controller adapter) and
      // the unmount edge (`saveSnapshotOnDestroy`) both call the final
      // capture. They are the last chance this thread gets, so a cooldown
      // armed by the reader's last scroll frame must not swallow them.
      vi.useFakeTimers();
      const h = harness('thread-bound-final');
      h.priors.maybePersistSizePriorsInterim();
      h.grow(40);
      // Inside the cooldown: the interim call is refused.
      h.priors.maybePersistSizePriorsInterim();
      expect(h.stored()).toBe(100);

      h.priors.persistSizePriorsFinal();

      expect(h.stored()).toBe(140);
      vi.useRealTimers();
    });

    it('stores a signature-only change on the final edge, past the size gate', () => {
      // A turn end upserts the last assistant row's status/updatedAt and a
      // window sync page replaces rows with equal content: the signature
      // changes, the height does not. The gated capture (settle edge)
      // sees an unchanged total and skips, which would leave the stored
      // signature stale and miss on the next open. The final edge must
      // store the live signature regardless.
      const h = harness('thread-final-signature');
      h.priors.maybePersistSizePriors();
      const before = h.signature();
      expect(h.storedSignatures()).toEqual([before]);

      h.touch(2);
      const after = h.signature();
      expect(after).not.toBe(before);
      h.priors.maybePersistSizePriors();
      expect(h.storedSignatures()).toEqual([before]);

      h.priors.persistSizePriorsFinal();

      expect(h.storedSignatures()).toEqual([after]);
      expect(h.stored()).toBe(100);
    });
  });
});
