// stores/threadFlushRowReveal.ts
//
// "Is this flushed message's row in the timeline?" — the predicate behind the
// send-queue preview's Zone 2 XOR timeline invariant, kept next to the
// pane because both halves of the answer belong to a pane: the loaded
// item window (a row refused admission is not in the timeline, whatever
// SQLite holds) and the reveal gate's boundary (a row withheld behind a
// still-draining smoother has not mounted yet).
//
// Quiet reservations stay in the preview until confirmation or interrupt
// promotion. Confirmed rows follow `sliceRevealedNodes`: their position must
// be at or before the boundary, or the boundary must be absent. A held row
// outside the loaded window (`itemWithinLoadedWindow`, the projection's own
// rule) is not in the timeline either, however the reveal gate stands: a
// queued message the backend moved past the window's newest cursor is such a
// row until the cursor follows it. A confirmed row before the window's oldest
// edge is history the pane pages back to, so it is in the timeline already:
// a pane that does not hold it learns its position from `deliveredAt`.

import type { Item } from '../types/models';
import type { RevealBoundary } from '../utils/subagentGrouping';
import type { FlushedItem } from './sendQueue.svelte';
import { compareCursors, compareItemToCursor, itemWithinLoadedWindow, type TimelineCursorLike } from './threadItems';
import { isPendingFlushRow } from '../utils/userMessageMeta';

/** The pane's loaded-window edges, as the projection reads them. */
export interface LoadedWindowEdges {
  oldest: TimelineCursorLike | null;
  newest: TimelineCursorLike | null;
}

/** A loaded, confirmed row renders when the reveal gate passes its position. */
export function itemIsRevealed(
  item: Item,
  revealBoundary: RevealBoundary | null,
): boolean {
  if (isPendingFlushRow(item)) return false;
  if (revealBoundary === null) return true;
  return compareItemToCursor(item, revealBoundary) <= 0;
}

/**
 * The Zone 2 entries whose rows are in this pane's timeline: held, inside
 * the loaded window and past the reveal gate, or confirmed before the
 * window's oldest edge. Returns an empty array for the overwhelmingly
 * common case (no pending entries), so the caller's chokepoint costs one
 * registry read per reveal pass.
 */
export function flushedUserItemIdsInTimeline(
  pending: readonly FlushedItem[],
  getItemById: (itemId: string) => Item | undefined,
  revealBoundary: RevealBoundary | null,
  window: LoadedWindowEdges,
): string[] {
  if (pending.length === 0) return [];
  const inTimeline: string[] = [];
  for (const entry of pending) {
    const item = getItemById(entry.userItemId);
    const delivered = item && !isPendingFlushRow(item) ? item : entry.deliveredAt;
    if (delivered && window.oldest && compareCursors(delivered, window.oldest) < 0) {
      inTimeline.push(entry.userItemId);
      continue;
    }
    if (!item) continue;
    if (!itemWithinLoadedWindow(item, window.oldest, window.newest)) continue;
    if (!itemIsRevealed(item, revealBoundary)) continue;
    inTimeline.push(entry.userItemId);
  }
  return inTimeline;
}
