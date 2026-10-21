// The Pull requests page's state (docs/specs/pull-requests.md, "The
// page"): one list subscription per computer the page shows, the rows it
// projects through the remembered filters, the needs-you badge, the focus
// set whose detail the pump reads, each pane's selected PR, and marking a
// PR viewed while its review is on screen.
//
// The list pump (docs/architecture/forge-transport.md, "The PR list pump")
// is held through an entity store keyed by computer: the first page pane
// on a computer subscribes, the last one leaving unsubscribes, and a
// reconnect subscribes again. `SubscribePRList` routes to the selected
// computer and its handle calls route home, so every call is pinned to the
// computer the page lists. `pulls:updated` frames carry the pump's
// sequence: the subscribe result and every frame rank against one
// watermark per computer, so an older frame never overwrites a newer
// snapshot. The page's other halves: pullsPageRows.ts (projections),
// pullsPageFilters.svelte.ts (remembered filters), pullsPageAgent.svelte.ts
// (Open with agent), pipelinesPage.svelte.ts (the Pipelines tab).

import { SvelteMap } from 'svelte/reactivity';
import { untrack } from 'svelte';
import { MarkPRViewed, RefreshPRList, SetPRListActive, SetPRListFocus, SubscribePRList, UnsubscribePRList } from './bindings';
import type { PRListItem, PRListSnapshot, PRSeen } from './bindings';
import { createEntityStore } from './entityStore.svelte';
import { getCompactScreen, isCompactLayout, onScreenCompactPaneId } from './layoutMode.svelte';
import { applyPipelineFrame, applyPipelineLogFrame, resyncPipelinesAfterGap } from './pipelineCI.svelte';
import { onPRSnapshotApplied, type PRSnapshotApplied } from './prReviewStore.svelte';
import { setPullsNeedsYouCount } from './pullsAttention.svelte';
import { ensurePullsFilters, mrFilters } from './pullsPageFilters.svelte';
import {
  focusKeys,
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
import { disposePRReviewStates } from './reviewPane.svelte';
import { isTransportClassError } from './transportStatus.svelte';
import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import { errString } from '../utils/errors';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { documentHidden } from '../utils/pageVisibility';
import { prReferenceWire, type PRRef } from '../utils/prReference';
import { createRefreshScheduler, type RefreshScheduler } from '../utils/refreshScheduler';

/** A pane's selected PR, as listed when selected; `row` follows the list
 * while the PR stays listed. */
export interface PullsSelection {
  readonly key: string;
  readonly backend: BackendKey;
  readonly projectId: string;
  readonly pr: PRRef;
  readonly row: PRListItem;
}

const FOCUS_DELAY_MS = 300;
const FOCUS_MAX_WAIT_MS = 1_500;

// ---------------------------------------------------------------------------
// The list subscription
// ---------------------------------------------------------------------------

// The highest snapshot sequence applied per computer. Cleared when a
// source run starts: a restarted backend's pump numbers from one again.
const appliedSeqByBackend = new Map<BackendKey, number>();
// The live handle per computer, for the votes, the focus and the refresh.
const subscriptionByBackend = new Map<BackendKey, string>();
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
  source: async ({ key: backend, apply, signal }) => {
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
    // A new handle focuses nothing.
    pages.get(backend)?.subscribed();
    return cleanup;
  },
  onApply: (backend, snapshot) => pages.get(backend)?.applied(snapshot),
});

// ---------------------------------------------------------------------------
// Per computer
// ---------------------------------------------------------------------------

class PullsPageBackend {
  readonly panes = new Set<string>();
  attachment: { release(): void } | null = null;
  refreshing = $state(false);
  refreshError = $state<string | null>(null);
  refreshSeq = 0;
  /** Seen records this client wrote (MarkPRViewed answers) by row key,
   * shown until the list's own record catches up. */
  readonly seenOverrides = new SvelteMap<string, PRSeen>();
  /** `<row key>\n<head>` pairs marked viewed. */
  readonly marked = new Set<string>();
  readonly focus: RefreshScheduler;
  sentFocus: string | null = null;

  readonly snapshot = $derived.by(() => listStore.peek(this.backend));
  readonly error = $derived.by(() => listStore.peekError(this.backend));
  readonly rows: readonly PullsListedRow[] = $derived.by(() => listedRows(this.snapshot, this.backend));
  readonly rowsByKey = $derived(new Map(this.rows.map((row) => [row.key, row])));
  readonly groups: readonly PullsGroup[] = $derived.by(() => groupRows(this.snapshot, this.backend, mrFilters(this.backend), this.seenOverrides));
  readonly projects: readonly PullsProjectChoice[] = $derived(projectChoices(this.snapshot));
  readonly errors: readonly PullsProjectError[] = $derived.by(() => projectErrors(this.snapshot, mrFilters(this.backend).projects));

  constructor(readonly backend: BackendKey) {
    this.focus = createRefreshScheduler({
      name: `pullsFocus:${backend}`,
      delayMs: FOCUS_DELAY_MS,
      maxWaitMs: FOCUS_MAX_WAIT_MS,
      run: (token) => this.sendFocus(token),
    });
  }

  subscribed(): void {
    this.sentFocus = null;
    this.focus.request({ immediate: true });
  }

  applied(snapshot: PRListSnapshot): void {
    if (badgeBackend() === this.backend) setPullsNeedsYouCount(snapshot.needsYou);
    // A record this client wrote goes once the list's own catches up, or
    // the PR leaves the list.
    if (this.seenOverrides.size > 0) {
      const byKey = new Map(listedRows(snapshot, this.backend).map((row) => [row.key, row.item]));
      for (const [key, seen] of this.seenOverrides) {
        const row = byKey.get(key);
        if (!row || (row.seen !== null && row.seen.openedAt >= seen.openedAt)) this.seenOverrides.delete(key);
      }
    }
    // Rows on screen may have joined or left the list.
    this.focus.request();
  }

  async sendFocus(token: { isCurrent(): boolean }): Promise<void> {
    const id = subscriptionByBackend.get(this.backend);
    if (!id) return;
    const onScreen = new Set<string>();
    for (const paneId of this.panes) for (const key of paneEntries.get(paneId)?.onScreen ?? []) onScreen.add(key);
    const keys = focusKeys(this.rows, onScreen);
    const stamp = `${id}\n${keys.join('\n')}`;
    if (stamp === this.sentFocus) return;
    try {
      const result = await withBackendTarget(this.backend, () => SetPRListFocus(id, keys));
      if (!token.isCurrent()) return;
      this.sentFocus = stamp;
      const unknown = result?.unknown ?? [];
      if (unknown.length > 0) reportFrontendDiagnostic('pulls: focused rows the list does not hold', `${unknown.length} of ${keys.length}`);
    } catch (err) {
      // A dead wire takes the handle with it; the resubscribe restates.
      if (isTransportClassError(err)) return;
      reportFrontendDiagnostic('pulls: setting the focused rows failed', errString(err));
    }
  }

  dispose(): void {
    this.focus.dispose();
    this.attachment?.release();
    this.attachment = null;
  }
}

const pages = new SvelteMap<BackendKey, PullsPageBackend>();

// ---------------------------------------------------------------------------
// Per pane
// ---------------------------------------------------------------------------

class PaneEntry {
  selection = $state.raw<PullsSelection | null>(null);
  onScreen: ReadonlySet<string> = new Set();
  constructor(readonly backend: BackendKey) {}
}

const paneEntries = new SvelteMap<string, PaneEntry>();
// Attach order, newest last: the badge counts the newest page's computer.
const attachOrder: string[] = [];

function badgeBackend(): BackendKey | null {
  const paneId = attachOrder[attachOrder.length - 1];
  return paneId === undefined ? null : (paneEntries.get(paneId)?.backend ?? null);
}

let receiverInstalled = false;
let unobserveSnapshots: (() => void) | null = null;

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
    unobserveSnapshots ??= onPRSnapshotApplied(markViewedOnSnapshot);
    ensurePullsFilters(backend);
    let page = pages.get(backend);
    if (!page) {
      page = new PullsPageBackend(backend);
      pages.set(backend, page);
    }
    paneEntries.set(paneId, new PaneEntry(backend));
    attachOrder.push(paneId);
    page.panes.add(paneId);
    page.attachment ??= listStore.attach(backend, backend);
    const snapshot = listStore.snapshot(backend);
    if (snapshot) setPullsNeedsYouCount(snapshot.needsYou);
  });
}

/** The pane's page went: its selection, review states and hold on the
 * list go with it; the last page on a computer unsubscribes. */
export function detachPullsPage(paneId: string): void {
  untrack(() => {
    const entry = paneEntries.get(paneId);
    if (!entry) return;
    paneEntries.delete(paneId);
    attachOrder.splice(attachOrder.indexOf(paneId), 1);
    disposePRReviewStates(paneId);
    const page = pages.get(entry.backend);
    if (page) {
      page.panes.delete(paneId);
      if (page.panes.size === 0) {
        page.dispose();
        pages.delete(entry.backend);
      } else {
        page.focus.request();
      }
    }
    const badge = badgeBackend();
    setPullsNeedsYouCount(badge === null ? 0 : (listStore.snapshot(badge)?.needsYou ?? 0));
  });
}

/** A `pulls:updated` frame from `backend`. */
export function applyPullsSnapshot(snapshot: PRListSnapshot, backend: BackendKey): void {
  if (!admit(backend, Number(snapshot.seq ?? 0))) return;
  listStore.apply(backend, snapshot);
}

/** A gap on `pulls:updated`: the pump emits only on change against its
 * own last snapshot, so the list subscribes again for a fresh one. */
export function resyncPullsAfterGap(backend: BackendKey): void {
  if (pages.has(backend)) listStore.invalidate(backend);
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

function pageOf(paneId: string): PullsPageBackend | null {
  const entry = paneEntries.get(paneId);
  return entry ? (pages.get(entry.backend) ?? null) : null;
}

/** Reactive: the list has not answered yet. */
export function pullsLoading(paneId: string): boolean {
  const page = pageOf(paneId);
  return page !== null && page.snapshot === null && page.error === null;
}

/** Reactive: the list subscription failed (it retries); null when healthy. */
export function pullsError(paneId: string): string | null {
  return pageOf(paneId)?.error ?? null;
}

/** Reactive: the snapshot's own failure (no projects read); null when none. */
export function pullsSnapshotError(paneId: string): string | null {
  return pageOf(paneId)?.snapshot?.error || null;
}

/** Reactive: the sequence of the snapshot shown; 0 before one. */
export function pullsSnapshotSeq(backend: BackendKey): number {
  return pages.get(backend)?.snapshot?.seq ?? 0;
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

/** Reactive: the last refresh call's failure, until the next refresh. */
export function pullsRefreshError(paneId: string): string | null {
  return pageOf(paneId)?.refreshError ?? null;
}

/** The header's refresh: the pump reads every project and the focused
 * rows' detail now. Per-project failures arrive in the next snapshot. */
export async function refreshPullsPage(paneId: string): Promise<void> {
  const page = pageOf(paneId);
  if (!page) return;
  const id = subscriptionByBackend.get(page.backend);
  // No live handle: the subscribe in flight reads every project.
  if (!id) return;
  const seq = ++page.refreshSeq;
  page.refreshing = true;
  page.refreshError = null;
  try {
    await withBackendTarget(page.backend, () => RefreshPRList(id));
  } catch (err) {
    if (seq === page.refreshSeq) page.refreshError = errString(err);
  } finally {
    if (seq === page.refreshSeq) page.refreshing = false;
  }
}

// ---------------------------------------------------------------------------
// Selection and focus
// ---------------------------------------------------------------------------

/** Reactive: the pane's selected PR, its row as listed now. */
export function selectedPR(paneId: string): PullsSelection | null {
  const entry = paneEntries.get(paneId);
  const selection = entry?.selection ?? null;
  if (!selection) return null;
  const listed = pages.get(selection.backend)?.rowsByKey.get(selection.key);
  return listed && listed.item !== selection.row ? { ...selection, row: listed.item } : selection;
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

/** The rows the pane shows on screen, by row key. The focus set is their
 * union over the computer's panes. */
export function setFocusedRows(paneId: string, keys: Iterable<string>): void {
  untrack(() => {
    const entry = paneEntries.get(paneId);
    if (!entry) return;
    entry.onScreen = new Set(keys);
    pages.get(entry.backend)?.focus.request();
  });
}

// ---------------------------------------------------------------------------
// Viewed marks
// ---------------------------------------------------------------------------

// The pane's review is what the reader is looking at: on desktop the page
// is mounted, on compact the thread screen shows this pane.
function paneShowing(paneId: string): boolean {
  if (!isCompactLayout()) return true;
  return getCompactScreen() === 'thread' && onScreenCompactPaneId() === paneId;
}

function markViewedOnSnapshot(key: string, applied: PRSnapshotApplied): void {
  if (!applied.visible || !applied.headSHA) return;
  for (const [paneId, entry] of paneEntries) {
    const selection = entry.selection;
    if (selection?.key !== key || !paneShowing(paneId)) continue;
    const page = pages.get(entry.backend);
    if (page) void markViewed(page, selection, applied.headSHA);
  }
}

async function markViewed(page: PullsPageBackend, selection: PullsSelection, headSHA: string): Promise<void> {
  const tag = `${selection.key}\n${headSHA}`;
  if (page.marked.has(tag)) return;
  page.marked.add(tag);
  try {
    const seen = await withBackendTarget(page.backend, () => MarkPRViewed(prReferenceWire(selection.pr), headSHA));
    if (pages.get(page.backend) === page && seen) page.seenOverrides.set(selection.key, seen);
  } catch (err) {
    // The next snapshot of this head tries again.
    page.marked.delete(tag);
    if (!isTransportClassError(err)) reportFrontendDiagnostic('pulls: marking a PR viewed failed', errString(err));
  }
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
  votes.resetForTest();
  unobserveSnapshots?.();
  unobserveSnapshots = null;
  receiverInstalled = false;
}
