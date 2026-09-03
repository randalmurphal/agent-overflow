// In-flight LOCAL writes to a thread's read marker.
//
// `lastReadAt` is the one thread-row field where "newer" is not "larger".
// A read stamp is a timestamp, so a later read is a bigger number — but
// explicit unread is persisted as epoch 0, which is the SMALLEST value
// the field takes and means the most recent thing that happened to it.
// So the merge in `eventsThreadRows.ts` cannot decide between a wire row
// and a local one from the values alone.
//
// It used to try: any 0 in the merge won, from any source, forever. That
// is not a rule about time. A cached 0 was folded back into every later
// merge, so once ANY client marked a thread unread, this client's row
// could never read as read again — another device opening the thread
// broadcast a real timestamp and the cached 0 absorbed it, with no event
// able to clear it short of a reload (2026-09-03).
//
// What actually distinguishes "my unread is newer" from "a 0 that is
// already stale" is whether THIS page load has a write in flight, which
// is a fact only the writer can state. That is what this module holds:
// the value the caller is writing, for as long as it is writing it.
// Afterwards the wire row is the authority, which is what lets every
// client converge on whatever the backend last persisted.
//
// A counter rather than a boolean, the shape `pendingPreferenceMutations`
// uses in `editors.svelte.ts`: two writes on one thread can overlap (a
// read-mark settling while a mark-unread is in flight) and the first to
// settle must not clear the second's claim. The newest claim's VALUE is
// the one that wins, because it is the newest thing this client did.
//
// Plain module state, not a rune: it is read from the event fan-out while
// merging a row, never from a template, so a signal here would wake every
// sidebar reader for a write nobody renders.

interface ReadMarkerClaim {
  count: number;
  lastReadAt: number | undefined;
}

const claims = new Map<string, ReadMarkerClaim>();

/**
 * Claim the read marker of `threadId` at `lastReadAt` for the duration of
 * a local write, and answer the release.
 *
 * Call it AROUND the whole optimistic write — the RPC and the local patch
 * both — not just the RPC: the window a stale wire row lands in covers
 * either half on its own.
 *
 * The release is idempotent, so a caller may hand it to a `finally` and a
 * teardown path both.
 */
export function claimLocalReadMarker(
  threadId: string,
  lastReadAt: number | undefined,
): () => void {
  if (!threadId) return () => {};
  const existing = claims.get(threadId);
  if (existing) {
    existing.count += 1;
    existing.lastReadAt = lastReadAt;
  } else {
    claims.set(threadId, { count: 1, lastReadAt });
  }
  let released = false;
  return () => {
    if (released) return;
    released = true;
    const claim = claims.get(threadId);
    if (!claim) return;
    claim.count -= 1;
    if (claim.count <= 0) claims.delete(threadId);
  };
}

/**
 * Run `write` with the read marker claimed. The claim outlives the whole
 * operation including its local patch, and is released even if it throws.
 */
export async function withLocalReadMarker<T>(
  threadId: string,
  lastReadAt: number | undefined,
  write: () => Promise<T>,
): Promise<T> {
  const release = claimLocalReadMarker(threadId, lastReadAt);
  try {
    return await write();
  } finally {
    release();
  }
}

/**
 * The read marker this page load is currently writing for `threadId`, if
 * any. `held` false means the wire row is the authority.
 */
export function pendingLocalReadMarker(
  threadId: string,
): { held: boolean; lastReadAt: number | undefined } {
  const claim = claims.get(threadId);
  if (!claim) return { held: false, lastReadAt: undefined };
  return { held: true, lastReadAt: claim.lastReadAt };
}

/** Test-only. Drops a claim a torn-down component never released. */
export function resetLocalReadMarkersForTest(): void {
  claims.clear();
}
