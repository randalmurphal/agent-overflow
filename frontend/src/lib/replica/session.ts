// The replica's public surface: an IndexedDB copy of recently viewed
// thread windows that paints before `SyncThreadWindow` answers and is
// replaced by the page it returns
// (docs/architecture/thread-replica-sync.md §6).
//
// Three invariants live here rather than in the callers:
//
//  - **Identity gates everything.** The database is keyed by
//    `backendId`; a `replicaGeneration` change clears it wholesale. Both
//    arrive on the bootstrap manifest and are re-validated on every
//    reconnect, so a restored backend cannot serve rows minted against
//    a divergent future. Reads carry the identity token they were issued
//    under and resolve null when it has moved on.
//  - **Failures are loud, then final.** Every IndexedDB error is logged
//    and reported through the frontend error path; after
//    MAX_CONSECUTIVE_FAILURES the replica disables itself for the
//    session and every operation becomes a no-op. Degraded behaviour is
//    exactly today's cold open, so failing closed costs paint latency
//    and nothing else.
//  - **Nothing is migrated.** Envelope version, schema version and
//    generation mismatches drop records (or the whole database). See
//    envelope.ts.
//  - **The database outranks this page.** One origin can have more than
//    one page, so `session.index` is a mirror and never the authority:
//    commits re-read and merge the stored accounting record inside their
//    own transaction and hand the union back. The mirror exists so the
//    hot removal path can answer "do we hold this thread?" without a
//    transaction, and it is marked dirty (re-read lazily) whenever a
//    commit rejects without saying whether it landed.
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import {
  UNKNOWN_BACKEND_IDENTITY,
  onBackendIdentity,
  type BackendIdentity,
} from '../transport/backendIdentity';
import {
  REPLICA_SCHEMA_VERSION,
  bodyFitsCaps,
  estimateBodyChars,
  metaMatches,
  normalizeBody,
  readEnvelope,
  wrapEnvelope,
  type ReplicaBody,
  type ReplicaMeta,
} from './envelope';
import { planRemoval, planWrite } from './eviction';
import {
  META_IDENTITY_KEY,
  META_INDEX_KEY,
  META_STORE,
  THREADS_STORE,
  clearStores,
  commitEnvelope,
  commitRemoval,
  deleteDatabase,
  deleteEnvelopes,
  indexedDbAvailable,
  listDatabaseNames,
  openReplicaDb,
  readRecord,
  readThreadKeys,
  writeRecord,
  type ReplicaIndexEntry,
} from './idb';
import { replicaDatabaseName, unclaimedReplicaDatabases } from './purge';

/**
 * Consecutive failed operations before the replica gives up for the
 * session. One transient error (a quota blip, a backgrounded tab losing
 * its storage handle) should not cost the durable cache; a backend that
 * keeps failing is one the cold-open path must stop waiting on.
 */
const MAX_CONSECUTIVE_FAILURES = 3;

interface ReplicaSession {
  /** Monotonic token; every identity change invalidates in-flight work. */
  token: number;
  identity: BackendIdentity;
  db: IDBDatabase | null;
  /**
   * This page's view of the stored accounting record. A mirror, not the
   * truth: commits plan against the stored record inside their own
   * transaction (idb.ts `commitEnvelope`) and hand the merged rows back
   * to refresh this. It exists so the removal path can answer "does this
   * page hold an envelope for that thread?" without a transaction.
   */
  index: ReplicaIndexEntry[];
  /**
   * The mirror may no longer describe the stored record: a commit
   * rejected without telling us whether its transaction landed (a
   * watchdog can fire on a transaction that commits a moment later).
   * Re-read before the next reader of the mirror rather than guessing —
   * a mirror that has silently lost a row would let the removal path
   * short-circuit and strand that envelope until the next boot.
   */
  indexDirty: boolean;
  ready: Promise<void> | null;
}

let session: ReplicaSession = {
  token: 0,
  identity: { backendId: '', generation: '', name: '' },
  db: null,
  index: [],
  indexDirty: false,
  ready: null,
};
let disabled = false;
let consecutiveFailures = 0;

function isIndexEntry(value: unknown): value is ReplicaIndexEntry {
  if (!value || typeof value !== 'object') return false;
  const entry = value as Partial<ReplicaIndexEntry>;
  return (
    typeof entry.threadId === 'string' &&
    Number.isFinite(entry.savedAt) &&
    Number.isFinite(entry.chars)
  );
}

/**
 * Validate a stored accounting record. The one place the record's shape
 * is trusted — every read of it (boot, mirror re-sync, in-transaction
 * merge) goes through here, so a record left by a different build or a
 * corrupt write degrades to "fewer accounted rows", never to a throw
 * inside a transaction handler.
 */
function readIndexEntries(raw: unknown): ReplicaIndexEntry[] {
  if (!raw || typeof raw !== 'object') return [];
  const entries = (raw as { entries?: unknown }).entries;
  if (!Array.isArray(entries)) return [];
  return entries.filter(isIndexEntry);
}

function noteFailure(operation: string, err: unknown): void {
  consecutiveFailures += 1;
  const detail = err instanceof Error ? `${err.name}: ${err.message}` : String(err);
  console.error(`replica: ${operation} failed — ${detail}`);
  reportFrontendDiagnostic(`replica: ${operation} failed`, detail);
  if (consecutiveFailures >= MAX_CONSECUTIVE_FAILURES && !disabled) {
    disabled = true;
    console.error(
      `replica: disabled for this session after ${consecutiveFailures} consecutive failures`,
    );
    reportFrontendDiagnostic(
      'replica: disabled for this session',
      `${consecutiveFailures} consecutive IndexedDB failures`,
    );
    closeSession();
  }
}

function noteSuccess(): void {
  consecutiveFailures = 0;
}

function closeSession(): void {
  const db = session.db;
  session = { ...session, db: null, index: [], indexDirty: false, ready: null };
  if (db) {
    try {
      db.close();
    } catch (err) {
      console.error('replica: closing database failed', err);
    }
  }
}

/**
 * Close the connection AND move the token on, leaving the replica bound
 * to no backend. Both halves matter before deleting a database this page
 * has open: the connection is what would block the deletion, and the
 * token is what stops an in-flight commit re-creating what was deleted.
 * The next identity event re-opens; nothing else does.
 */
function detachSession(): void {
  const token = session.token + 1;
  closeSession();
  session = { ...session, token, identity: UNKNOWN_BACKEND_IDENTITY };
}

/**
 * Backend ids whose replica database must survive a sweep: the backends
 * this client is attached to. One today — the identity the bootstrap
 * manifest named — and the reason this is a SET rather than that single
 * id is that a multi-backend attach list (docs/specs/remote-access.md
 * §10) drops straight in here with nothing else to change.
 *
 * An unidentified backend contributes nothing, so the set can come back
 * empty. Callers must read that as "no live backend can be named", never
 * as "nothing is live" — the same rule backendIdentity.ts states for the
 * empty identity itself.
 */
function attachedBackendIds(): Set<string> {
  const ids = new Set<string>();
  if (session.identity.backendId !== '') ids.add(session.identity.backendId);
  return ids;
}

/** Name of the database this session currently has open, or ''. */
function openDatabaseName(): string {
  const backendId = session.identity.backendId;
  return backendId === '' ? '' : replicaDatabaseName(backendId);
}

/**
 * Point the replica at a backend. Opens (or re-opens) the per-backend
 * database, drops it wholesale when the stored generation or schema
 * version does not match, and rebuilds the in-memory accounting index.
 * An empty `backendId` or `generation` disables the replica.
 *
 * Returns once the session is usable — awaited by the operations below,
 * never by callers.
 */
export function initReplica(identity: BackendIdentity): Promise<void> {
  closeSession();
  const token = session.token + 1;
  session = {
    token,
    identity,
    db: null,
    index: [],
    indexDirty: false,
    ready: null,
  };
  if (disabled || !identity.backendId || !identity.generation || !indexedDbAvailable()) {
    return Promise.resolve();
  }
  const ready = (async () => {
    const db = await openReplicaDb(replicaDatabaseName(identity.backendId));
    if (session.token !== token) {
      db.close();
      return;
    }
    const meta = await readRecord<ReplicaMeta>(db, META_STORE, META_IDENTITY_KEY);
    if (session.token !== token) {
      db.close();
      return;
    }
    if (!metaMatches(meta, identity.generation)) {
      // Generation re-mint or a schema bump. Never migrated: the rows
      // describe a history this build cannot vouch for.
      await clearStores(db);
      await writeRecord(db, META_STORE, META_IDENTITY_KEY, {
        generation: identity.generation,
        schemaVersion: REPLICA_SCHEMA_VERSION,
      } satisfies ReplicaMeta);
      if (session.token !== token) {
        db.close();
        return;
      }
      session.db = db;
      session.index = [];
      return;
    }
    const stored = await readRecord<unknown>(db, META_STORE, META_INDEX_KEY);
    const keys = await readThreadKeys(db);
    if (session.token !== token) {
      db.close();
      return;
    }
    const known = new Set(keys);
    const index = readIndexEntries(stored).filter((entry) => known.has(entry.threadId));
    // Envelopes with no accounting row are unbounded storage: they can
    // never be swept because nothing knows their size. Drop them — as
    // ONE transaction, because this sits in front of the first replica
    // read of the session.
    const accounted = new Set(index.map((entry) => entry.threadId));
    const orphans = keys.filter((key) => !accounted.has(key));
    if (orphans.length > 0) {
      await deleteEnvelopes(db, orphans);
      if (session.token !== token) {
        db.close();
        return;
      }
    }
    session.db = db;
    session.index = index;
    session.indexDirty = false;
  })().then(
    () => {
      noteSuccess();
      scheduleUnclaimedSweep(token);
    },
    (err: unknown) => {
      if (session.token === token) closeSession();
      noteFailure('open', err);
    },
  );
  session.ready = ready;
  return ready;
}

/** What a purge did. Every field is a distinct outcome, not a detail. */
export interface ReplicaPurgeResult {
  /** Databases dropped. */
  deleted: readonly string[];
  /** Databases that were targeted and did not go (blocked, or wedged). */
  failed: readonly string[];
  /**
   * False when this engine cannot list the origin's databases, so only
   * the database this page had open could be considered (idb.ts
   * `listDatabaseNames`).
   */
  enumerated: boolean;
  /** True when an identity change cut the purge short. */
  cancelled: boolean;
}

/** One deletion failure: loud, but not evidence the read path is wedged. */
function notePurgeFailure(name: string, err: unknown): void {
  const detail = err instanceof Error ? `${err.name}: ${err.message}` : String(err);
  console.error(`replica: deleting ${name} failed — ${detail}`);
  reportFrontendDiagnostic(`replica: deleting ${name} failed`, detail);
}

/**
 * Delete `targets`, re-checking `token` before each one so an identity
 * change arriving mid-purge cancels the rest instead of deleting the
 * database that identity just opened. The session is already detached
 * from anything in this list — its caller does that before it decides
 * what the list is — so no target here can be an open database.
 */
async function deleteDatabases(
  targets: readonly string[],
  token: number,
): Promise<{ deleted: string[]; failed: string[]; cancelled: boolean }> {
  const deleted: string[] = [];
  const failed: string[] = [];
  for (const name of targets) {
    if (session.token !== token) return { deleted, failed, cancelled: true };
    try {
      await deleteDatabase(name);
      deleted.push(name);
    } catch (err) {
      failed.push(name);
      notePurgeFailure(name, err);
    }
  }
  return { deleted, failed, cancelled: false };
}

/**
 * Delete every replica database on this origin that `liveBackendIds` does
 * not claim.
 *
 * **This is the purge primitive sign-out and device revocation call**
 * (docs/specs/remote-access.md §9: the replica is "purged on sign-out and
 * device revocation"). Pass the backends that REMAIN attached — an empty
 * set for a full sign-out, which drops the current backend's database
 * too. Boot passes the attached set, which is what stops a backend id
 * that moved from leaving a database nothing will ever open again: it
 * sits outside every per-database cap, so nothing else can ever reclaim
 * it.
 *
 * Runs even when the replica has disabled itself after repeated failures.
 * The latch protects the READ path from a wedged engine; a purge that
 * skipped a disabled session would leave exactly the data a sign-out was
 * asked to remove.
 *
 * A purge that took the open database leaves the replica bound to no
 * backend, which is what a sign-out wants. Identity events only fire on
 * CHANGE, so a caller that stays attached to the same backend afterwards
 * (revoking one of several, purging then continuing) re-arms with
 * `initReplica`; nothing else will.
 *
 * Where `indexedDB.databases()` is missing (Firefox before 126) the
 * origin cannot be enumerated: only the database this page has open can
 * be named, so that one is still purged when the live set does not claim
 * it, and older orphans wait for an engine that can list them. The result
 * says which case ran.
 */
export async function purgeReplicaDatabases(
  liveBackendIds: ReadonlySet<string>,
  token: number = replicaToken(),
): Promise<ReplicaPurgeResult> {
  if (!indexedDbAvailable()) {
    return { deleted: [], failed: [], enumerated: false, cancelled: false };
  }
  // The open database is the one target nameable without enumerating, and
  // it is decided FIRST, before any await: an open still in flight would
  // otherwise re-create it a moment after a listing failed to see it, and
  // the purge would report success over a database that came back.
  // Detaching here also means nothing below can be looking at a database
  // it is about to delete.
  const openName = openDatabaseName();
  const targets = openName === '' ? [] : unclaimedReplicaDatabases([openName], liveBackendIds);
  let live = token;
  if (targets.length > 0) {
    if (session.token !== live) {
      return { deleted: [], failed: [], enumerated: false, cancelled: true };
    }
    detachSession();
    live = session.token;
  }
  const names = await listDatabaseNames();
  if (names !== null) {
    for (const name of unclaimedReplicaDatabases(names, liveBackendIds)) {
      if (!targets.includes(name)) targets.push(name);
    }
  }
  return { ...(await deleteDatabases(targets, live)), enumerated: names !== null };
}

/**
 * The in-flight sweep, so one cannot overlap the next. Not a latch: an
 * identity change after a sweep finished schedules another, because the
 * backend that just went is exactly the one with a database to reap.
 */
let sweepInFlight: Promise<void> | null = null;

/**
 * Reap replica databases no attached backend claims, after the session
 * is usable. Scheduled rather than awaited: this is storage hygiene over
 * the whole origin, and the cold-open read that `initReplica` gates must
 * not wait behind it.
 */
function scheduleUnclaimedSweep(token: number): void {
  if (sweepInFlight) return;
  if (session.token !== token) return;
  const live = attachedBackendIds();
  // Nothing nameable is live. Sweeping on an empty set would read
  // "delete everything", which is a sign-out's instruction, not an
  // unidentified backend's.
  if (live.size === 0) return;
  sweepInFlight = purgeReplicaDatabases(live, token)
    .then(
      () => undefined,
      (err: unknown) => {
        noteFailure('purge', err);
      },
    )
    .finally(() => {
      sweepInFlight = null;
    });
}

/**
 * Resolve the live database for `token`, waiting out an in-flight open.
 * Null whenever the replica is disabled, unopened, or the caller's token
 * has been superseded by an identity change.
 */
async function activeDb(token: number): Promise<IDBDatabase | null> {
  if (disabled) return null;
  if (session.token !== token) return null;
  const ready = session.ready;
  if (ready) await ready;
  if (disabled || session.token !== token) return null;
  return session.db;
}

/**
 * The accounting mirror, re-read from the database first if a commit
 * left it in doubt. Null when the caller's token has been superseded
 * mid-read. Costs nothing on the common path — the mirror is only dirty
 * after a rejected commit.
 */
async function syncedIndex(
  db: IDBDatabase,
  token: number,
): Promise<readonly ReplicaIndexEntry[] | null> {
  if (!session.indexDirty) return session.index;
  const stored = await readRecord<unknown>(db, META_STORE, META_INDEX_KEY);
  if (session.token !== token) return null;
  session.index = readIndexEntries(stored);
  session.indexDirty = false;
  return session.index;
}

/** Adopt the rows a commit merged, replacing this page's mirror. */
function adoptMergedIndex(merged: ReplicaIndexEntry[], token: number): void {
  if (session.token !== token) return;
  session.index = merged;
  session.indexDirty = false;
}

/** The token a caller must hand back to have its result honoured. */
export function replicaToken(): number {
  return session.token;
}

/**
 * Read one thread's persisted window. Null on a miss, on any validation
 * failure, and whenever the replica is unavailable — a caller cannot
 * distinguish those, and must not: all three mean "paint nothing, fetch".
 */
export async function getReplicaWindow(
  threadId: string,
  token: number = replicaToken(),
): Promise<ReplicaBody | null> {
  try {
    const db = await activeDb(token);
    if (!db) return null;
    const raw = await readRecord<unknown>(db, THREADS_STORE, threadId);
    if (session.token !== token) return null;
    if (raw === undefined) return null;
    const body = readEnvelope(raw);
    noteSuccess();
    if (!body) {
      // Stored by an older build. Drop rather than repair.
      void removeReplicaWindow(threadId, token);
      return null;
    }
    return body;
  } catch (err) {
    noteFailure('read', err);
    return null;
  }
}

/**
 * Persist one thread's window under an attested stamp. Oversized
 * windows are skipped (and any previous envelope for the thread dropped,
 * so the stored copy never outlives the window it described).
 */
export async function putReplicaWindow(
  threadId: string,
  body: ReplicaBody,
  token: number = replicaToken(),
): Promise<void> {
  try {
    const db = await activeDb(token);
    if (!db) return;
    const normalized = normalizeBody(body);
    if (!bodyFitsCaps(normalized)) {
      await removeReplicaWindow(threadId, token);
      return;
    }
    const entry: ReplicaIndexEntry = {
      threadId,
      savedAt: normalized.savedAt,
      chars: estimateBodyChars(normalized),
    };
    const merged = await commitEnvelope(db, threadId, wrapEnvelope(normalized), (stored) =>
      planWrite(readIndexEntries(stored), entry),
    );
    adoptMergedIndex(merged, token);
    noteSuccess();
  } catch (err) {
    session.indexDirty = true;
    noteFailure('write', err);
  }
}

/** Drop one thread's window — deletion, revert, `gone`, schema drift. */
export async function removeReplicaWindow(
  threadId: string,
  token: number = replicaToken(),
): Promise<void> {
  try {
    const db = await activeDb(token);
    if (!db) return;
    const index = await syncedIndex(db, token);
    if (!index) return;
    // Nothing accounted for this thread means nothing to delete, and a
    // transaction that rewrites an unchanged record is not free: the
    // inactive-thread drop in eventsItemStream fires for every unmounted
    // streaming thread at flush rate, and a background or workflow
    // thread the replica has never held is the COMMON case. Repeat calls
    // for the same absent thread stay O(index) with no IndexedDB work.
    if (!index.some((entry) => entry.threadId === threadId)) return;
    const merged = await commitRemoval(db, threadId, (stored) =>
      planRemoval(readIndexEntries(stored), threadId),
    );
    adoptMergedIndex(merged, token);
    noteSuccess();
  } catch (err) {
    session.indexDirty = true;
    noteFailure('remove', err);
  }
}

/**
 * Bind the replica to the transport's backend identity. Runs at module
 * init: every identity a manifest reports (first connect and every
 * reconnect refetch) re-points the database, and a generation change
 * clears it before any read can serve a row from the previous one.
 */
onBackendIdentity((identity) => {
  void initReplica(identity);
});

/** Test-only: forget the session and re-enable after a failure latch. */
export function __resetReplicaForTest(): void {
  closeSession();
  session = {
    token: session.token + 1,
    identity: { backendId: '', generation: '', name: '' },
    db: null,
    index: [],
    indexDirty: false,
    ready: null,
  };
  disabled = false;
  consecutiveFailures = 0;
}

/** Test-only: the in-flight unclaimed-database sweep, when one is running. */
export function __replicaSweepForTest(): Promise<void> | null {
  return sweepInFlight;
}

/** Test-only: is a database open and the session un-latched? */
export function __replicaEnabledForTest(): boolean {
  return !disabled && session.db !== null;
}

/** Test-only: current accounting rows. */
export function __replicaIndexForTest(): readonly ReplicaIndexEntry[] {
  return session.index;
}
