// The switch-edge classification in timelineRestore.svelte.ts — which
// transition the timeline just observed, and the choreography each one
// owes the scroll controller.
//
// These run against a REAL controller rather than a spy object on
// purpose: the thing that broke in production was not "which method did
// we call" but "the restore was refused by the consent gate, so the
// surface stayed at scrollTop=0 while claiming the bottom". Only the
// controller's own gate can witness that, so the assertions are the
// reader-visible outcome (where the viewport ended up, whether content
// is hidden behind the warm gate) with the call spies as support.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { TimelineNode } from '../../utils/subagentGrouping';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import {
  createUseStickToBottomController,
  type UseStickToBottomController,
} from '../../utils/scroll/index.svelte';
import { resetScrollIntentModuleStateForTest } from '../../utils/scroll/intent';
import { MockResizeObserver, stubGeometry, type Geometry } from '../../utils/scroll/testGeometry';
import { createTimelineRestore, type TimelineRestore } from './timelineRestore.svelte';

const ROW_PX = 100;

function leaf(id: string): TimelineNode {
  return { kind: 'leaf', item: { id } } as unknown as TimelineNode;
}

/** Content 1000px tall in a 600px viewport → bottom target 400. */
function makeGeometry(): Geometry {
  return { scrollHeight: 1000, clientHeight: 600, scrollTop: 0, contentHeight: 1000 };
}

interface Harness {
  restore: TimelineRestore;
  stick: UseStickToBottomController;
  geom: Geometry;
  scrollEl: HTMLDivElement;
  armWarmupWithReset: ReturnType<typeof vi.fn>;
  resetAutoLoadGates: ReturnType<typeof vi.fn>;
  /** Mutate the pane the restore session reads through its getter. */
  pane: { threadId: string | null; scrollStateKey: string | null; items: unknown[]; loading: boolean };
  /** One engine-sourced sample, as the virtualizer's subscription replays it. */
  deliverGeometry(height?: number): void;
  destroy(): void;
}

function makeHarness(nodes: TimelineNode[]): Harness {
  const scrollEl = document.createElement('div');
  const contentEl = document.createElement('div');
  scrollEl.appendChild(contentEl);
  document.body.appendChild(scrollEl);
  const geom = makeGeometry();
  stubGeometry(scrollEl, contentEl, geom);

  const stick = createUseStickToBottomController({ externalContentGeometry: true });
  stick.attach(scrollEl, contentEl);

  const pane = {
    threadId: null as string | null,
    scrollStateKey: null as string | null,
    items: [] as unknown[],
    loading: false,
  };

  // Only the geometry queries the restore session actually reaches for:
  // the snapshot capture's anchor (findItemIndex + getItemOffset) and
  // the offset it captures at.
  const listRef = {
    getScrollOffset: () => geom.scrollTop,
    findItemIndex: (offset: number) => Math.min(Math.floor(offset / ROW_PX), nodes.length - 1),
    getItemOffset: (index: number) => index * ROW_PX,
    scrollToIndex: () => {},
  } as unknown as TimelineVirtualizerHandle;

  const armWarmupWithReset = vi.fn(() => stick.armWarmup());
  const resetAutoLoadGates = vi.fn();

  const restore = createTimelineRestore({
    getPane: () => pane as unknown as ThreadPane,
    stick,
    getListRef: () => listRef,
    getScrollEl: () => scrollEl,
    getRevealedNodes: () => nodes,
    getGroupedNodes: () => nodes,
    findTimelineNodeIndex: (itemId) =>
      nodes.findIndex((node) => (node as { item?: { id?: string } }).item?.id === itemId),
    persistSizePriors: () => {},
    persistSizePriorsExact: () => {},
    armWarmupWithReset,
    resetAutoLoadGates,
  });

  return {
    restore,
    stick,
    geom,
    scrollEl,
    armWarmupWithReset,
    resetAutoLoadGates,
    pane,
    deliverGeometry(height = geom.contentHeight) {
      stick.deliverContentGeometry({
        height,
        width: 800,
        windowMeasured: false,
        maxFirstMeasureCorrectionPx: 0,
        viewportHeight: geom.clientHeight,
      });
    },
    destroy() {
      stick.detach();
      scrollEl.remove();
    },
  };
}

/** Mount the pane on a thread, the way pane.switchThread leaves it. */
function mountThread(h: Harness, threadId: string, itemCount = 3): void {
  h.pane.threadId = threadId;
  h.pane.scrollStateKey = threadId;
  h.pane.items = Array.from({ length: itemCount }, (_, i) => ({ id: `${threadId}-${i}` }));
  h.pane.loading = false;
}

describe('timeline restore switch edges', () => {
  let originalRO: typeof ResizeObserver | undefined;
  let harness: Harness | undefined;

  beforeEach(() => {
    resetScrollIntentModuleStateForTest();
    MockResizeObserver.instances = [];
    originalRO = globalThis.ResizeObserver;
    (globalThis as unknown as { ResizeObserver: typeof MockResizeObserver }).ResizeObserver =
      MockResizeObserver;
  });

  afterEach(() => {
    harness?.destroy();
    harness = undefined;
    if (originalRO) {
      (globalThis as unknown as { ResizeObserver: typeof ResizeObserver }).ResizeObserver = originalRO;
    }
  });

  it('arms warm-up and the restore snap on the FIRST mount of a populated pane', () => {
    // The regression: a page refresh restoring a pane layout, or an
    // existing thread opened in a new pane, mounts with rows already
    // there. The nullable-sentinel classifier read that as
    // placeholder→materialized (skipWarmup + markAtBottom, no consent),
    // so the restore below was refused and the reader was left at
    // scrollTop=0 over a tail-seeded transcript.
    const nodes = [leaf('a'), leaf('b'), leaf('c')];
    const h = (harness = makeHarness(nodes));
    mountThread(h, 'thread-first-mount');

    h.restore.handleSwitchEdgePre('thread-first-mount', 0);

    expect(h.armWarmupWithReset).toHaveBeenCalledTimes(1);
    expect(h.resetAutoLoadGates).toHaveBeenCalledTimes(1);
    // armRestoreSnap's defensive escape, and the warm gate still closed
    // — the two things the optimistic branch would have got wrong
    // (skipWarmup opens the gate; no arm leaves escape false).
    expect(h.stick.escapedFromLock).toBe(true);
    expect(h.stick.isWarm).toBe(false);

    h.restore.maybeRestoreAfterFlush();

    // The restore executed: forceStick({reason:'restore'}) passed the
    // consent gate and landed the viewport on the bottom target.
    expect(h.restore.restoredThreadId).toBe('thread-first-mount');
    expect(h.geom.scrollTop).toBe(400);
    expect(h.stick.isAtBottom).toBe(true);
  });

  it('reaches the true bottom on the first geometry sample alone', () => {
    // The Fix A + Fix B integration: the virtualizer's subscription
    // replays one sample before the restore effect runs, and nothing
    // else arrives. The surface must still end up at the true bottom —
    // no second geometry change to rescue it.
    const nodes = [leaf('a'), leaf('b'), leaf('c')];
    const h = (harness = makeHarness(nodes));
    mountThread(h, 'thread-first-sample');

    h.restore.handleSwitchEdgePre('thread-first-sample', 0);
    // The replayed sample lands while the defensive escape is up, so it
    // deliberately writes nothing.
    h.deliverGeometry();
    expect(h.geom.scrollTop).toBe(0);

    h.restore.maybeRestoreAfterFlush();

    expect(h.geom.scrollTop).toBe(400);
  });

  it('materializes a placeholder without the warm gate or a restore snap', () => {
    const nodes: TimelineNode[] = [];
    const h = (harness = makeHarness(nodes));

    // Draft placeholder first: no thread, nothing to anchor against.
    h.restore.handleSwitchEdgePre(null, 0);
    expect(h.stick.isAtBottom).toBe(true);
    expect(h.stick.escapedFromLock).toBe(false);

    mountThread(h, 'thread-materialized', 1);
    h.restore.handleSwitchEdgePre('thread-materialized', 0);

    // Optimistic: the row renders immediately (warm gate skipped), the
    // controller claims the bottom, and no restore consent is armed
    // because there is no measurement cascade to restore across.
    expect(h.armWarmupWithReset).not.toHaveBeenCalled();
    expect(h.stick.isWarm).toBe(true);
    expect(h.stick.isAtBottom).toBe(true);
    expect(h.stick.escapedFromLock).toBe(false);
  });

  it('re-arms and resets the restore session on a thread switch', () => {
    const nodes = [leaf('a'), leaf('b'), leaf('c')];
    const h = (harness = makeHarness(nodes));
    mountThread(h, 'thread-one');
    h.restore.handleSwitchEdgePre('thread-one', 0);
    h.restore.maybeRestoreAfterFlush();
    expect(h.restore.restoredThreadId).toBe('thread-one');
    h.armWarmupWithReset.mockClear();

    mountThread(h, 'thread-two');
    h.restore.handleSwitchEdgePre('thread-two', 0);

    expect(h.restore.restoredThreadId).toBeNull();
    expect(h.armWarmupWithReset).toHaveBeenCalledTimes(1);
    expect(h.stick.escapedFromLock).toBe(true);
    expect(h.stick.isWarm).toBe(false);

    h.geom.scrollTop = 0;
    h.restore.maybeRestoreAfterFlush();
    expect(h.geom.scrollTop).toBe(400);
  });

  it('takes the same path for a same-thread re-switch, and no path at all for a repeat edge', () => {
    // A forced in-place reload (revert / refresh) keeps pane.threadId
    // and bumps pane.switchGeneration.
    const nodes = [leaf('a'), leaf('b'), leaf('c')];
    const h = (harness = makeHarness(nodes));
    mountThread(h, 'thread-reload');
    h.restore.handleSwitchEdgePre('thread-reload', 0);
    h.restore.maybeRestoreAfterFlush();
    h.armWarmupWithReset.mockClear();
    h.resetAutoLoadGates.mockClear();

    // The effect.pre re-running over an unchanged edge must do nothing.
    h.restore.handleSwitchEdgePre('thread-reload', 0);
    expect(h.armWarmupWithReset).not.toHaveBeenCalled();
    expect(h.resetAutoLoadGates).not.toHaveBeenCalled();
    expect(h.restore.restoredThreadId).toBe('thread-reload');

    h.restore.handleSwitchEdgePre('thread-reload', 1);
    expect(h.restore.restoredThreadId).toBeNull();
    expect(h.armWarmupWithReset).toHaveBeenCalledTimes(1);
    expect(h.stick.escapedFromLock).toBe(true);
  });

  it('leaves the reader alone when a gesture escapes between the arm and the restore', () => {
    const nodes = [leaf('a'), leaf('b'), leaf('c')];
    const h = (harness = makeHarness(nodes));
    mountThread(h, 'thread-escaped');

    h.restore.handleSwitchEdgePre('thread-escaped', 0);
    // A real upward wheel on the surface: it clears the one-shot restore
    // consent the arm just set (utils/scroll/intent.ts § Restore-snap
    // consent), which is the load-bearing distinguisher between the
    // arm's own defensive escape and a user who has actually escaped.
    h.scrollEl.dispatchEvent(new WheelEvent('wheel', { deltaY: -120, bubbles: true }));
    h.geom.scrollTop = 120;

    h.restore.maybeRestoreAfterFlush();

    expect(h.geom.scrollTop).toBe(120);

    // And a repeat pass over the same edge (the effect.pre re-running on
    // any pane read) must not re-arm and snap them down either.
    h.restore.handleSwitchEdgePre('thread-escaped', 0);
    h.restore.maybeRestoreAfterFlush();
    expect(h.geom.scrollTop).toBe(120);
  });
});
