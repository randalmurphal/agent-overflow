// The live workflow cache the overlay reads (UI-SPEC §2.1: "Data rides the
// typed `workflow:*` event channel; RPCs through `stores/bindings.ts`").
//
// Two hydration scopes, deliberately different in weight:
//   - attention — one `WorkflowListUnresolvedItems('')` at boot so the footer
//     badge is authoritative on app open (§7: the badge re-surfaces, a missed
//     notification never loses work). Summaries only: no snapshot, no seeds,
//     no digest.
//   - overlay — the full listing plus per-project catalogs, automations and
//     costs, loaded when the overlay opens.
//
// Run DETAIL (phases, units, artifacts, outputs, spend) is loaded for the ONE
// run the overlay is looking at and evicted the moment it looks at another, so
// frontend memory stays bounded by what is on screen (root CLAUDE.md principle
// 4). It is deliberately root-only: a run's SHAPE — the whole called tree, its
// waves, its frontier — belongs to `stores/workflowRunMap.svelte.ts`, which
// fetches the tree in one call and patches it from events. Nothing here walks
// children, and nothing here should start again (RUN-MAP §4.2).

import type {
  WorkItem,
  WorkflowAutomationView,
  WorkflowDefinitionCatalog,
  WorkflowEngineStateEvent,
  WorkflowItemDetail,
  WorkflowItemStateEvent,
  WorkflowPhaseStateEvent,
  WorkflowResolvedReceipt,
  WorkflowSoftStopEvent,
} from '../types/workflow';
import {
  WorkflowGetEngineState,
  WorkflowGetItem,
  WorkflowListAutomations,
  WorkflowListDefinitions,
  WorkflowListItemCosts,
  WorkflowListItems,
  WorkflowListUnresolvedItems,
} from './bindings';
import { addToast } from './toast.svelte';
import { userFacingError } from '../utils/userFacingError';
import { patchWorkflowItems, patchWorkflowSoftStop, workflowAttentionCount } from './workflowData';
import { readComputerRows, retainUnavailableComputerRows } from './computerRows';
import { attachedBackends, backendById, onBackendDetached, onBackendsChanged, requireEntityBackend, withBackendTarget } from '../transport/backends';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { noteProject, noteThread, noteWorkflowItem, projectBackend, workflowItemBackend } from '../transport/entityIndex';
import { getAttachedBackends } from './attachedBackends.svelte';

const REFRESH_DEBOUNCE_MS = 200;

export interface WorkflowLivePhase {
  phaseId: string;
  attempt: number;
  status: string;
  unitId: string;
}

let runs = $state<WorkItem[]>([]);
let costs = $state<Record<string, number>>({});
let catalogs = $state(new Map<string, WorkflowDefinitionCatalog>());
let automations = $state(new Map<string, WorkflowAutomationView[]>());
let details = $state(new Map<string, WorkflowItemDetail>());
let livePhases = $state(new Map<string, WorkflowLivePhase>());
let receipts = $state(new Map<string, WorkflowResolvedReceipt>());
let paused = $state(new Map<BackendKey, boolean>());
let overlayLoaded = $state(false);
let loading = $state(false);
let loadError = $state<string | null>(null);

let lastProjectIds: string[] = [];
let refreshTimer: ReturnType<typeof setTimeout> | null = null;
let inFlight: Promise<void> | null = null;
const detailLoads = new Map<string, { promise: Promise<WorkflowItemDetail | null> | null }>();
let listRevision = 0;
let projectRevision = 0;
let attentionRequested = false;
const engineRevisions = new Map<BackendKey, number>();

export function getWorkflowRuns(): readonly WorkItem[] {
  return runs;
}

export function getWorkflowRun(itemId: string): WorkItem | undefined {
  return runs.find((item) => item.id === itemId);
}

export function getWorkflowAttentionCount(): number {
  return workflowAttentionCount(runs);
}

export function getWorkflowCosts(): Readonly<Record<string, number>> {
  return costs;
}

export function getWorkflowCatalog(projectId: string): WorkflowDefinitionCatalog | undefined {
  return catalogs.get(projectId);
}

export function getWorkflowAutomations(projectId: string): readonly WorkflowAutomationView[] {
  return automations.get(projectId) ?? [];
}

export function getWorkflowDetail(itemId: string): WorkflowItemDetail | undefined {
  return details.get(itemId);
}

export function getWorkflowLivePhase(itemId: string): WorkflowLivePhase | undefined {
  return livePhases.get(itemId);
}

export function getWorkflowReceipts(): ReadonlyMap<string, WorkflowResolvedReceipt> {
  return receipts;
}

export function getWorkflowReceipt(itemId: string): WorkflowResolvedReceipt | undefined {
  return receipts.get(itemId);
}

export function isWorkflowEnginePaused(backend?: BackendKey): boolean {
  if (backend !== undefined) return paused.get(backend) === true;
  const computers = getAttachedBackends();
  return computers.length > 0 && computers.every((entry) => paused.get(entry.id) === true);
}

export function isWorkflowOverlayLoaded(): boolean {
  return overlayLoaded;
}

export function isWorkflowLoading(): boolean {
  return loading;
}

export function getWorkflowLoadError(): string | null {
  return loadError;
}

/**
 * Session receipts (§4.3 "resolved (this session)"). They keep an acted-on run
 * in the sweep set long enough to show its green receipt before the cursor
 * advances, and are dropped when the overlay closes.
 */
export function recordWorkflowReceipt(receipt: WorkflowResolvedReceipt): void {
  receipts = new Map(receipts).set(receipt.itemId, receipt);
}

export function clearWorkflowReceipts(): void {
  if (receipts.size === 0) return;
  receipts = new Map();
}

/**
 * Boot hydration for the footer badge. Never toasts: a backend that has no
 * workflow engine yet is the ordinary case, and the sidebar must not shout
 * about it on every launch.
 */
export async function hydrateWorkflowAttention(): Promise<void> {
  attentionRequested = true;
  try {
    await readRuns(false);
  } catch (err) {
    console.warn('workflows: attention hydration failed:', err);
  }
}

async function readRuns(all: boolean): Promise<void> {
  const revision = ++listRevision;
  const result = await readComputerRows<WorkItem>(
    () => all ? WorkflowListItems('') : WorkflowListUnresolvedItems(''),
    (item, backend) => {
      noteWorkflowItem(item.id, backend);
      if (item.projectId) noteProject(item.projectId, backend);
      if (item.triageThreadId) noteThread(item.triageThreadId, backend);
    }, undefined, undefined, (late) => {
      if (revision === listRevision) runs = retainUnavailableComputerRows(runs, late, (item) => workflowItemBackend(item.id));
    });
  if (result && revision === listRevision) runs = retainUnavailableComputerRows(runs, result, (item) => workflowItemBackend(item.id));
}

/**
 * Full overlay hydration. `projectIds` are the projects the sidebar knows
 * about; projects carrying runs are added so a run in a project the caller
 * did not list still gets its catalog.
 */
export async function loadWorkflowsOverlayData(projectIds: readonly string[]): Promise<void> {
  lastProjectIds = [...projectIds];
  if (inFlight) return inFlight;
  loading = true;
  inFlight = (async () => {
    try {
      await Promise.all([
        readRuns(true),
        ...attachedBackends().map((entry) => resyncWorkflowEngineState(entry.id)),
      ]);
      const ids = new Set(lastProjectIds.filter(Boolean));
      for (const item of runs) if (item.projectId) ids.add(item.projectId);
      await loadProjectScopedData([...ids]);
      overlayLoaded = true;
      loadError = null;
    } catch (err) {
      loadError = userFacingError(err, 'Could not load workflows.');
      addToast('error', loadError);
    } finally {
      loading = false;
      inFlight = null;
    }
  })();
  return inFlight;
}

async function loadProjectScopedData(projectIds: readonly string[]): Promise<void> {
  const revision = ++projectRevision;
  const loaded = await Promise.all(projectIds.map(async (projectId) => {
    let backend: BackendKey;
    try { backend = requireEntityBackend(projectBackend(projectId)); }
    catch { return null; } // A removed project's old view may still be closing.
    const computer = backendById(backend);
    const [catalog, automations, costs] = await Promise.all([
      withBackendTarget(backend, () => WorkflowListDefinitions(projectId)).catch(() => null),
      withBackendTarget(backend, () => WorkflowListAutomations(projectId)).catch(() => null),
      withBackendTarget(backend, () => WorkflowListItemCosts(projectId)).catch(() => null),
    ]);
    return { projectId, backend, computer, catalog, automations, costs };
  }));
  if (revision !== projectRevision) return;
  const nextCatalogs = new Map<string, WorkflowDefinitionCatalog>();
  const nextAutomations = new Map<string, WorkflowAutomationView[]>();
  const nextCosts: Record<string, number> = {};
  for (const entry of loaded) {
    if (!entry) continue;
    if (backendById(entry.backend) !== entry.computer) continue;
    const catalog = entry.catalog ?? catalogs.get(entry.projectId);
    const automation = entry.automations ?? automations.get(entry.projectId);
    if (catalog) nextCatalogs.set(entry.projectId, catalog);
    if (automation) nextAutomations.set(entry.projectId, automation);
    if (!entry.costs) {
      for (const item of runs) if (item.projectId === entry.projectId && costs[item.id] !== undefined) nextCosts[item.id] = costs[item.id];
    }
    for (const [itemId, cost] of Object.entries(entry.costs ?? {})) {
      if (typeof cost === 'number' && Number.isFinite(cost)) nextCosts[itemId] = cost;
    }
  }
  catalogs = nextCatalogs;
  automations = nextAutomations;
  costs = nextCosts;
}

/** Re-list runs without re-fetching catalogs. Coalesced across event bursts. */
export function refreshWorkflowRunsSoon(): void {
  if (refreshTimer !== null) return;
  refreshTimer = setTimeout(() => {
    refreshTimer = null;
    if (!overlayLoaded && !attentionRequested) return;
    void (async () => {
      try {
        await readRuns(overlayLoaded);
      } catch (err) {
        console.warn('workflows: run refresh failed:', err);
      }
    })();
  }, REFRESH_DEBOUNCE_MS);
}

/**
 * Load one run's detail. Single-flight per run, so the mount effect and an
 * event-driven force reload landing together read once; `force` bypasses the
 * cache but still joins nothing, because a forced reload exists precisely to
 * replace what the cache holds.
 */
export async function loadWorkflowDetail(itemId: string, force = false): Promise<WorkflowItemDetail | null> {
  if (!itemId) return null;
  if (!force) {
    const cached = details.get(itemId);
    if (cached) return cached;
    const pending = detailLoads.get(itemId);
    if (pending?.promise) return pending.promise;
  }
  const pending = { promise: null as Promise<WorkflowItemDetail | null> | null };
  detailLoads.set(itemId, pending);
  const load = (async () => {
    try {
      const backend = requireEntityBackend(workflowItemBackend(itemId));
      const computer = backendById(backend);
      const detail = await withBackendTarget(backend, () => WorkflowGetItem(itemId));
      if (detailLoads.get(itemId) !== pending || backendById(backend) !== computer) return null;
      details = new Map(details).set(itemId, detail);
      return detail;
    } catch (err) {
      if (detailLoads.get(itemId) === pending) addToast('error', userFacingError(err, 'Could not load this run.'));
      return null;
    } finally {
      if (detailLoads.get(itemId) === pending) detailLoads.delete(itemId);
    }
  })();
  pending.promise = load;
  return load;
}

/**
 * Drop every cached detail but the run the overlay is looking at. No child
 * walk: only the focused root is ever loaded, so "keep" is one id.
 *
 * This runs inside an `$effect` that READS `details`, so a write when nothing
 * is dropped re-enters the effect forever. The guard is therefore "is the cache
 * already exactly the root", not a size comparison — `rootItemId` names the run
 * whether or not its detail has landed yet, and a size check would keep
 * rewriting an already-minimal cache.
 */
export function retainWorkflowDetails(rootItemId: string | null): void {
  for (const id of detailLoads.keys()) if (id !== rootItemId) detailLoads.delete(id);
  if (details.size === 0) return;
  const kept = rootItemId !== null ? details.get(rootItemId) : undefined;
  if (rootItemId === null || kept === undefined) {
    details = new Map();
    return;
  }
  if (details.size === 1) return;
  details = new Map([[rootItemId, kept]]);
}

export function applyWorkflowItemState(event: WorkflowItemStateEvent): void {
  if (!event || typeof event.itemId !== 'string' || event.itemId === '') return;
  listRevision++;
  const known = runs.some((item) => item.id === event.itemId);
  runs = known ? patchWorkflowItems(runs, event) : runs;
  // An event supersedes outstanding list reads, but carries only part of the
  // row (no endedAt, for example). Reconcile once after the event burst.
  refreshWorkflowRunsSoon();
  if (details.has(event.itemId)) void loadWorkflowDetail(event.itemId, true);
  if (event.to !== 'running') {
    const next = new Map(livePhases);
    if (next.delete(event.itemId)) livePhases = next;
  }
}

export function applyWorkflowPhaseState(event: WorkflowPhaseStateEvent): void {
  if (!event || typeof event.itemId !== 'string' || event.itemId === '') return;
  livePhases = new Map(livePhases).set(event.itemId, {
    phaseId: event.phaseId ?? '',
    attempt: event.attempt ?? 0,
    status: event.status ?? '',
    unitId: event.unitId ?? '',
  });
  if (details.has(event.itemId)) void loadWorkflowDetail(event.itemId, true);
}

export function applyWorkflowSoftStop(event: WorkflowSoftStopEvent): void {
  if (!event || typeof event.itemId !== 'string' || event.itemId === '') return;
  if (typeof event.armed !== 'boolean') return;
  listRevision++;
  runs = patchWorkflowSoftStop(runs, event.itemId, event.armed);
  refreshWorkflowRunsSoon();
  if (details.has(event.itemId)) void loadWorkflowDetail(event.itemId, true);
}

export function applyWorkflowEngineState(event: WorkflowEngineStateEvent, backend: BackendKey = HOME_BACKEND): void {
  if (!event || typeof event.paused !== 'boolean') return;
  engineRevisions.set(backend, (engineRevisions.get(backend) ?? 0) + 1);
  paused = new Map(paused).set(backend, event.paused);
}

/**
 * Transport-gap recovery for `workflow:engine-state`. The channel is
 * edge-triggered — one frame when the engine pauses or resumes, and nothing
 * afterwards restates it — so a dropped frame leaves the pause banner and every
 * pause-gated affordance describing the opposite of what the engine is doing,
 * indefinitely. `WorkflowGetEngineState` is the authority.
 */
export async function resyncWorkflowEngineState(backend: BackendKey = HOME_BACKEND): Promise<void> {
  const computer = backendById(backend);
  const revision = (engineRevisions.get(backend) ?? 0) + 1;
  engineRevisions.set(backend, revision);
  try {
    const state = await withBackendTarget(backend, () => WorkflowGetEngineState());
    if (state && engineRevisions.get(backend) === revision && backendById(backend) === computer) applyWorkflowEngineState(state, backend);
  } catch (err) {
    console.warn('workflows: engine state resync failed:', err);
  }
}

/** A definition file was written through the app (studio save path). */
export function applyWorkflowDefinitionsChanged(): void {
  if (!overlayLoaded) return;
  void loadProjectScopedData([...catalogs.keys()].length > 0 ? [...catalogs.keys()] : lastProjectIds);
}

onBackendsChanged(() => {
  if (attentionRequested || overlayLoaded) refreshWorkflowRunsSoon();
});

onBackendDetached(({ backendId, projectIds, workflowItemIds }) => {
  const removed = new Set(workflowItemIds);
  runs = runs.filter((item) => !removed.has(item.id));
  const nextCosts = { ...costs };
  for (const id of removed) {
    details.delete(id);
    detailLoads.delete(id);
    livePhases.delete(id);
    receipts.delete(id);
    delete nextCosts[id];
  }
  costs = nextCosts;
  details = new Map(details);
  livePhases = new Map(livePhases);
  receipts = new Map(receipts);
  for (const id of projectIds) { catalogs.delete(id); automations.delete(id); }
  catalogs = new Map(catalogs);
  automations = new Map(automations);
  paused.delete(backendId);
  engineRevisions.delete(backendId);
  paused = new Map(paused);
});

export function resetWorkflowRunsForTest(): void {
  if (refreshTimer !== null) {
    clearTimeout(refreshTimer);
    refreshTimer = null;
  }
  inFlight = null;
  detailLoads.clear();
  listRevision++;
  projectRevision++;
  attentionRequested = false;
  engineRevisions.clear();
  lastProjectIds = [];
  runs = [];
  costs = {};
  catalogs = new Map();
  automations = new Map();
  details = new Map();
  livePhases = new Map();
  receipts = new Map();
  paused = new Map();
  overlayLoaded = false;
  loading = false;
  loadError = null;
}
