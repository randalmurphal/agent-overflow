// Key-sequence diffing for the virtualizer adapter: decides whether a
// data change is a pure head/tail splice (the cheap applyLength path) or
// a keyed change (reorder / mid-list insert) that must remap measured
// sizes by row identity (engine.applyKeyedReorder).
//
// This exists because the adapter's length-only change detection missed
// same-length reorders entirely: a timeline upsert that changes an
// item's sort position (e.g. a queued message repositioned to the turn
// tail) re-sorts the items array without changing its length, and the
// engine kept serving position-keyed measurements from the old
// arrangement — rows below the move point rendered at wrong offsets
// (overlapping) until an unrelated remeasure.

/**
 * True when `next` is `prev` with rows only added/removed at the head
 * (`headSplice` > 0 inserts, < 0 removes — the `shift` contract) or at
 * the tail (headSplice 0), with every surviving key at an unchanged
 * relative position. Pure changes take the engine's applyLength path;
 * anything else needs a keyed remap.
 */
export function isPureHeadTailChange(
  prev: readonly unknown[],
  next: readonly unknown[],
  headSplice: number,
): boolean {
  if (headSplice !== 0 && next.length !== prev.length + headSplice) return false;
  if (headSplice > 0) {
    for (let i = 0; i < prev.length; i++) {
      if (next[i + headSplice] !== prev[i]) return false;
    }
    return true;
  }
  if (headSplice < 0) {
    for (let i = 0; i < next.length; i++) {
      if (next[i] !== prev[i - headSplice]) return false;
    }
    return true;
  }
  const shared = Math.min(prev.length, next.length);
  for (let i = 0; i < shared; i++) {
    if (next[i] !== prev[i]) return false;
  }
  return true;
}

/**
 * Builds `prevIndexByNewIndex` for engine.applyKeyedReorder:
 * `result[i]` = the index `next[i]`'s key had in `prev`, or -1 for a key
 * not present before (its row starts unmeasured). Duplicate keys map to
 * the LAST occurrence in `prev` — the keyed `{#each}` would already be
 * broken by duplicates, so no extra handling is owed here.
 */
export function keyedReorderPermutation(
  prev: readonly unknown[],
  next: readonly unknown[],
): number[] {
  const prevIndexByKey = new Map<unknown, number>();
  for (let i = 0; i < prev.length; i++) prevIndexByKey.set(prev[i], i);
  const permutation = new Array<number>(next.length);
  for (let i = 0; i < next.length; i++) {
    permutation[i] = prevIndexByKey.get(next[i]) ?? -1;
  }
  return permutation;
}
