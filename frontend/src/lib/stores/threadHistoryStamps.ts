// Per-thread history invalidation stamps
// (docs/architecture/thread-replica-sync.md §3): the `(epoch, rev)` pair the
// backend advances on every persisted mutation of a thread's items, and
// the `attested` provenance flag that grades it.
//
// Not reactive on purpose. Nothing renders from a stamp — it is read
// imperatively when a window sync is issued and when a window is cached,
// so a `$state` box would only subscribe readers to a value they never
// display.
//
// The grading (§3.4, "understate, never overstate"):
//
//  - **Attested** stamps come from a `SyncThreadWindow` response, the
//    one place stamps and rows are read in a single transaction.
//  - **Unattested** stamps come from `provider:turn_completed` and
//    `user_message:reverted`. A writer outside the emitting goroutine
//    (the async highlight-span worker) can commit a rev bump before the
//    stamp is read while its frame arrives later — or never. In memory
//    that window is milliseconds and self-heals through frame delivery,
//    replay, or the gap rule below; persisted, it would be a permanent
//    false `fresh` over content missing that write.
//  - **Unknown** is the state a transport gap forces. An understated
//    stamp costs one redundant window fetch; there is no such thing as a
//    cheap overstatement.
//
// This registry holds ONE stamp per thread — the newest from any source,
// which is what the next sync REQUEST sends so an unchanged thread can
// answer `fresh`. What a replica WRITE pairs with a window deliberately
// does not live here: attestation is a property of a WINDOW, not of a
// thread id (§3.4). A thread-keyed "newest attested" answer can name a
// page a given pane never received — its write-back never fired, the
// pane later repainted from an older replica envelope, and the sync that
// would have converged them threw — so pairing it with whatever rows a
// pane happens to hold is a permanent false `fresh`. The write gate is
// the attestation the pane carries for the rows it is persisting
// (`windowAttestation` in threadSwitchLoad.svelte.ts); a registry slot for it was
// deleted for exactly that reason — do not add one back.
//
// The `attested` FLAG on the held stamp is still load-bearing here: it
// is copied into L1 snapshots (so a restored window knows its stamp's
// grade) and it is what lets a page-less `fresh` echo confirm a painted
// window, or refuse to.
import { onBackendIdentity } from '../transport/backendIdentity';

export interface ThreadHistoryStamp {
  epoch: number;
  rev: number;
  /** True only for stamps a `SyncThreadWindow` response attested. */
  attested: boolean;
}

/** Wire value for "no stamp" — never equal to a real one, so the
 *  backend always answers with a page. */
export const UNKNOWN_STAMP_VALUE = -1;

const stamps = new Map<string, ThreadHistoryStamp>();

/** The stamp to send as `haveEpoch`/`haveRev` on the next sync. */
export function getThreadHistoryStamp(threadId: string): ThreadHistoryStamp | null {
  return stamps.get(threadId) ?? null;
}

/**
 * Record the stamp a sync response attested for the page it returned —
 * or, for `fresh`, echoed against a stamp that was ITSELF attested.
 * The caller must have applied that page (or be keeping the attested
 * rows the stamp matched) before calling. A `fresh` echo of an
 * event-carried stamp must NOT land here: the server only confirmed
 * its own counter, not that this client received every frame up to it
 * — record it via adoptEventStamp and it stays unattested.
 */
export function recordAttestedStamp(threadId: string, epoch: number, rev: number): void {
  if (!threadId) return;
  if (!Number.isFinite(epoch) || !Number.isFinite(rev)) return;
  stamps.set(threadId, { epoch, rev, attested: true });
}

/**
 * Adopt an event-carried stamp. Zero means "no stamp" on the wire (the
 * thread row was gone when the event was built) and is ignored, as is
 * any stamp older than the one already held — events can arrive out of
 * order relative to a sync response that raced them. An equal-or-newer
 * event stamp REPLACES an attested one, downgrading the grade: the
 * event proves the backend moved past (or to) what was attested, and
 * only a sync response may claim attestation.
 */
export function adoptEventStamp(threadId: string, epoch: unknown, rev: unknown): void {
  if (!threadId) return;
  if (typeof epoch !== 'number' || typeof rev !== 'number') return;
  if (!Number.isFinite(epoch) || !Number.isFinite(rev)) return;
  if (epoch <= 0 && rev <= 0) return;
  const existing = stamps.get(threadId);
  if (existing && (existing.rev > rev || existing.epoch > epoch)) return;
  stamps.set(threadId, { epoch, rev, attested: false });
}

/**
 * Drop a thread's stamp to unknown. Called when we can no longer say
 * what the backend's counters were: a transport gap on a stamped or
 * content-bearing channel, or a mutation we applied without a stamp to
 * go with it.
 */
export function dropThreadHistoryStamp(threadId: string): void {
  stamps.delete(threadId);
}

/** Drop every stamp — reconnect-wide gap, or a backend generation change. */
export function dropAllThreadHistoryStamps(): void {
  stamps.clear();
}

// A generation re-mint means the backend's counters no longer continue
// the sequence these stamps were read from, so every one of them is a
// claim about a history that no longer exists.
onBackendIdentity(() => {
  dropAllThreadHistoryStamps();
});

export function __resetThreadHistoryStampsForTest(): void {
  stamps.clear();
}
