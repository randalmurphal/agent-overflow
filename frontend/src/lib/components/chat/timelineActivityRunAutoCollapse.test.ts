import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import type { Item } from '../../types/models';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import type { ActivityRunNode, TimelineNode } from '../../utils/subagentGrouping';
import { makeItem } from '../../../test/helpers/chat';
import { createThreadActivityRuns } from '../../stores/threadActivityRuns.svelte';
import { createTimelineActivityRunAutoCollapse } from './timelineActivityRunAutoCollapse';
import { createTimelineQuietWork, QUIET_WORK_MIN_INTERVAL_MS } from './timelineQuietWork';

// The gate over a REAL registry — release semantics are the point — with the
// pane and engine reduced to exactly what the gate reads, and the pass
// driven through a real quiet scheduler so the "never during
// reader-visible motion" gate is exercised where it now lives.
// Geometry: 600px viewport, rows laid out by the offsets each test states.

interface Harness {
  pane: ThreadPane;
  runs: ReturnType<typeof createThreadActivityRuns>;
  nodes: TimelineNode[];
  /** Row offsets by index; the entry one past the last row is totalSize. */
  offsets: number[];
  scrollTop: number;
  viewport: number;
  /** What the pane's controller reports for a spring glide in flight. */
  autoScrollInFlight: boolean;
  items: Map<string, Item>;
  expandedItemIds: Set<string>;
  quietWork: ReturnType<typeof createTimelineQuietWork>;
  /** One projection pass: resolves ids/collapse for every run node. */
  project(liveRunIndex?: number): void;
  /** Schedules the quiet scheduler and drains its tick. */
  sweep(): Promise<void>;
}

function harness(): Harness {
  const runs = createThreadActivityRuns({
    defaultCollapsed: () => true,
    windowRows: () => 30,
    scrollController: () => null,
  });
  const items = new Map<string, Item>();
  const expandedItemIds = new Set<string>();

  const self: Harness = {
    runs,
    nodes: [],
    offsets: [],
    scrollTop: 0,
    viewport: 600,
    autoScrollInFlight: false,
    items,
    expandedItemIds,
    pane: undefined as unknown as ThreadPane,
    quietWork: undefined as unknown as ReturnType<typeof createTimelineQuietWork>,
    project(liveRunIndex = -1) {
      runs.beginPass();
      self.nodes.forEach((node, index) => {
        if (node.kind !== 'activity_run') return;
        const resolved = runs.resolve(
          node.memberItemIds.map((id) => [id]),
          node.threadId,
        );
        node.runId = resolved.runId;
        node.live = index === liveRunIndex;
      });
      self.nodes.forEach((node) => {
        if (node.kind !== 'activity_run') return;
        node.collapsed = runs.collapsedFor(node.runId, node.live);
      });
      runs.endPass();
    },
    async sweep() {
      self.quietWork.schedule();
      await tick();
      await tick();
      // The scheduler is rate-bound, so a sweep following a recent one is
      // served by a trailing run rather than immediately. Fake timers, and
      // one interval, so the tests keep reading as "sweep, then look".
      await vi.advanceTimersByTimeAsync(QUIET_WORK_MIN_INTERVAL_MS);
      await tick();
      await tick();
    },
  };

  self.pane = {
    activityRuns: runs,
    getItemById: (id: string) => items.get(id),
    hasUserExpansionWithin: (ids: Iterable<string>) => {
      for (const id of ids) {
        if (expandedItemIds.has(id)) return true;
      }
      return false;
    },
    // No preserveViewportBottom on the stub: withViewportBottomHeld falls
    // back to a plain call, which is the shape panes without a virtualizer
    // register anyway.
    scrollController: {
      autoScrollInFlight: () => self.autoScrollInFlight,
    },
  } as unknown as ThreadPane;

  const listRef = {
    getViewportSize: () => self.viewport,
    getScrollOffset: () => self.scrollTop,
    getTotalSize: () => self.offsets[self.offsets.length - 1] ?? 0,
    getItemOffset: (index: number) => self.offsets[index] ?? 0,
  } as unknown as TimelineVirtualizerHandle;

  self.quietWork = createTimelineQuietWork({
    isTest: false,
    autoScrollInFlight: () => self.autoScrollInFlight,
    passes: [
      createTimelineActivityRunAutoCollapse({
        getPane: () => self.pane,
        getListRef: () => listRef,
        getRevealedNodes: () => self.nodes,
      }),
    ],
  });

  return self;
}

function runNode(memberItemIds: string[], threadId = 'thread-1'): ActivityRunNode {
  return {
    kind: 'activity_run',
    runId: '',
    threadId,
    children: memberItemIds.map((id) => ({
      kind: 'leaf',
      item: makeItem({ id, kind: 'tool_call', threadId }),
    })),
    collapsed: false,
    live: false,
    atTail: false,
    mountedFrom: 0,
    mountedRows: memberItemIds.length,
    membershipEpoch: 1,
    memberItemIds,
  };
}

function proseNode(id: string): TimelineNode {
  return { kind: 'leaf', item: makeItem({ id, kind: 'assistant_text' }) };
}

/**
 * The standard scene: one settled run at the top, prose filling 2400px below
 * it, reader pinned to the bottom. The run is fully above the viewport and
 * more than one viewport from the tail — eligible unless a test says why not.
 */
function pinnedPastSettledRun(h: Harness): ActivityRunNode {
  const run = runNode(['t0', 't1']);
  h.nodes = [run, proseNode('p0'), proseNode('p1')];
  h.offsets = [0, 400, 1600, 2800];
  h.project(0);
  h.project(-1);
  h.scrollTop = 2800 - 600;
  return run;
}

describe('timelineActivityRunAutoCollapse', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('releases a settled off-screen run, which collapses it to the default', async () => {
    const h = harness();
    const run = pinnedPastSettledRun(h);
    expect(run.collapsed).toBe(false);

    await h.sweep();
    h.project();

    expect(h.runs.openedLiveRunIds()).toEqual([]);
    expect(run.collapsed).toBe(true);
  });

  it('leaves the live run alone even when geometry would allow it', async () => {
    const h = harness();
    const run = runNode(['t0', 't1']);
    h.nodes = [run, proseNode('p0'), proseNode('p1')];
    h.offsets = [0, 400, 1600, 2800];
    // Still live on the latest pass — a run is not settled while the next
    // activity row would join it, whatever its distance from the reader.
    //
    // Deliberately NOT a scene today's projector produces: it marks only the
    // tail run live, and a tail node's bottom is totalSize, which the
    // tail-distance rule already refuses. A live non-tail run is the one
    // geometry that isolates the gate's semantic guard, and the guard must
    // hold even if liveness semantics ever widen past the tail.
    h.project(0);
    h.scrollTop = 2200;

    await h.sweep();
    h.project(0);

    expect(run.collapsed).toBe(false);
  });

  it('defers to an in-flight glide and releases once it settles', async () => {
    // The release's bottom-pinned restore is a direct write; landing it
    // while the spring is animating (or armed) would snap the glide the
    // reader is watching. The scheduler's quiet gate holds the pass back —
    // the glide's settle re-triggers it (scrollend, or the scheduler's own
    // recheck timer), modeled here as the second sweep.
    const h = harness();
    const run = pinnedPastSettledRun(h);
    h.autoScrollInFlight = true;

    await h.sweep();
    h.project();
    expect(run.collapsed).toBe(false);
    expect(h.runs.openedLiveRunIds()).toEqual([run.runId]);

    h.autoScrollInFlight = false;
    await h.sweep();
    h.project();
    expect(run.collapsed).toBe(true);
    expect(h.runs.openedLiveRunIds()).toEqual([]);
  });

  it('waits for the run to leave the viewport', async () => {
    const h = harness();
    const run = pinnedPastSettledRun(h);
    // Reader scrolled back up: the run is on screen.
    h.scrollTop = 100;

    await h.sweep();
    h.project();

    expect(run.collapsed).toBe(false);
  });

  it('waits for the run to fall a viewport behind the tail', async () => {
    // The reader scrolled up PAST the latest run to reread something older.
    // The run is fully below their viewport — but it is what they were just
    // watching, and scrolling back down to it must not reveal a chip. Only
    // distance from the TAIL protects this case; viewport-exit alone would
    // collapse it the moment they scrolled up.
    const h = harness();
    const run = runNode(['t0', 't1']);
    h.nodes = [proseNode('p0'), run, proseNode('p1')];
    h.offsets = [0, 1900, 2300, 2800];
    h.project(1);
    h.project(-1);
    h.scrollTop = 0;

    await h.sweep();
    h.project();

    expect(run.collapsed).toBe(false);
  });

  it('respects a reader parked inside the run', async () => {
    const h = harness();
    const run = pinnedPastSettledRun(h);
    // The two ways the registry knows: a pinned mount window, or an inner
    // scroll snapshot recorded as escaped.
    h.runs.setWindowAnchor(run.runId, 't0');

    await h.sweep();
    h.project();
    expect(run.collapsed).toBe(false);

    h.runs.setWindowAnchor(run.runId, null);
    h.runs.saveScrollSnapshot(run.runId, { scrollTop: 40, escaped: true });

    await h.sweep();
    h.project();
    expect(run.collapsed).toBe(false);

    // Both released: the reader has genuinely moved on.
    h.runs.saveScrollSnapshot(run.runId, { scrollTop: 40, escaped: false });
    await h.sweep();
    h.project();
    expect(run.collapsed).toBe(true);
  });

  it('respects an expansion the reader made inside the run', async () => {
    const h = harness();
    const run = pinnedPastSettledRun(h);
    h.expandedItemIds.add('t1');

    await h.sweep();
    h.project();
    expect(run.collapsed).toBe(false);

    h.expandedItemIds.clear();
    await h.sweep();
    h.project();
    expect(run.collapsed).toBe(true);
  });

  it('releases a run holding a failure — the chip carries the failure marker', async () => {
    // The failure hold was removed (2026-08-18): the collapsed chip's summary
    // already renders a failure marker (`activityRunSummary`), so one errored
    // command must not pin a viewport of history open in normal operation.
    const h = harness();
    const run = pinnedPastSettledRun(h);
    h.items.set('t1', makeItem({ id: 't1', kind: 'tool_call', status: 'errored' }));

    await h.sweep();
    h.project();

    expect(run.collapsed).toBe(true);
  });

  it('does nothing while the scroller is unmeasured', async () => {
    // Against a zero viewport every run is "out of sight" — the same reason
    // the row-UI prune refuses to prune against one.
    const h = harness();
    const run = pinnedPastSettledRun(h);
    h.viewport = 0;

    await h.sweep();
    h.project();

    expect(run.collapsed).toBe(false);
  });

  it('releases every eligible run in one pass', async () => {
    // The turn's own history: the first run holds the tail, prose displaces
    // it, a second run takes over, prose settles that one too. Both carry
    // holds; both are far above a bottom-pinned reader.
    const h = harness();
    const first = runNode(['t0', 't1']);
    const second = runNode(['t2', 't3']);
    h.nodes = [first];
    h.offsets = [0, 300];
    h.project(0);
    h.nodes = [first, proseNode('p0'), second, proseNode('p1'), proseNode('p2')];
    h.offsets = [0, 300, 900, 1200, 2400, 3600];
    h.project(2);
    h.project(-1);
    h.scrollTop = 3000;
    expect(h.runs.openedLiveRunIds()).toHaveLength(2);

    await h.sweep();
    h.project();

    expect(h.runs.openedLiveRunIds()).toEqual([]);
    expect(first.collapsed).toBe(true);
    expect(second.collapsed).toBe(true);
  });

  it('a stale scheduled pass no-ops after invalidate', async () => {
    const h = harness();
    const run = pinnedPastSettledRun(h);

    h.quietWork.schedule();
    h.quietWork.invalidate();
    await tick();
    await tick();
    h.project();

    expect(run.collapsed).toBe(false);
  });
});
