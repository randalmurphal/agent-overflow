// Per-computer load state of the sidebar's catalogs: 'loading' until the
// computer first answers the list read, 'loaded' from then on, 'failed'
// while the read fails with an error of its own. The sidebar never
// presents a catalog that is not loaded as an empty one.
//
// Every read settles here, the boot's, a hello's and a retry's alike: a
// failure through computerCatalog (./computerRows.ts), an answer in the
// commit that readComputerRows calls with its rows.
//
// It also holds whether each computer's thread list is listed in the
// sidebar (catalogTreeListed). A computer becomes listed once its thread
// catalog is known (answered, cached rows committed, or failed with an
// error of its own) and no read of its threads or groups is in flight. A
// read past its startup deadline is still in flight. Until then the
// sidebar holds that computer's project threads and groups, so the two
// catalogs first appear together. A listed computer stays listed until it
// detaches; its later reads commit in pairs (./threadCatalogs.ts).
//
// Until a catalog loads, a failed read retries itself:
//
//   - A read that missed its startup deadline or met a backend answering
//     temporarily_unavailable retries after 1, 2, 4 and 8 s, then every
//     10 s. A retry waits for the answer rather than for the startup
//     deadline, so a slow computer's answer is not superseded by the next
//     attempt.
//   - A failed read shows its error with a Retry and keeps retrying on the
//     same schedule. A scope refusal shows its error and waits for Retry:
//     asking again cannot change a grant.
//   - A read refused because the computer is offline or still starting
//     waits for the connection: its first hello refreshes every list
//     (./computerHydration.ts).
//
// One retry timer per catalog, whatever triggered the failure, so repeated
// hellos cannot stack retries. An answer cancels it. A loaded catalog
// stays loaded: later refreshes keep the last answer (./computerRows.ts).
// Detaching a computer drops its state, its listing and its timers.

import { untrack } from 'svelte';
import { attachedBackends, onBackendDetached } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { isScopeRefusal } from '../transport/scopeRefusal';
import { ReadDeadlineError } from '../utils/readBeforeDeadline';
import { userFacingError } from '../utils/userFacingError';
import { catalogReadInFlight } from './computerCatalogReads';
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
// Whether each computer's thread list is listed, by backend. Boxless keys
// read false: a computer is held until its catalogs say otherwise.
const listed = createKeyedSignalRegistry<boolean>(false);
// Computers whose thread catalog is known: answered, cached rows committed,
// or failed with an error of its own. Read only by the listing's writer.
const threadsKnown = new Set<BackendKey>();

interface CatalogRetry {
  /** Consecutive failures; picks the next delay. */
  attempt: number;
  timer: ReturnType<typeof setTimeout> | null;
}
const retries = new Map<string, CatalogRetry>();

type CatalogReader = (backend: BackendKey) => Promise<void>;
const readers = new Map<CatalogKind, CatalogReader>();

function catalogKey(kind: CatalogKind, backend: BackendKey): string {
  return `${kind}\u0000${backend}`;
}

/** One computer's load state for one catalog. Reactive on that key alone. */
export function catalogLoadState(backend: BackendKey, kind: CatalogKind): CatalogLoad {
  return states.get(catalogKey(kind, backend));
}

/**
 * Whether the sidebar lists `backend`'s project threads and groups.
 * Reactive on that computer alone.
 */
export function catalogTreeListed(backend: BackendKey): boolean {
  return listed.get(backend);
}

// The listing's one writer, run after every settled read: a read ending is
// the only event that can make the condition true.
function updateListing(backend: BackendKey): void {
  if (untrack(() => listed.get(backend)) || !threadsKnown.has(backend)) return;
  if (catalogReadInFlight(backend, 'threads') || catalogReadInFlight(backend, 'groups')) return;
  listed.set(backend, true);
}

/**
 * Install how a catalog re-reads one computer. The store owning the rows
 * registers it; this module never imports those stores. The reader must
 * settle through readComputerRows and handle its own rejection.
 */
export function registerCatalogReader(kind: CatalogKind, read: CatalogReader): void {
  readers.set(kind, read);
}

/**
 * Record how one computer's read of a catalog ended. An answer settles in
 * the same synchronous block as the commit of its rows.
 */
export function settleCatalogRead(kind: CatalogKind, backend: BackendKey, outcome: CatalogReadOutcome): void {
  if (!attachedBackends().some((entry) => entry.id === backend)) return;
  settlePhase(kind, backend, outcome);
  updateListing(backend);
}

function settlePhase(kind: CatalogKind, backend: BackendKey, outcome: CatalogReadOutcome): void {
  const key = catalogKey(kind, backend);
  if (outcome.answered) {
    if (kind === 'threads') threadsKnown.add(backend);
    clearRetry(key);
    states.set(key, LOADED);
    return;
  }
  const { error } = outcome;
  if (outcome.cached && kind === 'threads') threadsKnown.add(backend);
  if (untrack(() => states.get(key)).phase === 'loaded') return;
  if (error instanceof ReadDeadlineError || isTemporarilyUnavailableError(error)) {
    scheduleRetry(kind, backend);
    return;
  }
  if (isPassiveConnectionFailure(error)) {
    clearRetry(key);
    return;
  }
  if (kind === 'threads') threadsKnown.add(backend);
  states.set(key, { phase: 'failed', error: userFacingError(error, `Could not load ${kind}.`) });
  if (isScopeRefusal(error)) {
    clearRetry(key);
    return;
  }
  scheduleRetry(kind, backend);
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
 * Re-read every catalog of `backend` that is not loaded, now. The failed
 * state's Retry; a person acting, so the retry schedule starts over.
 */
export function retryCatalogLoad(backend: BackendKey): void {
  for (const kind of CATALOG_KINDS) {
    const key = catalogKey(kind, backend);
    if (untrack(() => states.get(key)).phase === 'loaded') continue;
    clearRetry(key);
    runReader(kind, backend);
  }
}

function scheduleRetry(kind: CatalogKind, backend: BackendKey): void {
  const key = catalogKey(kind, backend);
  let retry = retries.get(key);
  if (retry === undefined) {
    retry = { attempt: 0, timer: null };
    retries.set(key, retry);
  }
  if (retry.timer !== null) return;
  const delay = CATALOG_RETRY_DELAYS_MS[retry.attempt] ?? CATALOG_RETRY_MAX_MS;
  retry.attempt++;
  const scheduled = retry;
  scheduled.timer = setTimeout(() => {
    scheduled.timer = null;
    runReader(kind, backend);
  }, delay);
}

function runReader(kind: CatalogKind, backend: BackendKey): void {
  const read = readers.get(kind);
  if (read === undefined) return;
  void read(backend).catch((error: unknown) => {
    // Readers settle their outcome before rejecting; this is only a reader
    // that broke its contract.
    console.error(`Failed to retry the ${kind} catalog:`, error);
  });
}

function clearRetry(key: string): void {
  const retry = retries.get(key);
  if (retry === undefined) return;
  if (retry.timer !== null) clearTimeout(retry.timer);
  retries.delete(key);
}

function dropBackend(backend: BackendKey): void {
  for (const kind of CATALOG_KINDS) {
    const key = catalogKey(kind, backend);
    clearRetry(key);
    states.drop(key);
  }
  threadsKnown.delete(backend);
  listed.drop(backend);
}

onBackendDetached(({ backendId }) => dropBackend(backendId));

/** Test seam: pending retry timers, by catalog. */
export function __catalogRetryCountForTest(): number {
  let count = 0;
  for (const retry of retries.values()) if (retry.timer !== null) count++;
  return count;
}

/**
 * Test seam: list every attached computer's threads, phases untouched, for
 * a test that seeds the row stores directly.
 */
export function listCatalogTreesForTest(): void {
  for (const entry of attachedBackends()) listed.set(entry.id, true);
}

/** Test seam: forget every state and timer. Readers are module wiring and stay. */
export function resetCatalogLoadForTest(): void {
  for (const key of [...retries.keys()]) clearRetry(key);
  states.reset();
  threadsKnown.clear();
  listed.reset();
}
