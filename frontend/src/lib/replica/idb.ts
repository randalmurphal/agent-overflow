// Minimal promise-shaped IndexedDB adapter for the thread replica.
// The only IndexedDB in the app; deliberately not a general-purpose
// wrapper — it knows this database's two object stores and nothing else.
//
// Every operation is bounded by a watchdog. An IndexedDB request can
// legitimately never settle (a `versionchange` blocked by another tab,
// a wedged storage backend), and the replica sits on the cold-open path:
// an unbounded await there would hang the thread open, which is strictly
// worse than the miss it was meant to avoid. A watchdog that fires does
// not un-issue the request it gave up on, so anything the request would
// have HANDED BACK has to be disposed of when it arrives late — see
// `openReplicaDb`.

export const REPLICA_DB_VERSION = 1;
export const THREADS_STORE = 'threads';
export const META_STORE = 'meta';
/** Key of the identity record inside META_STORE. */
export const META_IDENTITY_KEY = 'identity';
/** Key of the accounting record inside META_STORE. */
export const META_INDEX_KEY = 'index';

/**
 * Per-operation watchdog. Generous relative to a healthy IndexedDB
 * (sub-millisecond against an open database) and far below any wait a
 * user would attribute to the replica: tripping it means the storage
 * backend is wedged, which the caller turns into a failure and, after
 * enough of them, a disabled replica.
 */
export const REPLICA_OP_TIMEOUT_MS = 2_000;

/**
 * One accounting row per stored envelope. Kept beside the envelopes
 * rather than derived from them so an eviction sweep costs one small
 * read instead of deserializing every window in the database.
 */
export interface ReplicaIndexEntry {
  threadId: string;
  savedAt: number;
  chars: number;
}

/**
 * The outcome of reconciling a page's intended change with the stored
 * accounting record: the rows to store, and the envelopes that leave in
 * the same transaction. Produced by `eviction.ts`.
 */
export interface IndexMerge {
  entries: ReplicaIndexEntry[];
  evict: readonly string[];
}

/**
 * Reconciler handed to a commit. Called SYNCHRONOUSLY inside the commit
 * transaction with the raw stored index record (`unknown` — validating
 * it is the caller's business, in one place), so the plan it returns is
 * built against what the database holds at commit time rather than a
 * page-local mirror another page may have moved on from.
 */
export type IndexMerger = (storedRecord: unknown) => IndexMerge;

export class ReplicaTimeoutError extends Error {
  constructor(label: string) {
    super(`replica: ${label} timed out after ${REPLICA_OP_TIMEOUT_MS}ms`);
    this.name = 'ReplicaTimeoutError';
  }
}

function asError(err: unknown, fallback: string): Error {
  if (err instanceof Error) return err;
  if (err === null || err === undefined) return new Error(fallback);
  return new Error(String(err));
}

/**
 * Bound `run` by the watchdog. `run` receives a signal that fires the
 * moment the wrapper stops caring about the result, so an operation that
 * acquires a resource can release one that arrives too late.
 */
function withTimeout<T>(label: string, run: (signal: AbortSignal) => Promise<T>): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const abandon = new AbortController();
    let settled = false;
    const timer = setTimeout(() => {
      if (settled) return;
      settled = true;
      abandon.abort();
      reject(new ReplicaTimeoutError(label));
    }, REPLICA_OP_TIMEOUT_MS);
    run(abandon.signal).then(
      (value) => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        resolve(value);
      },
      (err: unknown) => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        abandon.abort();
        reject(asError(err, `replica: ${label} failed`));
      },
    );
  });
}

function requestToPromise<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error ?? new Error('replica: IndexedDB request failed'));
  });
}

function transactionDone(tx: IDBTransaction): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    tx.oncomplete = () => resolve();
    tx.onabort = () => reject(tx.error ?? new Error('replica: transaction aborted'));
    tx.onerror = () => reject(tx.error ?? new Error('replica: transaction failed'));
  });
}

/** Is IndexedDB reachable in this context at all? */
export function indexedDbAvailable(): boolean {
  return typeof indexedDB !== 'undefined' && indexedDB !== null;
}

/**
 * Every database name on this origin, or NULL where the engine cannot
 * enumerate them (`indexedDB.databases` is absent on Firefox before 126;
 * Chromium, WebKit and every shell this app ships in have it).
 *
 * Null means UNKNOWN and never "none": a caller that turned it into a
 * deletion decision would delete on exactly the engines that cannot show
 * it what it is deleting. `purge.ts` documents what is lost where the
 * enumeration is missing.
 */
export function listDatabaseNames(): Promise<string[] | null> {
  // lib.dom types `databases()` as always present; the runtime does not
  // agree, and this is the check that keeps the older engine working.
  const factory: Partial<Pick<IDBFactory, 'databases'>> = indexedDB;
  const databases = factory.databases;
  if (typeof databases !== 'function') return Promise.resolve(null);
  return withTimeout('list databases', async () => {
    const infos = await databases.call(indexedDB);
    const names: string[] = [];
    for (const info of infos) {
      if (typeof info.name === 'string' && info.name !== '') names.push(info.name);
    }
    return names;
  });
}

/**
 * Delete one database by name, watchdogged like every other operation.
 *
 * A deletion is BLOCKED for as long as any connection stays open — this
 * page's own connection is the caller's business to close first, but
 * another page on the origin is not, and `onblocked` is the only signal
 * that it happened. Rejecting there is the honest answer ("this one did
 * not go"); the request itself stays live and usually completes the
 * moment the other page's `versionchange` handler closes its connection,
 * which is why the next sweep finds nothing to do.
 */
export function deleteDatabase(name: string): Promise<void> {
  return withTimeout(
    'delete database',
    () =>
      new Promise<void>((resolve, reject) => {
        const request = indexedDB.deleteDatabase(name);
        request.onsuccess = () => resolve();
        request.onerror = () =>
          reject(request.error ?? new Error(`replica: deleting ${name} failed`));
        request.onblocked = () =>
          reject(new Error(`replica: deleting ${name} blocked by another connection`));
      }),
  );
}

export function openReplicaDb(name: string): Promise<IDBDatabase> {
  return withTimeout(
    'open',
    (signal) =>
      new Promise<IDBDatabase>((resolve, reject) => {
        // An open that is abandoned — the watchdog gave up, or another
        // connection blocked us — still succeeds later, and its handle
        // has no owner: nothing will ever close it, so it pins the page
        // as a `versionchange` blocker for the rest of its lifetime.
        // Close it the moment it arrives instead.
        let abandoned = signal.aborted;
        signal.addEventListener(
          'abort',
          () => {
            abandoned = true;
          },
          { once: true },
        );
        const request = indexedDB.open(name, REPLICA_DB_VERSION);
        request.onupgradeneeded = () => {
          const db = request.result;
          if (!db.objectStoreNames.contains(THREADS_STORE)) db.createObjectStore(THREADS_STORE);
          if (!db.objectStoreNames.contains(META_STORE)) db.createObjectStore(META_STORE);
        };
        request.onsuccess = () => {
          const db = request.result;
          // Another tab (or this one, after a schema bump) asking to
          // upgrade must not be blocked by our handle. Closing here turns
          // that into a disabled replica for this page rather than a
          // wedged upgrade for both.
          db.onversionchange = () => db.close();
          if (abandoned) {
            db.close();
            return;
          }
          resolve(db);
        };
        request.onerror = () =>
          reject(request.error ?? new Error('replica: IndexedDB open failed'));
        request.onblocked = () => {
          abandoned = true;
          reject(new Error('replica: IndexedDB open blocked by another connection'));
        };
      }),
  );
}

export function readRecord<T>(db: IDBDatabase, store: string, key: string): Promise<T | undefined> {
  return withTimeout(`read ${store}`, async () => {
    const tx = db.transaction(store, 'readonly');
    const result = await requestToPromise<T | undefined>(
      tx.objectStore(store).get(key) as IDBRequest<T | undefined>,
    );
    return result;
  });
}

export function writeRecord(
  db: IDBDatabase,
  store: string,
  key: string,
  value: unknown,
): Promise<void> {
  return withTimeout(`write ${store}`, async () => {
    const tx = db.transaction(store, 'readwrite');
    tx.objectStore(store).put(value, key);
    await transactionDone(tx);
  });
}

/**
 * Read-modify-write the accounting record and mutate the envelope store
 * in one transaction, so the index can never describe a set of envelopes
 * that does not exist (and vice versa).
 *
 * The index is READ here rather than supplied by the caller because two
 * pages on one origin share the database: writing a page-local mirror
 * wholesale silently unaccounts every envelope the other page owns,
 * which makes those envelopes invisible to eviction until the next boot.
 * `merge` therefore sees the record as stored at commit time and folds
 * this page's change into it; the merged rows come back so the caller's
 * mirror can be refreshed from the union rather than from its own guess.
 *
 * Every follow-up request is issued synchronously from the read's
 * success handler. Chaining them off an awaited promise would race the
 * transaction's auto-commit on engines that check for pending requests
 * before the microtask queue drains.
 */
function commitIndexed(
  db: IDBDatabase,
  label: string,
  merge: IndexMerger,
  mutate: (threads: IDBObjectStore) => void,
): Promise<ReplicaIndexEntry[]> {
  return withTimeout(
    label,
    () =>
      new Promise<ReplicaIndexEntry[]>((resolve, reject) => {
        const tx = db.transaction([THREADS_STORE, META_STORE], 'readwrite');
        const threads = tx.objectStore(THREADS_STORE);
        const meta = tx.objectStore(META_STORE);
        let merged: ReplicaIndexEntry[] = [];
        let mergeError: unknown = null;
        const read = meta.get(META_INDEX_KEY);
        read.onsuccess = () => {
          let plan: IndexMerge;
          try {
            plan = merge(read.result);
          } catch (err) {
            mergeError = err;
            tx.abort();
            return;
          }
          merged = plan.entries;
          for (const id of plan.evict) threads.delete(id);
          mutate(threads);
          meta.put({ entries: merged }, META_INDEX_KEY);
        };
        tx.oncomplete = () => resolve(merged);
        tx.onabort = () =>
          reject(asError(mergeError ?? tx.error, `replica: ${label} aborted`));
        tx.onerror = () => reject(asError(tx.error, `replica: ${label} failed`));
      }),
  );
}

/** Write one envelope and fold its accounting row into the stored index. */
export function commitEnvelope(
  db: IDBDatabase,
  threadId: string,
  envelope: unknown,
  merge: IndexMerger,
): Promise<ReplicaIndexEntry[]> {
  return commitIndexed(db, 'commit envelope', merge, (threads) => {
    threads.put(envelope, threadId);
  });
}

/** Drop one thread's envelope and its accounting row together. */
export function commitRemoval(
  db: IDBDatabase,
  threadId: string,
  merge: IndexMerger,
): Promise<ReplicaIndexEntry[]> {
  return commitIndexed(db, 'remove envelope', merge, (threads) => {
    threads.delete(threadId);
  });
}

/**
 * Delete envelopes that carry no accounting row, in one transaction for
 * the whole set. Boot-path only: it runs before the first replica read
 * can be served, so a watchdogged transaction per orphan would put the
 * cold open behind an unbounded queue of them. The accounting record is
 * untouched — by definition none of these keys appear in it.
 */
export function deleteEnvelopes(db: IDBDatabase, threadIds: readonly string[]): Promise<void> {
  return withTimeout('reap orphans', async () => {
    const tx = db.transaction(THREADS_STORE, 'readwrite');
    const threads = tx.objectStore(THREADS_STORE);
    for (const threadId of threadIds) threads.delete(threadId);
    await transactionDone(tx);
  });
}

/** Empty both stores. The caller re-stamps identity afterwards. */
export function clearStores(db: IDBDatabase): Promise<void> {
  return withTimeout('clear', async () => {
    const tx = db.transaction([THREADS_STORE, META_STORE], 'readwrite');
    tx.objectStore(THREADS_STORE).clear();
    tx.objectStore(META_STORE).clear();
    await transactionDone(tx);
  });
}

/** Every envelope key currently stored — used to detect unaccounted rows. */
export function readThreadKeys(db: IDBDatabase): Promise<string[]> {
  return withTimeout('list keys', async () => {
    const tx = db.transaction(THREADS_STORE, 'readonly');
    const keys = await requestToPromise<IDBValidKey[]>(tx.objectStore(THREADS_STORE).getAllKeys());
    return keys.filter((key): key is string => typeof key === 'string');
  });
}
