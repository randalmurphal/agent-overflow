// stores/threadFlushRowReveal.ts
//
// "Is this flushed message's row on screen?" — the predicate behind the
// send-queue preview's Zone 2 XOR timeline invariant, kept next to the
// pane because both halves of the answer belong to a pane: the loaded
// item window (a row refused admission is not in the timeline, whatever
// SQLite holds) and the reveal gate's boundary (a row withheld behind a
// still-draining smoother has not mounted yet).
//
// Quiet reservations stay in the preview until confirmation or interrupt
// promotion. Confirmed rows follow `sliceRevealedNodes`: their position must
// be at or before the boundary, or the boundary must be absent.

import type { Item } from '../types/models';
import type { RevealBoundary } from '../utils/subagentGrouping';
import type { FlushedItem } from './sendQueue.svelte';
import { compareItemToCursor } from './threadItems';
import { isPendingFlushRow } from '../utils/userMessageMeta';

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
 * The Zone 2 entries whose rows this pane currently renders. Returns an
 * empty array for the overwhelmingly common case (no pending entries),
 * so the caller's chokepoint costs one registry read per reveal pass.
 */
export function renderedFlushedUserItemIds(
  pending: readonly FlushedItem[],
  getItemById: (itemId: string) => Item | undefined,
  revealBoundary: RevealBoundary | null,
): string[] {
  if (pending.length === 0) return [];
  const rendered: string[] = [];
  for (const entry of pending) {
    const item = getItemById(entry.userItemId);
    if (!item) continue;
    if (!itemIsRevealed(item, revealBoundary)) continue;
    rendered.push(entry.userItemId);
  }
  return rendered;
}
