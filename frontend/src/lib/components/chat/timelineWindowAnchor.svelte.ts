// Anchored timeline-height transactions: run a change that moves rows, then
// put the reader back where they were once it flushes. Both entry points share
// the same shape — capture intent, pause the spring, run the change, restore
// after the flush — and differ only in which edge they hold still.
//
// `preserveTimelineWindowAnchor` is the seam `thread.svelte.ts`'s recent-window
// pruning calls through, and holds the TOP: rows vanish from an edge the reader
// is not looking at, so the row they are reading keeps its offset.
//
// `preserveViewportBottom` is for a height change the reader ASKED for — a run
// collapsing or expanding — and holds the BOTTOM, so the change opens upward
// and the rows they were reading do not move down the page. It also keeps the
// spring out of it: an expand while stuck at the bottom would otherwise reach
// the controller as content growth and animate across the whole delta.
//
// Both reach components through `PaneScrollController` (MessageTimeline's
// `paneScrollController` adapter), so a caller needs the pane, not the
// timeline.

import { tick } from 'svelte';
import type {
  PreserveViewportBottomOptions,
  ThreadPane,
  TimelineWindowAnchorOperation,
} from '../../stores/thread.svelte';
import type { UseStickToBottomController } from '../../utils/scroll/index.svelte';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import type { TimelineNode } from '../../utils/subagentGrouping';
import { timelineNodeKey } from '../../utils/subagentGrouping';
import {
  captureTimelineAnchor,
  captureTimelineTailAnchor,
  isPureKeyedHeadDrop,
  type TimelineAnchor,
  type TimelineTailAnchor,
} from './timelineScroll';

interface TimelineWindowAnchorIntent {
  switchGeneration: number;
  shouldStickToBottom: boolean;
  anchor: TimelineAnchor | null;
}

interface ViewportBottomIntent {
  switchGeneration: number;
  holdingBottom: boolean;
  anchor: TimelineTailAnchor | null;
  yieldToStructuralAppend: boolean;
}

export interface TimelineWindowAnchorOptions {
  getPane(): ThreadPane;
  stick: UseStickToBottomController;
  getListRef(): TimelineVirtualizerHandle | undefined;
  getScrollEl(): HTMLDivElement | undefined;
  getRevealedNodes(): TimelineNode[];
  findTimelineNodeIndex(itemId: string): number;
  saveScrollSnapshot(): void;
  nextRestoreToken(): number;
  isRestoreTokenCurrent(token: number): boolean;
}

export interface TimelineWindowAnchor {
  /** Reactive — the `virtualizerShiftAtHead` $derived reads this. */
  readonly pruneShiftAtHead: boolean;
  clearTimelineWindowPruneShift(): void;
  preserveTimelineWindowAnchor(operation: TimelineWindowAnchorOperation): boolean;
  preserveViewportBottom(change: () => void, opts?: PreserveViewportBottomOptions): void;
}

export function createTimelineWindowAnchor(
  options: TimelineWindowAnchorOptions,
): TimelineWindowAnchor {
  let timelineWindowPruneShiftAtHead = $state(false);
  let timelineWindowPruneShiftResetToken = 0;

  // The reader is on the timeline's last row, so the bottom edge IS the anchor
  // and no node has to be named to hold it.
  function holdingBottom(): boolean {
    return options.stick.isSticky || (!options.stick.escapedFromLock && options.stick.isAtBottom);
  }

  function captureTimelineWindowAnchorIntent(): TimelineWindowAnchorIntent {
    const pane = options.getPane();
    const shouldStickToBottom = holdingBottom();
    const currentListRef = options.getListRef();
    return {
      switchGeneration: pane.switchGeneration,
      shouldStickToBottom,
      anchor:
        shouldStickToBottom || !currentListRef
          ? null
          : captureTimelineAnchor(
              options.getRevealedNodes(),
              currentListRef,
              currentListRef.getScrollOffset(),
              { clampIndex: true },
            ),
    };
  }

  function canApplyPruneWithoutDroppingAnchor(
    intent: TimelineWindowAnchorIntent,
    operation: TimelineWindowAnchorOperation,
  ): boolean {
    const revealedNodes = options.getRevealedNodes();
    if (intent.shouldStickToBottom || revealedNodes.length === 0) {
      return true;
    }
    return intent.anchor !== null && operation.keepsItem(intent.anchor.itemId);
  }

  function timelineNodeKeys(): string[] {
    return options.getRevealedNodes().map((node) => timelineNodeKey(node));
  }

  function markTimelineWindowPruneShiftForOneFlush(): void {
    timelineWindowPruneShiftAtHead = true;
    const resetToken = ++timelineWindowPruneShiftResetToken;
    void tick().then(() => {
      if (timelineWindowPruneShiftResetToken !== resetToken) return;
      timelineWindowPruneShiftAtHead = false;
    });
  }

  function clearTimelineWindowPruneShift(): void {
    timelineWindowPruneShiftResetToken += 1;
    timelineWindowPruneShiftAtHead = false;
  }

  // Every restore in this module preserves intent: scrollToIndex writes are
  // tagged programmatic at the controller chokepoint, so no escape flip and no
  // external-scroll wrap is needed. They are also self-converging — the
  // virtualizer re-targets across several passes as measurements land — which
  // is what lets a single `tick()` be enough to schedule them.
  function restoreBottomEdge(): void {
    const revealedNodes = options.getRevealedNodes();
    const lastIndex = revealedNodes.length - 1;
    if (lastIndex >= 0) {
      options.getListRef()?.scrollToIndex(lastIndex, { align: 'end' });
    }
    options.stick.markAtBottom();
    options.saveScrollSnapshot();
  }

  function restoreAnchorAfterTimelineWindowPrune(anchor: TimelineAnchor): void {
    const idx = options.findTimelineNodeIndex(anchor.itemId);
    if (idx < 0) return;
    options.getListRef()?.scrollToIndex(idx, {
      align: 'start',
      offset: -anchor.offsetTop,
    });
    options.saveScrollSnapshot();
  }

  async function restoreTimelineWindowAnchorAfterPrune(
    intent: TimelineWindowAnchorIntent,
    token: number,
    release: () => void,
  ): Promise<void> {
    try {
      await tick();
      if (!options.isRestoreTokenCurrent(token)) return;
      if (options.getPane().switchGeneration !== intent.switchGeneration) return;

      if (intent.shouldStickToBottom) {
        restoreBottomEdge();
        return;
      }

      if (!options.getListRef() || !intent.anchor) return;
      restoreAnchorAfterTimelineWindowPrune(intent.anchor);
    } finally {
      release();
    }
  }

  function preserveTimelineWindowAnchor(operation: TimelineWindowAnchorOperation): boolean {
    if (!options.getListRef() || !options.getScrollEl()) {
      operation.run();
      return true;
    }

    const intent = captureTimelineWindowAnchorIntent();
    if (!canApplyPruneWithoutDroppingAnchor(intent, operation)) {
      return false;
    }

    const release = options.stick.pauseAutoScroll();
    const token = options.nextRestoreToken();
    const beforeNodeKeys = timelineNodeKeys();
    try {
      operation.run();
      const afterNodeKeys = timelineNodeKeys();
      if (isPureKeyedHeadDrop(beforeNodeKeys, afterNodeKeys)) {
        markTimelineWindowPruneShiftForOneFlush();
      }
    } catch (err) {
      release();
      throw err;
    }
    void restoreTimelineWindowAnchorAfterPrune(intent, token, release);
    return true;
  }

  function restoreTailAnchor(anchor: TimelineTailAnchor): void {
    const idx = options.findTimelineNodeIndex(anchor.itemId);
    if (idx < 0) return;
    // `end` targets the node's bottom against the viewport's; the offset gives
    // back the gap it had, so the row lands exactly where the reader left it.
    options.getListRef()?.scrollToIndex(idx, {
      align: 'end',
      offset: -anchor.offsetBottom,
    });
    options.saveScrollSnapshot();
  }

  async function restoreAfterViewportBottomChange(
    intent: ViewportBottomIntent,
    token: number,
    release: () => void,
  ): Promise<void> {
    try {
      await tick();
      if (!options.isRestoreTokenCurrent(token)) return;
      if (options.getPane().switchGeneration !== intent.switchGeneration) return;

      if (intent.holdingBottom) {
        // An unasked transaction (the auto-collapse gate) can race a
        // streamed append landing in the flushes between its change and
        // this restore. Writing the bottom then would include the new
        // row — the glide the append armed finds zero distance and the
        // row teleports in (bug-report-20260731T141600Z). Stand down and
        // hand the fresh geometry to the live-content path: the armed
        // spring (or the one already chasing) owns the trip to the
        // bottom. The pinned pre-append view needs no write of its own —
        // the browser's clamp and the engine's yielded compensation keep
        // it in place.
        if (intent.yieldToStructuralAppend && options.stick.autoScrollInFlight()) {
          options.stick.observe('live-content');
          options.saveScrollSnapshot();
          return;
        }
        restoreBottomEdge();
        return;
      }
      if (!options.getListRef() || !intent.anchor) return;
      restoreTailAnchor(intent.anchor);
    } finally {
      release();
    }
  }

  function preserveViewportBottom(
    change: () => void,
    opts?: PreserveViewportBottomOptions,
  ): void {
    const listRef = options.getListRef();
    if (!listRef || !options.getScrollEl()) {
      change();
      return;
    }

    const intent: ViewportBottomIntent = {
      switchGeneration: options.getPane().switchGeneration,
      holdingBottom: holdingBottom(),
      anchor: null,
      yieldToStructuralAppend: opts?.yieldToStructuralAppend === true,
    };
    if (!intent.holdingBottom) {
      intent.anchor = captureTimelineTailAnchor(
        options.getRevealedNodes(),
        listRef,
        listRef.getScrollOffset(),
      );
    }

    // The pause is what keeps the spring out of this. Without it the growth
    // reaches the controller as "content grew while sticky" and it animates
    // across the whole delta — which for a collapse-all is most of the
    // conversation, and the reader watches their own click scroll past them.
    const release = options.stick.pauseAutoScroll();
    const token = options.nextRestoreToken();
    try {
      change();
    } catch (err) {
      release();
      throw err;
    }
    void restoreAfterViewportBottomChange(intent, token, release);
  }

  return {
    get pruneShiftAtHead() {
      return timelineWindowPruneShiftAtHead;
    },
    clearTimelineWindowPruneShift,
    preserveTimelineWindowAnchor,
    preserveViewportBottom,
  };
}
