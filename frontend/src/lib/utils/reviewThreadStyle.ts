import type { ReviewThread } from '../types/models';

// One vocabulary for a thread's state on every review surface: the
// Conversation card and the inline diff strip read the same classes, so
// a warning edge means "unresolved" wherever it appears.

export type ReviewThreadState = 'unresolved' | 'resolved' | 'outdated' | 'none';

/** A thread's display state; `orphaned` (line gone from the diff) reads
 * as outdated. Non-resolvable threads (PR-level conversation) are `none`. */
export function reviewThreadState(thread: ReviewThread, orphaned = false): ReviewThreadState {
  if (!thread.isResolvable) return 'none';
  if (thread.isOutdated || orphaned) return 'outdated';
  return thread.isResolved ? 'resolved' : 'unresolved';
}

/** The card: edge color carries the state; outdated threads go dashed. */
export const THREAD_CARD_CLASS: Record<ReviewThreadState, string> = {
  unresolved: 'border-warning/50 border-l-[3px] border-l-warning',
  resolved: 'border-success/35 border-l-[3px] border-l-success/70',
  outdated: 'border-dashed border-border-strong',
  none: 'border-border',
};

/** The header strip inside the card. */
export const THREAD_HEAD_CLASS: Record<ReviewThreadState, string> = {
  unresolved: 'bg-warning/8',
  resolved: 'bg-success/6',
  outdated: 'bg-surface-2/40',
  none: 'bg-surface-2/60',
};

/** The state chip; `none` renders no chip. */
export const THREAD_CHIP_CLASS: Record<ReviewThreadState, string> = {
  unresolved: 'bg-warning/12 text-warning',
  resolved: 'bg-success/12 text-success',
  outdated: 'bg-surface-2 text-fg-muted',
  none: '',
};

/** Settled threads stay readable but recede. */
export function threadBodyClass(state: ReviewThreadState): string {
  return state === 'resolved' || state === 'outdated' ? 'opacity-80' : '';
}
