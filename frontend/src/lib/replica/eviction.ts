// Replica-wide LRU planning. Pure so the bound is testable without a
// storage backend: given the accounting rows and the row about to be
// written, decide which threads leave.
//
// "Least recently used" is `savedAt` — the moment a window was last
// attested and persisted — not the moment it was last read. A read
// costs nothing and does not make a window fresher; a write does.
//
// Both planners run INSIDE the commit transaction, against the stored
// accounting record rather than the page-local mirror (see idb.ts
// `commitEnvelope` / `commitRemoval`): a second page on this origin
// shares one database, so the mirror is only ever this page's view of
// it. That is also why removal plans, not just writes, re-enforce the
// caps — an entry this page never wrote still spends the budget, and
// every index write must leave the stored set inside the bounds.
import { MAX_REPLICA_CHARS, MAX_REPLICA_THREADS } from './envelope';
import type { IndexMerge, ReplicaIndexEntry } from './idb';

/**
 * Enforce the replica-wide caps over `entries`, dropping oldest-`savedAt`
 * first. `pinned` (the row being written) is held out of the sort and
 * never evicted — a window we just attested is the freshest thing in the
 * database, and dropping it would make the write pointless.
 */
function sweep(
  entries: readonly ReplicaIndexEntry[],
  pinned: ReplicaIndexEntry | null,
): IndexMerge {
  const kept = entries
    .filter((entry) => entry.threadId !== pinned?.threadId)
    .sort((a, b) => a.savedAt - b.savedAt);
  const evict: string[] = [];
  const pinnedCount = pinned ? 1 : 0;
  let chars = pinned?.chars ?? 0;
  for (const entry of kept) chars += entry.chars;

  while (kept.length + pinnedCount > MAX_REPLICA_THREADS || chars > MAX_REPLICA_CHARS) {
    const oldest = kept.shift();
    if (!oldest) break;
    chars -= oldest.chars;
    evict.push(oldest.threadId);
  }

  return { entries: pinned ? [...kept, pinned] : kept, evict };
}

/**
 * Plan the write of `incoming` against `current`. The incoming row
 * replaces any existing row for the same thread.
 */
export function planWrite(
  current: readonly ReplicaIndexEntry[],
  incoming: ReplicaIndexEntry,
): IndexMerge {
  return sweep(current, incoming);
}

/** Accounting rows after dropping one thread, re-swept against the caps. */
export function planRemoval(
  current: readonly ReplicaIndexEntry[],
  threadId: string,
): IndexMerge {
  return sweep(
    current.filter((entry) => entry.threadId !== threadId),
    null,
  );
}
