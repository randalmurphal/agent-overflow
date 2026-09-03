// Session-import state: the catalog of provider sessions Agent Overflow
// doesn't know about yet, the modal's filters and selection, and the folded
// progress of an in-flight import run. Single owner — the modal, its
// toolbar, list and progress strip all read from here and never keep a
// parallel copy.
//
// `open` lives here rather than in the modal so a future palette command can
// raise the surface without mounting it first. It is also what lets the run
// close the surface itself when everything landed.
//
// RPCs go straight through `stores/bindings.ts` like every other store: the
// test seam is `setBindingMock('ListImportableSessions', …)` one layer
// deeper (frontend/AGENTS.md), so an indirection here would buy nothing and
// cost a wiring step that can be forgotten.

import {
  CancelSessionImport,
  ImportSessions,
  ListImportableSessions,
} from './bindings';
import { hasScope } from '../transport/scopes';
import type {
  ImportProviderFilter,
  ImportProviderStatus,
  ImportRowResult,
  ImportableSession,
  SessionImportProgressEvent,
} from '../types/sessionImport';
import { isRowStatus } from '../types/sessionImport';
import { isHiddenRow, projectGroupKey } from './sessionImportFilter';
import { countNoun } from '../utils/format';
import { userFacingError } from '../utils/userFacingError';
import { refreshSidebarProjections } from './eventsThreadRows';
import { addToast } from './toast.svelte';

export type SessionImportStatus = 'idle' | 'loading' | 'ready' | 'error';

/**
 * Rolling tally over a run's per-row outcomes. Maintained incrementally by
 * `applyImportProgress` rather than folded on read: an "Import all" over a
 * real Claude home is thousands of rows and thousands of frames, and both
 * the progress strip and the completion toast want these numbers — a fold
 * per frame per reader is quadratic in the size of the run.
 */
export interface ImportRunCounts {
  imported: number;
  failed: number;
  skipped: number;
  /** Threads the imported rows actually created (their `threadIds`). */
  threads: number;
}

const EMPTY_COUNTS: ImportRunCounts = Object.freeze({
  imported: 0,
  failed: 0,
  skipped: 0,
  threads: 0,
});

/** Folded state of one import run. */
export interface ImportRunState {
  importId: string;
  total: number;
  /**
   * Frames the backend has reported, monotonic. It reaches `total` on a run
   * that finished and stops short on one that was cancelled — never rounded
   * up, because that shortfall is the only signal that rows were left
   * untouched.
   */
  completed: number;
  /**
   * Whether the SURFACE still treats the run as in flight: the close guard,
   * the Stop button, the frozen list and the CTA all read this. It goes
   * false when the run reported `done` and ALSO when a gap proved frames
   * were lost — in the second case the backend may well still be running,
   * but this client can no longer promise it will ever hear the end, and a
   * permanently unclosable modal is not an option.
   *
   * A transport blip is NOT one of those: the transport replays a channel's
   * missed frames on reconnect, so the run keeps running through it and
   * settles on the replayed terminal frame.
   */
  active: boolean;
  /**
   * The run reported `done`. THIS — never `active` — is what closes the
   * fold: a run whose frames were gapped is inactive but not terminal, and
   * every LIVE frame that follows (the eventual `done` included) is still
   * its own and still folds. Ignoring those would freeze the surface on a
   * run the backend went on to finish.
   */
  terminal: boolean;
  /**
   * Per-row outcomes, keyed by row id. MUTATED IN PLACE — a Map is not a
   * reactive proxy, so a clone-per-frame used to be the change signal, and
   * on a 6500-row run that is 6500 copies of a growing map. Read it only
   * through `getImportRowResult`, which gates on `resultsVersion`.
   */
  results: Map<string, ImportRowResult>;
  /** Bumped on every `results` mutation. THE reactive signal for the map. */
  resultsVersion: number;
  /** Incremental tally over `results`; see `ImportRunCounts`. */
  counts: ImportRunCounts;
  /** The user asked the backend to stop; its terminal frame is still due. */
  stopRequested: boolean;
  /**
   * Frames were provably lost — the transport reported a gap, meaning the
   * server's replay ring could not cover what this client missed. STICKY:
   * a terminal frame that arrives afterwards settles the run but cannot
   * un-lose the outcomes that fell out of the ring, so the counts stay
   * "at least this much" and the surface never reports the run as clean.
   */
  connectionLost: boolean;
}

const IMPORT_UNGRANTED_MESSAGE = 'Importing provider sessions is only available on the local app.';

let open = $state(false);
let status = $state<SessionImportStatus>('idle');
let catalogError = $state('');
let providers = $state<readonly ImportProviderStatus[]>([]);
let rows = $state<readonly ImportableSession[]>([]);
let selected = $state<ReadonlySet<string>>(new Set());
let providerFilter = $state<ImportProviderFilter>('all');
let projectFilter = $state<string | null>(null);
let query = $state('');
let showAlreadyRan = $state(false);
let run = $state<ImportRunState | null>(null);

let inFlightScan: Promise<void> | null = null;
let starting = false;
/**
 * A run has changed what is importable, so the loaded catalog no longer
 * matches the provider homes. Not reactive: nothing renders it — it only
 * decides whether the next `loadImportCatalog()` may reuse what it has.
 * Without it, importing and reopening would offer the same sessions again
 * (the backend drops its own scan cache after a run; this is the frontend's
 * half of the same fact).
 */
let catalogStale = false;

// --- reads ----------------------------------------------------------------

export function isSessionImportOpen(): boolean {
  return open;
}

export function getSessionImportStatus(): SessionImportStatus {
  return status;
}

/** User-facing error for the whole surface: catalog scan AND run control. */
export function getSessionImportError(): string {
  return catalogError;
}

export function getImportProviders(): readonly ImportProviderStatus[] {
  return providers;
}

export function getImportRows(): readonly ImportableSession[] {
  return rows;
}

export function getImportSelection(): ReadonlySet<string> {
  return selected;
}

export function getImportProviderFilter(): ImportProviderFilter {
  return providerFilter;
}

export function getImportProjectFilter(): string | null {
  return projectFilter;
}

export function getImportQuery(): string {
  return query;
}

/** Whether sessions Agent Overflow itself produced are part of the offer. */
export function getImportShowAlreadyRan(): boolean {
  return showAlreadyRan;
}

export function getSessionImportRun(): ImportRunState | null {
  return run;
}

/**
 * One row's outcome, or undefined when the run hasn't reported on it.
 *
 * The ONLY read path into `run.results` — it touches `resultsVersion` so a
 * caller inside a reactive scope re-runs when the map mutates. Reading the
 * Map directly would be silently non-reactive.
 */
export function getImportRowResult(id: string): ImportRowResult | undefined {
  const current = run;
  if (!current) return undefined;
  // Dependency read, not a value read: see `ImportRunState.results`.
  void current.resultsVersion;
  return current.results.get(id);
}

/**
 * The run's tally. ONE accessor for both the progress strip and the
 * completion toast, so the two can't drift into describing the same run
 * differently.
 */
export function getImportRunCounts(): ImportRunCounts {
  return run?.counts ?? EMPTY_COUNTS;
}

/**
 * Row ids the run has reported as failed, in catalog order — the retry
 * target. Empty while the run is ACTIVE: a failure is not final until the
 * run stops offering to retry it itself, and walking the whole catalog on
 * every progress frame is exactly the per-frame fold `counts` exists to
 * avoid.
 *
 * A failure deliberately survives the provider, project and query filters —
 * those narrow a view, and a retry the user cannot see the target of is
 * still the retry they asked for. The already-ran toggle is the exception,
 * because it is exclusion rather than narrowing: it takes rows out of the
 * offer, and "Retry failed (2)" over rows the user cannot see OR deselect
 * would import them anyway.
 *
 * After a gap the run is inactive without being finished, so this answers
 * from what is known so far and keeps improving as the surviving frames land
 * — a retry started there is refused by the backend (one run at a time),
 * which surfaces as an error rather than a duplicate import.
 */
export function getFailedImportIds(): string[] {
  const current = run;
  if (!current || current.active) return [];
  const hiding = { showAlreadyRan };
  return rows
    .filter((row) => !isHiddenRow(row, hiding) && getImportRowResult(row.id)?.status === 'failed')
    .map((row) => row.id);
}

// --- surface ---------------------------------------------------------------

export function openSessionImport(): void {
  open = true;
}

/**
 * The ONE close path — Esc, backdrop, and the Cancel button all land here,
 * and all are refused while a run is in flight. Closing drops the run's
 * outcome stamps and the selection; the catalog and filters survive so a
 * reopen doesn't re-scan from scratch.
 */
export function closeSessionImport(): void {
  if (run?.active) return;
  open = false;
  run = null;
  if (selected.size > 0) selected = new Set();
}

// --- catalog ---------------------------------------------------------------

/**
 * Load the catalog. A concurrent call joins the running scan; without
 * `force` an already-loaded catalog is reused, which is what makes the
 * modal's load-on-open effect cheap.
 */
export function loadImportCatalog(force = false): Promise<void> {
  if (!hasScope('threads:operate')) {
    providers = [];
    rows = [];
    pruneSelectionAndFilters();
    catalogError = IMPORT_UNGRANTED_MESSAGE;
    status = 'error';
    return Promise.resolve();
  }
  if (inFlightScan) return inFlightScan;
  if (status === 'ready' && !force && !catalogStale) return Promise.resolve();

  status = 'loading';
  catalogError = '';
  const scan = runScan(force).finally(() => {
    if (inFlightScan === scan) inFlightScan = null;
  });
  inFlightScan = scan;
  return scan;
}

async function runScan(force: boolean): Promise<void> {
  try {
    const result = await ListImportableSessions({ forceRefresh: force });
    providers = result?.providers ?? [];
    rows = result?.rows ?? [];
    pruneSelectionAndFilters();
    catalogStale = false;
    status = 'ready';
  } catch (err) {
    // A failed scan leaves no rows: showing the previous catalog behind an
    // error would invite importing against a stale view of the provider home.
    providers = [];
    rows = [];
    pruneSelectionAndFilters();
    catalogError = userFacingError(err);
    status = 'error';
  }
}

/** Drop selections and a project filter that the new catalog no longer has. */
function pruneSelectionAndFilters(): void {
  if (selected.size > 0) {
    const ids = new Set(rows.map((row) => row.id));
    const kept = new Set<string>();
    for (const id of selected) {
      if (ids.has(id)) kept.add(id);
    }
    // Through the chokepoint even when nothing was dropped here: the new
    // catalog decides which rows are hidden, and a row the previous scan
    // offered may now be one of them.
    writeSelection(kept);
  }
  clearStrandedProjectFilter();
}

/**
 * Clear a project filter whose group has nothing left on offer.
 *
 * The dropdown builds its entries from the rows the already-ran toggle does
 * not withhold, so a filter naming a group that is no longer in it cannot be
 * cleared from the menu it was picked in — the trigger reads "Project" over
 * an empty list until something unrelated changes it. Both writers that can
 * strand one land here: a rescan, and the toggle taking a group's last
 * offered row away.
 */
function clearStrandedProjectFilter(): void {
  if (projectFilter === null) return;
  const hiding = { showAlreadyRan };
  const survives = rows.some(
    (row) => !isHiddenRow(row, hiding) && projectGroupKey(row) === projectFilter,
  );
  if (!survives) projectFilter = null;
}

// --- filters ---------------------------------------------------------------

export function setProviderFilter(next: ImportProviderFilter): void {
  if (providerFilter === next) return;
  providerFilter = next;
}

export function setProjectFilter(next: string | null): void {
  if (projectFilter === next) return;
  projectFilter = next;
}

export function setImportQuery(next: string): void {
  if (query === next) return;
  query = next;
}

/**
 * Show or hide the sessions Agent Overflow itself produced.
 *
 * Hiding RETRACTS them from the selection, unlike every other filter here.
 * The others narrow a view and the selection deliberately survives them; this
 * one takes rows out of the offer, and a primary button reading "Import (12)"
 * over rows the user has just said they don't want — and cannot see to
 * deselect — would import them anyway. The retraction is not this function's
 * to remember: it writes through the one selection chokepoint, which holds
 * the invariant for every writer.
 */
export function setShowAlreadyRan(next: boolean): void {
  if (showAlreadyRan === next) return;
  showAlreadyRan = next;
  if (next) return;
  if (selected.size > 0) writeSelection(new Set(selected));
  // Hiding can withdraw the last offered row of the project the filter names,
  // which the dropdown then stops listing — see `clearStrandedProjectFilter`.
  clearStrandedProjectFilter();
}

// --- selection -------------------------------------------------------------

/**
 * The ONE selection writer.
 *
 * Every write drops rows the already-ran toggle is withholding, so "the
 * selection never holds a hidden row" is structural rather than a rule each
 * caller has to observe. Cost is one pass over the catalog per selection
 * change — a user action, never a frame.
 */
function writeSelection(next: Set<string>): void {
  if (!showAlreadyRan && next.size > 0) {
    const hiding = { showAlreadyRan };
    for (const row of rows) {
      if (isHiddenRow(row, hiding)) next.delete(row.id);
    }
  }
  if (sameSelection(next, selected)) return;
  selected = next;
}

/** Sets of ids are equal when they agree on size and membership. */
function sameSelection(a: ReadonlySet<string>, b: ReadonlySet<string>): boolean {
  if (a.size !== b.size) return false;
  for (const id of a) {
    if (!b.has(id)) return false;
  }
  return true;
}

export function toggleRow(id: string): void {
  const next = new Set(selected);
  if (next.has(id)) next.delete(id);
  else next.add(id);
  writeSelection(next);
}

export function setSelection(ids: Iterable<string>): void {
  writeSelection(new Set(ids));
}

// --- run -------------------------------------------------------------------

/**
 * Start an import run. Returns once the backend has accepted the run (or
 * refused it) — the work itself reports through `applyImportProgress`.
 * Re-entrant calls are refused, including during the accept round trip, so a
 * double-click cannot start two runs over the same rows.
 */
export async function startImport(ids: readonly string[]): Promise<void> {
  if (starting || run?.active) return;
  if (!hasScope('threads:operate')) {
    catalogError = IMPORT_UNGRANTED_MESSAGE;
    return;
  }
  const unique = [...new Set(ids)].filter((id) => id.length > 0);
  if (unique.length === 0) return;

  starting = true;
  catalogError = '';
  try {
    const handle = await ImportSessions({ ids: unique });
    run = {
      importId: handle.importId,
      // The backend owns the total (a run can expand a row into branches);
      // fall back to the request size if it reports nothing usable.
      total: handle.total > 0 ? handle.total : unique.length,
      completed: 0,
      active: true,
      terminal: false,
      results: new Map(),
      resultsVersion: 0,
      counts: { imported: 0, failed: 0, skipped: 0, threads: 0 },
      stopRequested: false,
      connectionLost: false,
    };
  } catch (err) {
    run = null;
    catalogError = userFacingError(err);
  } finally {
    starting = false;
  }
}

/**
 * Ask the backend to stop the in-flight run. The run is NOT torn down here:
 * the backend still emits its terminal frame (with `completed` short of
 * `total`), and that frame is what settles the surface — inventing a local
 * end state would leave the modal disagreeing with rows still being written.
 */
export async function stopImport(): Promise<void> {
  const current = run;
  if (!current || !current.active || current.stopRequested) return;
  current.stopRequested = true;
  try {
    await CancelSessionImport(current.importId);
  } catch (err) {
    // The run may have finished between the click and the call; if it is
    // still going the user can ask again, so the flag has to come back off.
    if (run === current && current.active) current.stopRequested = false;
    catalogError = userFacingError(err);
  }
}

/**
 * Fold one progress frame into the run.
 *
 * Frames are defensive input: they can be re-delivered, arrive out of order,
 * or belong to a run this client no longer tracks. Completion therefore only
 * ever moves forward and never past the total, per-row outcomes are keyed so
 * a duplicate frame is idempotent, and anything after the terminal frame is
 * ignored — the `done` frame is the one that refreshes the sidebar, and it
 * must do so exactly once.
 *
 * The cutoff is `terminal`, not `active`. A gap ends the run for the SURFACE
 * without ending it for the backend, and the frames that follow one are
 * ordinary live frames about a run that is still executing — dropping them
 * would leave the modal stuck on the last pre-gap count while rows kept
 * landing, and would discard the very frame that finishes the run.
 */
export function applyImportProgress(evt: SessionImportProgressEvent): void {
  const current = run;
  if (!current || !evt) return;
  if (evt.importId !== current.importId) return;
  if (current.terminal) return;

  if (Number.isFinite(evt.total) && evt.total > 0) {
    current.total = Math.trunc(evt.total);
  }

  if (typeof evt.id === 'string' && evt.id !== '' && isRowStatus(evt.status)) {
    const result: ImportRowResult = {
      id: evt.id,
      status: evt.status,
      threadIds: Array.isArray(evt.threadIds) ? [...evt.threadIds] : [],
      error: typeof evt.error === 'string' ? evt.error : '',
    };
    // Re-delivery (a reconnect replays the channel) must stay idempotent,
    // so a row that already had an outcome retracts its old contribution
    // before the new one lands.
    const previous = current.results.get(result.id);
    if (previous) tally(current.counts, previous, -1);
    current.results.set(result.id, result);
    tally(current.counts, result, 1);
    current.resultsVersion += 1;
  }

  const reported = Number.isFinite(evt.completed) ? Math.trunc(evt.completed) : 0;
  current.completed = Math.min(current.total, Math.max(current.completed, reported));

  if (evt.done) {
    current.terminal = true;
    current.active = false;
    current.stopRequested = false;
    settleFinishedRun(current);
  }
}

/** Move one row's contribution into or out of the run's tally. */
function tally(counts: ImportRunCounts, result: ImportRowResult, sign: 1 | -1): void {
  if (result.status === 'imported') {
    counts.imported += sign;
    counts.threads += sign * result.threadIds.length;
  } else if (result.status === 'failed') {
    counts.failed += sign;
  } else {
    counts.skipped += sign;
  }
}

/**
 * Decide what a finished run leaves on screen.
 *
 * A clean full run has nothing left to say, so it closes itself and reports
 * in a toast. The thread count comes from actual outcomes, so a session with
 * no importable history is never guessed from the catalogue.
 * Anything the user still has to look at — a failure to retry, a run they
 * stopped early, a run whose frames were gapped — keeps the surface open
 * with the rows stamped.
 *
 * A gapped run is settled but not summarised: its terminal frame proves the
 * backend finished, and the rows that landed after the gap are stamped
 * correctly, but the outcomes that fell out of the replay ring are gone for
 * good. Toasting "Imported 6 sessions" over an unknown shortfall would be a
 * claim this client cannot make, so the surface stays open showing exactly
 * what it was told — including a Retry over the failures it did see.
 */
function settleFinishedRun(current: ImportRunState): void {
  const { imported, failed, skipped, threads } = current.counts;
  // Whatever landed is no longer importable, so the loaded catalog is now a
  // stale offer. The list on screen keeps its stamps (that is what the user
  // is reading); the next open re-scans.
  if (imported > 0) catalogStale = true;

  const stoppedEarly = current.completed < current.total;
  if (current.connectionLost || failed > 0 || stoppedEarly) return;

  closeSessionImport();
  addToast('info', completionMessage(imported, threads, skipped));
}

function completionMessage(imported: number, threads: number, skipped: number): string {
  const skippedSuffix = skipped > 0 ? ` ${countNoun(skipped, 'session')} skipped.` : '';
  if (imported === 0) {
    return skipped > 0
      ? `Nothing to import —${skippedSuffix}`
      : 'Nothing to import — those sessions are already here.';
  }
  return `Imported ${countNoun(imported, 'session')} (${countNoun(threads, 'thread')}).${skippedSuffix}`;
}

/**
 * The transport reported a GAP on the progress channel: frames this client
 * missed had already fallen out of the server's replay ring, so those
 * outcomes are gone for good.
 *
 * This is NOT the handler for a socket blip. A reconnect replays the
 * channel, terminal frame included, and killing the run on the status
 * transition would discard exactly the frames that finish it — the modal
 * would sit on "Connection lost" while the backend's run completed. Only a
 * gap is proof that frames were lost.
 *
 * What a gap proves is narrow, and the state it writes is narrow to match.
 * It says PAST frames were dropped; it says nothing about the backend run,
 * which is still executing and still emitting. So the run stops being
 * ACTIVE — the terminal frame may itself have been in the lost range, and a
 * modal that can never be closed is not an acceptable outcome of a dropped
 * socket — but it does NOT become terminal: every live frame that follows
 * still folds, and an eventual `done` still settles the run. The surface
 * stays open either way, because what landed is now partly unknown and
 * closing on that would look like success.
 */
export function markImportConnectionLost(): void {
  const current = run;
  if (!current || !current.active) return;
  current.active = false;
  current.stopRequested = false;
  current.connectionLost = true;
  // The gap hides what the run got through, so the catalog cannot be
  // trusted to still be an accurate offer.
  catalogStale = true;
  // A gap also means the imported rows' own `project:updated` /
  // `thread:updated` frames were among the ones dropped, and those are the
  // only thing that puts an imported thread in the sidebar. This is the one
  // place a whole-list resync is still owed: on the ordinary path the
  // per-row frames arrive and this call would repeat them.
  refreshSidebarProjections();
}

/** Test-only fixture isolation. */
export function resetSessionImportForTest(): void {
  open = false;
  status = 'idle';
  catalogError = '';
  providers = [];
  rows = [];
  selected = new Set();
  providerFilter = 'all';
  projectFilter = null;
  query = '';
  showAlreadyRan = false;
  run = null;
  inFlightScan = null;
  starting = false;
  catalogStale = false;
}
