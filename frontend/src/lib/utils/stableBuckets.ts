/**
 * Give every key of `next` whose bucket holds the same elements in the same
 * order as before the previous bucket array. A map of buckets is re-minted
 * whenever any element changes, so a consumer handed one bucket would
 * otherwise wake for a change in any of them.
 */
export function reuseUnchangedBuckets<K, V>(previous: ReadonlyMap<K, V[]>, next: Map<K, V[]>): Map<K, V[]> {
  for (const [key, bucket] of next) {
    const before = previous.get(key);
    if (before === undefined || before.length !== bucket.length) continue;
    if (before.every((item, index) => item === bucket[index])) next.set(key, before);
  }
  return next;
}
