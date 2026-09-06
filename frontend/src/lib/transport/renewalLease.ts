// Mutual exclusion for one exchange, across every browsing context this
// browser has open on the same origin.
//
// It exists for exactly one caller: rotating a paired device's credential
// (./deviceSession.ts). That exchange is single-use on the backend and
// reuse of a spent refresh secret ENDS the session family
// (internal/identity's rotation discipline), so two tabs of one browser
// renewing at the same moment do not merely duplicate work: they log the
// person out of every tab, with no way back but pairing again. The
// in-realm single-flight Map that used to be the whole guard cannot see
// another tab: it is per JavaScript realm, and a second tab is a second
// realm over the same localStorage.
//
// Two implementations of one contract, because the good primitive is
// SECURE-CONTEXT ONLY. `navigator.locks` is exactly this and is what runs
// wherever it exists; a plain-HTTP LAN page (spec §15 constraint 6) has
// no Web Locks, no `crypto.subtle` and no IndexedDB, and it is a shipped
// context, so the fallback is a lease in the storage both tabs already
// share.
//
// The fallback is a lease and not a lock, and the difference is the point:
// a tab that is killed mid-exchange cannot release anything, so the entry
// EXPIRES rather than being held forever. Nothing here waits longer than
// that expiry, and the caller re-reads its own state inside the lease
// rather than trusting exclusion alone. Recoverable renewal additionally
// saves its successor before send and compares the generation after fetch.
// This protects a suspended tab whose lease expires during the exchange.

/** What the guarded work is told about its own standing. */
export interface LeaseHold {
  /**
   * Whether this context still holds the lease, RE-READ from storage.
   *
   * Asked immediately before the irreversible step (the POST), never
   * cached: two contexts claiming in the same instant can both read their
   * own write back, and this is where that tie is broken. A read rather
   * than a flag set by the storage event, because the event is delivered
   * to other contexts on the browser's own schedule and a verdict that
   * depended on having received one would be right only when it was not
   * needed. Always true under Web Locks, which grants exclusively.
   */
  held(): boolean;
}

// How long a lease stands without being released. Long enough to cover a
// slow renewal round trip on a phone's link, short enough that a tab
// killed mid-exchange does not strand the next one for a visible pause.
// Nothing waits longer than this.
const LEASE_TTL_MS = 8_000;

// How often a waiting context re-reads. 25ms is imperceptible against a
// network exchange and costs a handful of synchronous storage reads.
const LEASE_POLL_MS = 25;

// How many times a context that lost a simultaneous claim tries again.
// Two: one loss is a real tie, and a context that loses twice is looking
// at a busy origin rather than a race, so it proceeds unheld and its
// caller re-reads instead.
const LEASE_CLAIM_ATTEMPTS = 2;

interface LeaseEntry {
  /** Who holds it. Compared, never interpreted. */
  id: string;
  /** When it was claimed, for the expiry above. */
  at: number;
}

function readEntry(key: string): LeaseEntry | null {
  if (typeof localStorage === 'undefined') return null;
  let raw: string | null;
  try {
    raw = localStorage.getItem(key);
  } catch {
    return null;
  }
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw) as LeaseEntry;
    if (typeof parsed.id !== 'string' || typeof parsed.at !== 'number') return null;
    return parsed;
  } catch {
    // A damaged entry is not a lease. Treating it as one would wedge
    // every context on this origin until it happened to be overwritten.
    return null;
  }
}

function writeEntry(key: string, entry: LeaseEntry | null): void {
  if (typeof localStorage === 'undefined') return;
  try {
    if (entry === null) localStorage.removeItem(key);
    else localStorage.setItem(key, JSON.stringify(entry));
  } catch {
    // Storage refused (private mode, quota). Exclusion then degrades to
    // the in-realm guard the caller already has, which is what this
    // browser had before this file existed.
  }
}

// Whether a live lease is held by somebody other than `ours`. An empty
// `ours` is "anybody at all", which is what a context waiting for its turn
// asks before it has an id of its own.
function heldByAnother(key: string, ours: string): boolean {
  const entry = readEntry(key);
  if (entry === null || entry.id === ours) return false;
  // An entry older than the TTL belongs to a context that is gone. Time
  // is the only release such a context can perform.
  return Date.now() - entry.at < LEASE_TTL_MS;
}

/** Resolve once no other context holds a live lease, bounded by the TTL. */
async function waitForFree(key: string): Promise<void> {
  const deadline = Date.now() + LEASE_TTL_MS;
  while (heldByAnother(key, '') && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, LEASE_POLL_MS));
  }
}

function mintLeaseID(): string {
  const bytes = new Uint8Array(8);
  crypto.getRandomValues(bytes);
  let id = '';
  for (const b of bytes) id += b.toString(16).padStart(2, '0');
  return id;
}

/**
 * Claim the lease, answering the id that was written and whether this
 * context is the one holding it.
 *
 * Write, then RE-READ: two contexts writing in the same instant both
 * succeed, and the second write is what the storage ends up holding, so
 * reading back is what tells them apart. A macrotask hop before the read
 * gives the other write time to land rather than deciding on an ordering
 * this context cannot see.
 */
async function claim(key: string): Promise<{ id: string; ours: boolean }> {
  let id = mintLeaseID();
  for (let attempt = 0; attempt < LEASE_CLAIM_ATTEMPTS; attempt++) {
    await waitForFree(key);
    id = mintLeaseID();
    writeEntry(key, { id, at: Date.now() });
    await new Promise((resolve) => setTimeout(resolve, 0));
    if (readEntry(key)?.id === id) return { id, ours: true };
  }
  // Lost every claim. The caller runs anyway, unheld: by now the winner
  // has finished and the caller's first act is to re-read the state it
  // was about to change, so the common outcome is that there is nothing
  // left to do.
  await waitForFree(key);
  return { id, ours: false };
}

/**
 * Run `work` with this origin's other browsing contexts excluded from the
 * same lease.
 *
 * The lease is ADVISORY and the caller must treat it that way: `work` is
 * always invoked, and `hold.held()` is what it consults before doing
 * anything it cannot take back. That shape is deliberate: a lock that
 * could refuse to run the work would turn a busy origin into a renewal
 * that never happens, and the caller's own re-read makes running twice
 * harmless anyway.
 */
export async function withRenewalLease<T>(
  key: string,
  work: (hold: LeaseHold) => Promise<T>,
): Promise<T> {
  const locks = typeof navigator === 'undefined' ? undefined : navigator.locks;
  if (locks && typeof locks.request === 'function') {
    // Web Locks grants exclusively and queues the rest, so a second
    // context runs AFTER this one rather than beside it, and the grant is
    // released even if this context is killed. Nothing to re-read and
    // nothing to expire.
    return await locks.request(key, () => work({ held: () => true }));
  }
  const { id, ours } = await claim(key);
  try {
    return await work({ held: () => ours && readEntry(key)?.id === id });
  } finally {
    // Only ever release OUR entry: a lease that expired and was re-claimed
    // by somebody else belongs to them now, and removing it would put two
    // contexts inside at once.
    if (readEntry(key)?.id === id) writeEntry(key, null);
  }
}

/**
 * The key one backend's renewal lease lives under: the session slot's own
 * storage key plus a suffix, so a browser paired with two machines leases
 * them independently and the two spellings cannot drift apart.
 */
export function renewalLeaseKey(sessionKey: string): string {
  return `${sessionKey}:renewing`;
}
