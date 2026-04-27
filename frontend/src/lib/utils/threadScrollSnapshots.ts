// Per-thread scroll position cache. Persists where the user was looking
// across thread switches inside a single session. Bounded LRU-style: the
// oldest entry is evicted when the cache exceeds MAX_ENTRIES so a long-
// lived session doesn't grow without bound.
//
// The snapshot is a two-shape discriminated union — `'bottom'` records
// "user was at the geometric bottom" (restore by scrolling to bottom +
// stickiness on); `'anchor'` records the first visible item's id and
// offset (restore by paging back until the item is loaded, then matching
// its rect.top to the recorded offset).

export type ScrollSnapshot =
  | { kind: 'bottom' }
  | { kind: 'anchor'; itemId: string; offsetTop: number };

const MAX_ENTRIES = 100;
const snapshots = new Map<string, ScrollSnapshot>();

export function setThreadScrollSnapshot(threadId: string, snapshot: ScrollSnapshot): void {
  // Re-insert to bump LRU position.
  if (snapshots.has(threadId)) {
    snapshots.delete(threadId);
  }
  snapshots.set(threadId, snapshot);
  while (snapshots.size > MAX_ENTRIES) {
    const oldest = snapshots.keys().next().value;
    if (oldest === undefined) break;
    snapshots.delete(oldest);
  }
}

export function getThreadScrollSnapshot(threadId: string): ScrollSnapshot | undefined {
  return snapshots.get(threadId);
}

export function clearThreadScrollSnapshotsForTest(): void {
  snapshots.clear();
}
