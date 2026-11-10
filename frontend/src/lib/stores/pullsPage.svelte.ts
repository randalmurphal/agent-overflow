// The Pull requests page's state (docs/specs/pull-requests.md, "The
// page"): one list subscription per computer the page shows, the rows it
// projects through the remembered filters and sort, the needs-you badge
// and each pane's selected PR.
//
// The list pump (docs/architecture/forge-transport.md, "The PR list pump")
// is held through an entity store keyed by computer: the first page pane
// on a computer subscribes, the last one leaving unsubscribes, and a
// reconnect subscribes again. `SubscribePRList` routes to the selected
// computer and its handle calls route home, so every call is pinned to the
// computer the page lists. `pulls:updated` frames carry the pump's
// sequence: the subscribe result and every frame rank against one
// watermark per computer, so an older frame never overwrites a newer
// snapshot. A subscribe answers from the computer's persisted copy at
// once, `stale` while the pump's first read runs; the page shows it with
// its refreshing mark rather than a loading state. The page's other
// halves: pullsPageRows.ts (projections),
// pullsPageFilters.svelte.ts (remembered filters), pullsPageAgent.svelte.ts
// (Open with agent), pipelinesPage.svelte.ts (the Pipelines tab).

import { SvelteMap } from 'svelte/reactivity';
import { untrack } from 'svelte';
import { RefreshPRList, SetPRListActive, SubscribePRList, UnsubscribePRList } from './bindings';
import type { PRListItem, PRListSnapshot } from './bindings';
import { createEntityStore } from './entityStore.svelte';
import { captureError, type CapturedError } from './errorReports.svelte';
import { applyPipelineFrame, applyPipelineLogFrame, resyncPipelinesAfterGap } from './pipelineCI.svelte';
import { setPullsNeedsYouCount } from './pullsAttention.svelte';
import { addToast } from './toast.svelte';
import { ensurePullsFilters, mrFilters } from './pullsPageFilters.svelte';
import {
  groupRows,
  listedRows,
  projectChoices,
  projectErrors,
  type PullsGroup,
  type PullsListedRow,
  type PullsProjectChoice,
  type PullsProjectError,
} from './pullsPageRows';
import { setPullsReceiver } from './pullsStore.svelte';
import { createPumpVotes } from './pumpVotes';
import { disposePRReviewStates } from './reviewPanePinned.svelte';
import { isTransportClassError } from './transportStatus.svelte';
import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import { hasScope, pageGrantsKnownTracked } from '../transport/scopes';
import { documentHidden } from '../utils/pageVisibility';
import type { PRRef } from '../utils/prReference';

/** A pane's selected PR, as listed when selected; `row` follows the list
 * while the PR stays listed. */
export interface PullsSelection {
  readonly key: string;
  readonly backend: BackendKey;
  readonly projectId: string;
  readonly pr: PRRef;
  readonly row: PRListItem;
}

// Every list call is a `git:operate` method.
const LIST_SCOPE = 'git:operate';
const UNGRANTED_MESSAGE = 'This device is not allowed to read pull requests on this computer.';

// ---------------------------------------------------------------------------
// The list subscription
// ---------------------------------------------------------------------------

// The highest snapshot sequence applied per computer. Cleared when a
// source run starts: a restarted backend's pump numbers from one again.
const appliedSeqByBackend = new Map<BackendKey, number>();
// The live handle per computer, for the votes and the refresh.
const subscriptionByBackend = new Map<BackendKey, string>();
// The latest source run per computer, settled either way: a gap resync
// hands it to the recovery fence.
const sourceRunByBackend = new Map<BackendKey, Promise<void>>();
const votes = createPumpVotes('pullsPage', (id, active) => SetPRListActive(id, active));

function admit(backend: BackendKey, seq: number): boolean {
  const applied = appliedSeqByBackend.get(backend);
  if (applied !== undefined && seq <= applied) return false;
  appliedSeqByBackend.set(backend, seq);
  return true;
}

const listStore = createEntityStore<PRListSnapshot, BackendKey>({
  name: 'pullsList',
  backendForKey: (backend) => backend,
  rawValue: true,
  source: (args) => {
    const run = subscribeList(args.key, args.apply, args.signal);
    sourceRunByBackend.set(args.key, run.then(() => undefined, () => undefined));
    return run;
  },
  onApply: (backend, snapshot) => pages.get(backend)?.applied(snapshot),
});

async function subscribeList(
  backend: BackendKey,
  apply: (snapshot: PRListSnapshot) => void,
  signal: AbortSignal,
): Promise<() => Promise<void>> {
  appliedSeqByBackend.delete(backend);
  const result = await withBackendTarget(backend, () => SubscribePRList());
  const id = String(result.id);
  const cleanup = async (): Promise<void> => {
    if (subscriptionByBackend.get(backend) === id) subscriptionByBackend.delete(backend);
    try {
      await withBackendTarget(backend, () => UnsubscribePRList(id));
    } catch (err) {
      // A dead wire needs no unsubscribe: the backend releases the
      // connection's subscriptions when it drops.
      if (!isTransportClassError(err)) throw err;
    }
  };
  if (signal.aborted) return cleanup;
  subscriptionByBackend.set(backend, id);
  if (documentHidden()) votes.setActive(backend, id, false);
  // A frame that landed while the subscribe waited for the first tick
  // and outranks its answer already holds a newer snapshot.
  const snapshot = result.snapshot;
  if (admit(backend, Number(snapshot?.seq ?? 0)) && snapshot) apply(snapshot);
  return cleanup;
}

// ---------------------------------------------------------------------------
// Per computer
// ---------------------------------------------------------------------------

class PullsPageBackend {
  readonly panes = new Set<string>();
  refreshing = $state(false);
  refreshFailure = $state.raw<CapturedError | null>(null);
  refreshSeq = 0;

  /** The grants are known and withhold the list's scope. */
  readonly refused = $derived.by(() => pageGrantsKnownTracked() && !hasScope(LIST_SCOPE, this.backend));
  readonly snapshot = $derived.by(() => listStore.peek(this.backend));
  readonly error = $derived.by(() => listStore.peekError(this.backend));
  readonly rows: readonly PullsListedRow[] = $derived.by(() => listedRows(this.snapshot, this.backend));
  readonly rowsByKey = $derived(new Map(this.rows.map((row) => [row.key, row])));
  readonly groups: readonly PullsGroup[] = $derived.by(() => groupRows(this.snapshot, this.backend, mrFilters(this.backend)));
  readonly projects: readonly PullsProjectChoice[] = $derived(projectChoices(this.snapshot));
  readonly errors: readonly PullsProjectError[] = $derived.by(() => projectErrors(this.snapshot, mrFilters(this.backend).projects, this.backend));

  // Holds the list while the computer grants its scope: a grant that
  // arrives subscribes, a revoked one releases the subscription.
  readonly #disposeGate: () => void;

  constructor(readonly backend: BackendKey) {
    this.#disposeGate = $effect.root(() => {
      $effect(() => {
        if (!hasScope(LIST_SCOPE, backend)) return;
        const attachment = listStore.attach(backend, backend);
        return () => attachment.release();
      });
    });
  }

  applied(snapshot: PRListSnapshot): void {
    setPullsNeedsYouCount(this.backend, snapshot.needsYou);
  }

  dispose(): void {
    this.#disposeGate();
    sourceRunByBackend.delete(this.backend);
  }
}

const pages = new SvelteMap<BackendKey, PullsPageBackend>();

// ---------------------------------------------------------------------------
// Per pane
// ---------------------------------------------------------------------------

class PaneEntry {
  selection = $state.raw<PullsSelection | null>(null);
  constructor(readonly backend: BackendKey, readonly attachment: number) {}
}

const paneEntries = new SvelteMap<string, PaneEntry>();
let nextAttachment = 0;

type PullsPageDetachedObserver = (paneId: string) => void;
let detachedObservers: readonly PullsPageDetachedObserver[] = [];

/** Observes a pane's page detaching, for state kept beside the page that
 * must go with it. Returns the unsubscribe. */
export function onPullsPageDetached(observer: PullsPageDetachedObserver): () => void {
  detachedObservers = [...detachedObservers, observer];
  return () => {
    detachedObservers = detachedObservers.filter((existing) => existing !== observer);
  };
}
let receiverInstalled = false;

/** Installs the page's handlers on the Pull requests channels
 * (pullsStore.svelte.ts). Idempotent; the first page or pipeline surface
 * to attach installs them. */
export function installPullsReceiver(): void {
  if (receiverInstalled) return;
  receiverInstalled = true;
  setPullsReceiver({
    pulls: applyPullsSnapshot,
    pipeline: applyPipelineFrame,
    pipelineLog: applyPipelineLogFrame,
    pullsGap: resyncPullsAfterGap,
    pipelinesGap: resyncPipelinesAfterGap,
  });
}

/** The pane's page mounted: it lists `backend`'s PRs until detached. */
export function attachPullsPage(paneId: string, backend: BackendKey): void {
  untrack(() => {
    const existing = paneEntries.get(paneId);
    if (existing?.backend === backend) return;
    if (existing) detachPullsPage(paneId);
    installPullsReceiver();
    ensurePullsFilters(backend);
    let page = pages.get(backend);
    if (!page) {
      page = new PullsPageBackend(backend);
      pages.set(backend, page);
    }
    paneEntries.set(paneId, new PaneEntry(backend, ++nextAttachment));
    page.panes.add(paneId);
  });
}

/** The pane's page went: its selection, review states and hold on the
 * list go with it; the last page on a computer unsubscribes. */
export function detachPullsPage(paneId: string): void {
  untrack(() => {
    const entry = paneEntries.get(paneId);
    if (!entry) return;
    paneEntries.delete(paneId);
    disposePRReviewStates(paneId);
    for (const observer of detachedObservers) observer(paneId);
    const page = pages.get(entry.backend);
    if (page) {
      page.panes.delete(paneId);
      if (page.panes.size === 0) {
        page.dispose();
        pages.delete(entry.backend);
      }
    }
  });
}

/** A `pulls:updated` frame from `backend`. */
export function applyPullsSnapshot(snapshot: PRListSnapshot, backend: BackendKey): void {
  if (!admit(backend, Number(snapshot.seq ?? 0))) return;
  listStore.apply(backend, snapshot);
}

/** A gap on `pulls:updated`: the pump emits only on change against its
 * own last snapshot, so the list subscribes again for a fresh one. Answers
 * that subscribe's settling, or undefined when none started. */
export function resyncPullsAfterGap(backend: BackendKey): Promise<void> | undefined {
  if (!pages.has(backend)) return undefined;
  const before = sourceRunByBackend.get(backend);
  listStore.invalidate(backend);
  const run = sourceRunByBackend.get(backend);
  return run === before ? undefined : run;
}

if (typeof document !== 'undefined') {
  document.addEventListener('visibilitychange', () => {
    const active = !documentHidden();
    for (const [backend, id] of subscriptionByBackend) votes.setActive(backend, id, active);
  });
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

/** The computer a pane's page lists; null before it attached. */
export function pullsPageBackend(paneId: string): BackendKey | null {
  return paneEntries.get(paneId)?.backend ?? null;
}

/** The pane's page attachment, new on every attach (the pane id outlives
 * a closed and reopened page); null while no page is attached. */
export function pullsPageAttachment(paneId: string): number | null {
  return untrack(() => paneEntries.get(paneId)?.attachment ?? null);
}

function pageOf(paneId: string): PullsPageBackend | null {
  const entry = paneEntries.get(paneId);
  return entry ? (pages.get(entry.backend) ?? null) : null;
}

/** Reactive: the list has not answered yet. */
export function pullsLoading(paneId: string): boolean {
  const page = pageOf(paneId);
  return page !== null && !page.refused && page.snapshot === null && page.error === null;
}

/** Reactive: the rows shown are the computer's persisted copy and the
 * pump's read is still running (the subscribe answered `stale`). */
export function pullsStale(paneId: string): boolean {
  return pageOf(paneId)?.snapshot?.stale ?? false;
}

/** Reactive: the list subscription failed (it retries), or this device
 * may not read the list; null when healthy. */
export function pullsError(paneId: string): string | null {
  const page = pageOf(paneId);
  if (page?.refused) return UNGRANTED_MESSAGE;
  return page?.error ?? null;
}

/** Reactive: the snapshot's own failure (no projects read); null when none. */
export function pullsSnapshotError(paneId: string): string | null {
  return pageOf(paneId)?.snapshot?.error || null;
}

export function pullsProjects(paneId: string): readonly PullsProjectChoice[] {
  return pageOf(paneId)?.projects ?? [];
}

/** The Merge requests tab's rows after the filter and project picker. */
export function pullsRowsByGroup(paneId: string): readonly PullsGroup[] {
  return pageOf(paneId)?.groups ?? [];
}

export function pullsProjectErrors(paneId: string): readonly PullsProjectError[] {
  return pageOf(paneId)?.errors ?? [];
}

/** The needs-you count over every project, whatever the filters. */
export function pullsNeedsYouCount(paneId: string): number {
  return pageOf(paneId)?.snapshot?.needsYou ?? 0;
}

export function pullsRefreshing(paneId: string): boolean {
  return pageOf(paneId)?.refreshing ?? false;
}

/** Reactive: the last refresh call's failure as the error surface shows
 * it (details and copy), until the next refresh. */
export function pullsRefreshFailure(paneId: string): CapturedError | null {
  return pageOf(paneId)?.refreshFailure ?? null;
}

/** The header's refresh: the pump reads every project now. Per-project
 * failures arrive in the next snapshot; a
 * refused call is the page's refresh failure until the next refresh, and
 * a device without the scope reads as a toast. */
export async function refreshPullsPage(paneId: string): Promise<void> {
  const page = pageOf(paneId);
  if (!page) return;
  if (!hasScope(LIST_SCOPE, page.backend)) {
    addToast('error', UNGRANTED_MESSAGE);
    return;
  }
  const id = subscriptionByBackend.get(page.backend);
  // No live handle: the subscribe in flight reads every project.
  if (!id) return;
  const seq = ++page.refreshSeq;
  page.refreshing = true;
  page.refreshFailure = null;
  try {
    await withBackendTarget(page.backend, () => RefreshPRList(id));
  } catch (err) {
    if (seq === page.refreshSeq) page.refreshFailure = captureError(err, { context: 'Could not refresh the merge requests' });
  } finally {
    if (seq === page.refreshSeq) page.refreshing = false;
  }
}

// ---------------------------------------------------------------------------
// Selection
// ---------------------------------------------------------------------------

/** Reactive: the pane's selected PR, its row as listed now. */
export function selectedPR(paneId: string): PullsSelection | null {
  const entry = paneEntries.get(paneId);
  const selection = entry?.selection ?? null;
  if (!selection) return null;
  const listed = pages.get(selection.backend)?.rowsByKey.get(selection.key);
  return listed && listed.item !== selection.row ? { ...selection, row: listed.item } : selection;
}

/** Reactive: a listed PR by row key as a selection would hold it,
 * without selecting it (a row's menu acts on its own PR). */
export function listedPR(paneId: string, key: string): PullsSelection | null {
  const entry = paneEntries.get(paneId);
  const row = entry ? pages.get(entry.backend)?.rowsByKey.get(key) : undefined;
  if (!entry || !row) return null;
  return { key, backend: entry.backend, projectId: row.project.projectId, pr: row.pr, row: row.item };
}

/** Selects a listed PR by row key; false when the list holds none. */
export function selectPR(paneId: string, key: string): boolean {
  return untrack(() => {
    const entry = paneEntries.get(paneId);
    const row = entry ? pages.get(entry.backend)?.rowsByKey.get(key) : undefined;
    if (!entry || !row) return false;
    entry.selection = { key, backend: entry.backend, projectId: row.project.projectId, pr: row.pr, row: row.item };
    return true;
  });
}

export function clearSelection(paneId: string): void {
  untrack(() => {
    const entry = paneEntries.get(paneId);
    if (entry) entry.selection = null;
  });
}

/** The compact probe: a PR's review is open above the list. */
export function detailOpen(paneId: string): boolean {
  return (paneEntries.get(paneId)?.selection ?? null) !== null;
}

// ---------------------------------------------------------------------------
// Test seam
// ---------------------------------------------------------------------------

export function __resetPullsPageForTest(): void {
  for (const paneId of paneEntries.keys()) detachPullsPage(paneId);
  listStore.suspend();
  listStore.resetAll();
  appliedSeqByBackend.clear();
  subscriptionByBackend.clear();
  sourceRunByBackend.clear();
  votes.resetForTest();
  receiverInstalled = false;
}
