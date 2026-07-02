// Timeline-window prune anchor transaction: the seam
// `thread.svelte.ts`'s recent-window pruning calls through
// `PaneScrollController.preserveTimelineWindowAnchor` (via
// MessageTimeline's `paneScrollController` adapter) to drop off-window
// rows without losing the user's scroll position — either they stay
// pinned to the bottom, or the pre-prune anchor row is restored at the
// same viewport offset once the prune's DOM update flushes.

import { tick } from 'svelte';
import type { ThreadPane, TimelineWindowAnchorOperation } from '../../stores/thread.svelte';
import type { UseStickToBottomController } from '../../utils/scroll/index.svelte';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import type { TimelineNode } from '../../utils/subagentGrouping';
import { timelineNodeKey } from '../../utils/subagentGrouping';
import { captureTimelineAnchor, isPureKeyedHeadDrop, type TimelineAnchor } from './timelineScroll';

interface TimelineWindowAnchorIntent {
  switchGeneration: number;
  shouldStickToBottom: boolean;
  anchor: TimelineAnchor | null;
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
}

export function createTimelineWindowAnchor(
  options: TimelineWindowAnchorOptions,
): TimelineWindowAnchor {
  let timelineWindowPruneShiftAtHead = $state(false);
  let timelineWindowPruneShiftResetToken = 0;

  function captureTimelineWindowAnchorIntent(): TimelineWindowAnchorIntent {
    const pane = options.getPane();
    const shouldStickToBottom =
      options.stick.isSticky || (!options.stick.escapedFromLock && options.stick.isAtBottom);
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

  // Both prune restores preserve intent: scrollToIndex writes are tagged
  // programmatic at the controller chokepoint, so no escape flip and no
  // external-scroll wrap is needed.
  function restoreBottomAfterTimelineWindowPrune(): void {
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
        restoreBottomAfterTimelineWindowPrune();
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

  return {
    get pruneShiftAtHead() {
      return timelineWindowPruneShiftAtHead;
    },
    clearTimelineWindowPruneShift,
    preserveTimelineWindowAnchor,
  };
}
