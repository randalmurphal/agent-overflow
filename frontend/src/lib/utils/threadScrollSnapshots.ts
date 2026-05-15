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

function snapshotsEqual(a: ScrollSnapshot | undefined, b: ScrollSnapshot): boolean {
  if (a === undefined) return false;
  if (a.kind !== b.kind) return false;
  if (a.kind === 'bottom' || b.kind === 'bottom') return a.kind === b.kind;
  return a.itemId === b.itemId && a.offsetTop === b.offsetTop;
}

export function setThreadScrollSnapshot(threadId: string, snapshot: ScrollSnapshot): void {
  // No-op when the new value is identical to the existing record.
  // Streaming writes one snapshot per virtua onscroll event during
  // auto-follow at the bottom; without this guard, the LRU dance
  // (delete + set + size check) churns dozens of times per second
  // for no information gain.
  if (snapshotsEqual(snapshots.get(threadId), snapshot)) return;
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

export function clearThreadScrollSnapshot(threadId: string): void {
  snapshots.delete(threadId);
}

export function clearThreadScrollSnapshotsForTest(): void {
  snapshots.clear();
}
