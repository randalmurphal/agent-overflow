// Geometry for reordering a vertical list by dragging one item. Positions are
// in one coordinate space captured when the drag starts; the caller adds the
// pointer and scroll movement to the held item's middle.

/** `items` with the entry at `from` moved to index `to`. */
export function moveItem<T>(items: readonly T[], from: number, to: number): T[] {
  const next = [...items];
  const [moved] = next.splice(from, 1);
  next.splice(to, 0, moved);
  return next;
}

/**
 * The index the held item lands at: the count of the other items whose
 * middle is above the held item's middle.
 */
export function dropIndex(middles: readonly number[], from: number, heldMiddle: number): number {
  let index = 0;
  for (let i = 0; i < middles.length; i++) {
    if (i !== from && middles[i] < heldMiddle) index++;
  }
  return index;
}

/**
 * How far the item at `index` moves to open the slot at `to` for the item
 * held from `from`; `span` is the held item's height plus the list gap.
 */
export function displacement(index: number, from: number, to: number, span: number): number {
  if (from < to && index > from && index <= to) return -span;
  if (to < from && index >= to && index < from) return span;
  return 0;
}
