import type { ReviewThread } from '../types/models';

// One vocabulary for a thread's state on every review surface: the
// Conversation card and the inline diff strip read the same classes, so
// a warning edge means "unresolved" wherever it appears. Outdated is a
// MARKER layered on the state, not a state of its own: a thread written
// against an earlier revision is still open or settled, still counted,
// stepped and resolvable; the dashed edge and the chip say the code moved.

export type ReviewThreadState = 'unresolved' | 'resolved' | 'none';

/** A thread's resolve state. Non-resolvable threads (PR-level
 * conversation) are `none`. */
export function reviewThreadState(thread: ReviewThread): ReviewThreadState {
  if (!thread.isResolvable) return 'none';
  return thread.isResolved ? 'resolved' : 'unresolved';
}

/** The thread was written against code that has since moved: the forge
 * says so (`isOutdated`), or its line is gone from the diff on screen
 * (`orphaned`). */
export function reviewThreadOutdated(thread: ReviewThread, orphaned = false): boolean {
  return thread.isOutdated || orphaned;
}

/** The card: edge color carries the state. */
export const THREAD_CARD_CLASS: Record<ReviewThreadState, string> = {
  unresolved: 'border-warning/50 border-l-[3px] border-l-warning',
  resolved: 'border-success/35 border-l-[3px] border-l-success/70',
  none: 'border-border',
};

/** Layered on the card of an outdated thread: the edge goes dashed. */
export const THREAD_OUTDATED_CARD_CLASS = 'border-dashed';

/** The header strip inside the card. */
export const THREAD_HEAD_CLASS: Record<ReviewThreadState, string> = {
  unresolved: 'bg-warning/8',
  resolved: 'bg-success/6',
  none: 'bg-surface-2/60',
};

/** The state chip; `none` renders no chip. */
export const THREAD_CHIP_CLASS: Record<ReviewThreadState, string> = {
  unresolved: 'bg-warning/12 text-warning',
  resolved: 'bg-success/12 text-success',
  none: '',
};

/** The outdated chip beside the state chip. */
export const THREAD_OUTDATED_CHIP_CLASS = 'bg-surface-2 text-fg-muted';

/** The outdated chip's hovertext: where the thread was written, when the
 * forge still knows (GitHub reports the original line; GitLab keeps the
 * position itself). */
export function outdatedTitle(thread: ReviewThread): string {
  const line = thread.line ?? thread.originalLine;
  return line
    ? `Written against an earlier revision, at line ${line} then`
    : 'Written against an earlier revision';
}

/** Settled threads stay readable but recede. */
export function threadBodyClass(state: ReviewThreadState): string {
  return state === 'resolved' ? 'opacity-80' : '';
}
