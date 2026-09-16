// stores/threadFlushRowReveal.ts
//
// "Is this flushed message's row on screen?" — the predicate behind the
// send-queue preview's Zone 2 XOR timeline invariant, kept next to the
// pane because both halves of the answer belong to a pane: the loaded
// item window (a row refused admission is not in the timeline, whatever
// SQLite holds) and the reveal gate's boundary (a row withheld behind a
// still-draining smoother has not mounted yet).
//
// Mirrors `sliceRevealedNodes` (utils/subagentGrouping.ts): that slice
// cuts the projected nodes at the first one after the boundary, so a row
// renders exactly while its (turnIndex, itemIndex) is at or before the
// boundary — or while there is no boundary at all.

import type { Item } from '../types/models';
import type { RevealBoundary } from '../utils/subagentGrouping';
import type { FlushedItem } from './sendQueue.svelte';
import { compareItemToCursor } from './threadItems';

/** A loaded row renders iff the reveal gate is open past its position. */
export function itemIsRevealed(
  item: Item,
  revealBoundary: RevealBoundary | null,
): boolean {
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
