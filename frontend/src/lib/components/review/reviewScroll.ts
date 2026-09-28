import type { ReadingAnchor } from '../../utils/reviewAnchor';

// The review pane's scroll owner. The review surface is a static document
// (no streaming, no bottom pin, no springs), so unlike chat it does NOT use
// utils/scroll/ — engine compensations and imperative jumps write scrollTop
// directly. This module exists so the review pane still has exactly ONE
// scrollTop writer (the frontend-scroll.md ownership rule), not so writes
// can be arbitrated: with a stationary reading anchor as the only policy,
// every compensation is applied verbatim.
//
// Reading positions are remembered per (threadId, scope, viewMode,
// wordWrap) for the session, as reading anchors rather than pixels: a
// long diff holds only some of its rows, so a pixel offset names a
// different line once the held rows move.

export interface ReviewScrollOwner {
  /** The TimelineVirtualizer `applyScrollTarget` prop. */
  applyScrollTarget(top: number): void;
  /** The TimelineVirtualizer `onCompensation` prop — applied verbatim. */
  applyCompensation(compensation: { target: number }): void;
}

// null is a position at the top.
const savedPositions = new Map<string, ReadingAnchor | null>();
const SAVED_POSITION_CAP = 200;

/** Remembers the reading position under `key` (on scrollend and when the
 * key changes or the surface goes). */
export function saveReadingPosition(key: string, anchor: ReadingAnchor | null): void {
  savedPositions.delete(key);
  savedPositions.set(key, anchor);
  while (savedPositions.size > SAVED_POSITION_CAP) {
    const oldest = savedPositions.keys().next().value;
    if (oldest === undefined) break;
    savedPositions.delete(oldest);
  }
}

/** The position saved under `key`: null at the top, undefined when none
 * was saved. */
export function savedReadingPosition(key: string): ReadingAnchor | null | undefined {
  return savedPositions.get(key);
}

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
  };
}

export function resetReviewScrollPositionsForTest(): void {
  savedPositions.clear();
}
