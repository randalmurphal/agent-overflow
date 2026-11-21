// Per-computer load state of the sidebar's catalogs: 'loading' until the
// computer first answers the list read, 'loaded' once it has, 'failed'
// while the read fails with an error of its own. The sidebar never
// presents a catalog that is not loaded as an empty one.
//
// Every read settles here, the boot's, a hello's and a retry's alike: a
// failure through computerCatalog (./computerRows.ts), an answer in the
// commit that commits its rows.
//
// A failed read retries itself. Each catalog store registers the reader
// that re-reads one computer, with the catalogs that reader covers: the
// thread list reads threads and groups as one (./threadCatalogs.ts), and
// one timer per reader and computer retries them together.
//
//   - A read that met a backend answering temporarily_unavailable retries
//     after 1, 2, 4 and 8 s, then every 10 s. A retry waits for the answer
//     rather than for the startup deadline. A read past its startup deadline
//     is still in flight: its late answer or late failure settles it.
//   - A failed read shows its error with a Retry and keeps retrying on the
//     same schedule, loaded or not: the rows it had stay listed. A scope
//     refusal shows its error and waits for Retry: asking again cannot
//     change a grant.
//   - A read refused because the computer is offline or still starting
//     waits for the connection: its first hello refreshes every list
//     (./computerHydration.ts).
//
// The timer runs while any catalog its reader covers wants a retry, so an
// answer of one catalog cannot cancel the retry another one's failure asked
// for, and repeated hellos cannot stack retries. Every read of a computer's
// catalogs, whatever started it (boot, hello, gap, retry), is an attempt
// (beginCatalogAttempt): beginning one consumes the retry its reader's
// catalogs asked for and cancels the armed timer, no timer runs for that
// reader and computer until every attempt has ended, and the timer is
// re-evaluated then. So a retry never supersedes a read that is still out,
// and Retry starts nothing while one is: it only restarts the schedule. A
// history generation
// observed mid-session puts every catalog of that computer back to
// 'loading' and re-reads it: its rows describe a history it no longer has.
// Detaching a computer drops its state and its timers.

import { untrack } from 'svelte';
import { attachedBackends, onBackendDetached } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import { onBackendIdentity } from '../transport/backendIdentity';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { isScopeRefusal } from '../transport/scopeRefusal';
import { ReadDeadlineError } from '../utils/readBeforeDeadline';
import { userFacingError } from '../utils/userFacingError';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { isTemporarilyUnavailableError } from './transportStatus.svelte';

export type CatalogKind = 'threads' | 'projects' | 'groups';
export type CatalogPhase = 'loading' | 'loaded' | 'failed';

export interface CatalogLoad {
  phase: CatalogPhase;
  /** What a person reads for a failed load; '' otherwise. */
  error: string;
}

/**
 * How one computer's read of a catalog ended: answered with its rows, or
 * failed, with whether rows of that computer (kept or from the replica)
 * stand in for the answer.
 */
export type CatalogReadOutcome = { answered: true } | { answered: false; error: unknown; cached: boolean };

/** The retry delays before the cap, in order. */
export const CATALOG_RETRY_DELAYS_MS: readonly number[] = [1_000, 2_000, 4_000, 8_000];
export const CATALOG_RETRY_MAX_MS = 10_000;

/** Every catalog, in the order the sidebar's status row names them. */
export const CATALOG_KINDS: readonly CatalogKind[] = ['projects', 'threads', 'groups'];
const LOADING: CatalogLoad = { phase: 'loading', error: '' };
const LOADED: CatalogLoad = { phase: 'loaded', error: '' };

// Boxless keys read LOADING: an attached computer that has not answered is
// loading, not empty.
const states = createKeyedSignalRegistry<CatalogLoad>(LOADING);
// Catalogs, by catalogKey, whose last read asked for a retry.
const wantsRetry = new Set<string>();

type CatalogReader = (backend: BackendKey) => Promise<void>;
interface RegisteredReader {
  kinds: readonly CatalogKind[];
  read: CatalogReader;
}
const readers: RegisteredReader[] = [];

interface CatalogRetry {
  /** Consecutive retries; picks the next delay. */
  attempt: number;
  timer: ReturnType<typeof setTimeout> | null;
}
// One per reader and computer.
const retries = new Map<RegisteredReader, Map<BackendKey, CatalogRetry>>();
// The attempts in flight, per reader and computer.
const inFlight = new Map<RegisteredReader, Map<BackendKey, Set<object>>>();

function catalogKey(kind: CatalogKind, backend: BackendKey): string {
  return `${kind}\u0000${backend}`;
}

/** One computer's load state for one catalog. Reactive on that key alone. */
export function catalogLoadState(backend: BackendKey, kind: CatalogKind): CatalogLoad {
  return states.get(catalogKey(kind, backend));
}

/**
 * Whether a read failure is an error of the computer's own, shown and
 * retried as a failed load, rather than a deadline, a temporarily
 * unavailable backend or a passive connection failure.
 */
export function catalogFailureShown(error: unknown): boolean {
  return !(error instanceof ReadDeadlineError) && !isTemporarilyUnavailableError(error) && !isPassiveConnectionFailure(error);
}

/**
 * Install how the catalogs `kinds` re-read one computer, together. The store
 * owning the rows registers it; this module never imports those stores. The
 * reader must settle through the catalog reads, begin each computer's read
 * as an attempt, and handle its own rejection.
 */
export function registerCatalogReader(kinds: readonly CatalogKind[], read: CatalogReader): void {
  readers.push({ kinds, read });
}

function readerOf(kind: CatalogKind): RegisteredReader | undefined {
  return readers.find((reader) => reader.kinds.includes(kind));
}

/**
 * Record how one computer's read of a catalog ended. An answer settles in
 * the same synchronous block as the commit of its rows.
 */
export function settleCatalogRead(kind: CatalogKind, backend: BackendKey, outcome: CatalogReadOutcome): void {
  if (!attachedBackends().some((entry) => entry.id === backend)) return;
  const key = catalogKey(kind, backend);
  if (outcome.answered) {
    wantsRetry.delete(key);
    states.set(key, LOADED);
  } else {
    settleFailure(kind, key, outcome.error);
  }
  updateRetry(kind, backend);
}

function settleFailure(kind: CatalogKind, key: string, error: unknown): void {
  // Still out: its late answer or late failure settles it.
  if (error instanceof ReadDeadlineError) return;
  if (isTemporarilyUnavailableError(error)) {
    wantsRetry.add(key);
    return;
  }
  if (isPassiveConnectionFailure(error)) {
    wantsRetry.delete(key);
    return;
  }
  states.set(key, { phase: 'failed', error: userFacingError(error, `Could not load ${kind}.`) });
  if (isScopeRefusal(error)) wantsRetry.delete(key);
  else wantsRetry.add(key);
}

/**
 * Mark the computers whose rows a read just committed as loaded. Called in
 * the same synchronous block as the commit, so no render sees a loaded
 * catalog without its rows.
 */
export function settleCatalogAnswers(kind: CatalogKind, answered: Iterable<BackendKey>): void {
  for (const backend of answered) settleCatalogRead(kind, backend, { answered: true });
}

/**
 * Re-read, now, every reader of `backend` that covers a catalog not loaded.
 * The failed state's Retry; a person acting, so the retry schedule starts
 * over. A reader with an attempt in flight reads nothing more: that attempt
 * waits for its answer, so it is already the newest try.
 */
export function retryCatalogLoad(backend: BackendKey): void {
  for (const reader of readers) {
    if (reader.kinds.every((kind) => untrack(() => states.get(catalogKey(kind, backend))).phase === 'loaded')) continue;
    clearRetry(reader, backend);
    if (!inFlight.get(reader)?.has(backend)) runReader(reader, backend);
  }
}

function updateRetry(kind: CatalogKind, backend: BackendKey): void {
  const reader = readerOf(kind);
  if (reader !== undefined) updateReaderRetry(reader, backend);
}

// Arm the reader's timer while any catalog it covers wants a retry and no
// attempt of it is in flight; clear it, schedule and all, once none wants one.
function updateReaderRetry(reader: RegisteredReader, backend: BackendKey): void {
  if (inFlight.get(reader)?.has(backend)) return;
  if (!reader.kinds.some((covered) => wantsRetry.has(catalogKey(covered, backend)))) {
    clearRetry(reader, backend);
    return;
  }
  let byBackend = retries.get(reader);
  if (byBackend === undefined) retries.set(reader, byBackend = new Map());
  let retry = byBackend.get(backend);
  if (retry === undefined) byBackend.set(backend, retry = { attempt: 0, timer: null });
  if (retry.timer !== null) return;
  const delay = CATALOG_RETRY_DELAYS_MS[retry.attempt] ?? CATALOG_RETRY_MAX_MS;
  retry.attempt++;
  const scheduled = retry;
  scheduled.timer = setTimeout(() => {
    scheduled.timer = null;
    runReader(reader, backend);
  }, delay);
}

function runReader(reader: RegisteredReader, backend: BackendKey): void {
  void reader.read(backend).catch((error: unknown) => {
    // Readers settle their outcome before rejecting; this is only a reader
    // that broke its contract.
    console.error(`Failed to re-read the ${reader.kinds.join(' and ')} catalogs:`, error);
  });
}

/**
 * Begin one computer's read of the catalogs `kind`'s reader covers; returns
 * its end, which the read calls once, when nothing of it is still out. Every
 * read enters here, whatever started it.
 */
export function beginCatalogAttempt(kind: CatalogKind, backend: BackendKey): () => void {
  const reader = readerOf(kind);
  if (reader === undefined) return () => {};
  for (const covered of reader.kinds) wantsRetry.delete(catalogKey(covered, backend));
  // A cancelled retry never ran, so it does not advance the schedule.
  const scheduled = retries.get(reader)?.get(backend);
  if (scheduled !== undefined && scheduled.timer !== null) {
    clearTimeout(scheduled.timer);
    scheduled.timer = null;
    scheduled.attempt--;
  }
  let byBackend = inFlight.get(reader);
  if (byBackend === undefined) inFlight.set(reader, byBackend = new Map());
  let attempts = byBackend.get(backend);
  if (attempts === undefined) byBackend.set(backend, attempts = new Set());
  const attempt = {};
  attempts.add(attempt);
  const held = attempts;
  const owner = byBackend;
  return () => {
    if (!held.delete(attempt) || held.size > 0) return;
    if (owner.get(backend) === held) owner.delete(backend);
    updateReaderRetry(reader, backend);
  };
}

function clearRetry(reader: RegisteredReader, backend: BackendKey): void {
  const byBackend = retries.get(reader);
  const retry = byBackend?.get(backend);
  if (retry === undefined) return;
  if (retry.timer !== null) clearTimeout(retry.timer);
  byBackend!.delete(backend);
}

// A history generation observed mid-session (a restored database) has no
// hello behind it.
onBackendIdentity((_identity, backend, change) => {
  if (change !== 'generation' || !attachedBackends().some((entry) => entry.id === backend)) return;
  for (const kind of CATALOG_KINDS) states.set(catalogKey(kind, backend), LOADING);
  for (const reader of readers) runReader(reader, backend);
});

function dropBackend(backend: BackendKey): void {
  for (const reader of readers) {
    clearRetry(reader, backend);
    inFlight.get(reader)?.get(backend)?.clear();
    inFlight.get(reader)?.delete(backend);
  }
  for (const kind of CATALOG_KINDS) {
    const key = catalogKey(kind, backend);
    wantsRetry.delete(key);
    states.drop(key);
  }
}

onBackendDetached(({ backendId }) => dropBackend(backendId));

/** Test seam: pending retry timers, by reader and computer. */
export function __catalogRetryCountForTest(): number {
  let count = 0;
  for (const byBackend of retries.values()) for (const retry of byBackend.values()) if (retry.timer !== null) count++;
  return count;
}

/** Test seam: forget every state and timer. Readers are module wiring and stay. */
export function resetCatalogLoadForTest(): void {
  for (const [reader, byBackend] of retries) for (const backend of [...byBackend.keys()]) clearRetry(reader, backend);
  for (const byBackend of inFlight.values()) for (const attempts of byBackend.values()) attempts.clear();
  inFlight.clear();
  wantsRetry.clear();
  states.reset();
}
