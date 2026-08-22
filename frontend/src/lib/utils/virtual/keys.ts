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
 * Builds `prevIndexByNewIndex` for engine.applyKeyedReorder:
 * `result[i]` = the index `next[i]`'s key had in `prev`, or -1 for a key
 * not present before (its row starts unmeasured). The adapter rejects
 * duplicate keys before this function, so every match is unambiguous.
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

export interface KeyedSequenceMutation {
  kind: 'unchanged' | 'tail' | 'head' | 'keyed';
  /** Positive for a prepend, negative for a head removal. */
  headSplice: number;
}

function prefixMatches(shorter: readonly unknown[], longer: readonly unknown[]): boolean {
  if (shorter.length > longer.length) return false;
  for (let i = 0; i < shorter.length; i++) {
    if (shorter[i] !== longer[i]) return false;
  }
  return true;
}

function suffixMatches(shorter: readonly unknown[], longer: readonly unknown[]): boolean {
  if (shorter.length > longer.length) return false;
  const shift = longer.length - shorter.length;
  for (let i = 0; i < shorter.length; i++) {
    if (shorter[i] !== longer[i + shift]) return false;
  }
  return true;
}

/**
 * Derives structural intent from keyed identity. Callers cannot mislabel a
 * head mutation as a tail mutation or leave a one-flush hint armed.
 */
export function classifyKeyedSequenceMutation(
  prev: readonly unknown[],
  next: readonly unknown[],
): KeyedSequenceMutation {
  if (prev.length === next.length) {
    return prefixMatches(prev, next)
      ? { kind: 'unchanged', headSplice: 0 }
      : { kind: 'keyed', headSplice: 0 };
  }
  if (prefixMatches(prev, next) || prefixMatches(next, prev)) {
    return { kind: 'tail', headSplice: 0 };
  }
  if (suffixMatches(prev, next) || suffixMatches(next, prev)) {
    return { kind: 'head', headSplice: next.length - prev.length };
  }
  return { kind: 'keyed', headSplice: 0 };
}
