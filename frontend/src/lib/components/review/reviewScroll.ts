// The review pane's scroll owner. The review surface is a static document
// (no streaming, no bottom pin, no springs), so unlike chat it does NOT use
// utils/scroll/ — engine compensations and imperative jumps write scrollTop
// directly. This module exists so the review pane still has exactly ONE
// scrollTop writer (the frontend-scroll.md ownership rule), not so writes
// can be arbitrated: with a stationary reading anchor as the only policy,
// every compensation is applied verbatim.
//
// Scroll positions are remembered per (threadId, scope, viewMode, wordWrap)
// for the session — the same geometry key that forces a virtualizer
// remount, since a position saved under one geometry is meaningless in
// another.

export interface ReviewScrollOwner {
  /** The TimelineVirtualizer `applyScrollTarget` prop. */
  applyScrollTarget(top: number): void;
  /** The TimelineVirtualizer `onCompensation` prop — applied verbatim. */
  applyCompensation(compensation: { target: number }): void;
  /** Save the current position under `key` (call on scrollend + destroy). */
  savePosition(key: string): void;
  /** Restore a previously saved position; false when none was saved. */
  restorePosition(key: string): boolean;
}

const savedPositions = new Map<string, number>();
const SAVED_POSITION_CAP = 200;

export function reviewScrollKey(
  threadId: string,
  scope: string,
  viewMode: string,
  wordWrap: boolean,
): string {
  return `${threadId}:${scope}:${viewMode}:${wordWrap ? 'wrap' : 'nowrap'}`;
}

export function createReviewScrollOwner(
  getScroller: () => HTMLElement | undefined,
): ReviewScrollOwner {
  function write(top: number): void {
    const scroller = getScroller();
    if (!scroller) return;
    scroller.scrollTop = top;
  }

  return {
    applyScrollTarget: write,
    applyCompensation(compensation) {
      write(compensation.target);
    },
    savePosition(key) {
      const scroller = getScroller();
      if (!scroller) return;
      savedPositions.delete(key);
      savedPositions.set(key, scroller.scrollTop);
      while (savedPositions.size > SAVED_POSITION_CAP) {
        const oldest = savedPositions.keys().next().value;
        if (oldest === undefined) break;
        savedPositions.delete(oldest);
      }
    },
    restorePosition(key) {
      const saved = savedPositions.get(key);
      if (saved === undefined) return false;
      write(saved);
      return true;
    },
  };
}

export function resetReviewScrollPositionsForTest(): void {
  savedPositions.clear();
}
