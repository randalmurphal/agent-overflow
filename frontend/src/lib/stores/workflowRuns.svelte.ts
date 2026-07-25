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
// Run DETAIL (phases, units, children, artifacts) is loaded per run on demand
// and evicted when the detail level leaves that tree, so frontend memory stays
// bounded by what is on screen (root CLAUDE.md principle 4).

import type {
  WorkItem,
  WorkflowAutomationView,
  WorkflowDefinitionCatalog,
  WorkflowEngineStateEvent,
  WorkflowItemDetail,
  WorkflowItemStateEvent,
  WorkflowPhaseStateEvent,
  WorkflowResolvedReceipt,
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
import { patchWorkflowItems, workflowAttentionCount } from './workflowData';

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
let paused = $state(false);
let overlayLoaded = $state(false);
let loading = $state(false);
let loadError = $state<string | null>(null);

let lastProjectIds: string[] = [];
let refreshTimer: ReturnType<typeof setTimeout> | null = null;
let inFlight: Promise<void> | null = null;
const detailLoads = new Map<string, Promise<WorkflowItemDetail | null>>();

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

export function isWorkflowEnginePaused(): boolean {
  return paused;
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
  try {
    const items = (await WorkflowListUnresolvedItems('')) as WorkItem[] | null;
    if (Array.isArray(items)) runs = items;
  } catch (err) {
    console.warn('workflows: attention hydration failed:', err);
  }
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
      const [items, engineState] = await Promise.all([
        WorkflowListItems('') as Promise<WorkItem[] | null>,
        WorkflowGetEngineState().catch(() => null),
      ]);
      runs = Array.isArray(items) ? items : [];
      if (engineState) paused = engineState.paused === true;
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
  const loaded = await Promise.all(projectIds.map(async (projectId) => ({
    projectId,
    catalog: await WorkflowListDefinitions(projectId).catch((err) => {
      console.warn(`workflows: definitions for ${projectId} failed:`, err);
      return null;
    }),
    automations: await WorkflowListAutomations(projectId).catch((err) => {
      console.warn(`workflows: automations for ${projectId} failed:`, err);
      return null;
    }),
    costs: await WorkflowListItemCosts(projectId).catch((err) => {
      console.warn(`workflows: costs for ${projectId} failed:`, err);
      return null;
    }),
  })));
  const nextCatalogs = new Map<string, WorkflowDefinitionCatalog>();
  const nextAutomations = new Map<string, WorkflowAutomationView[]>();
  const nextCosts: Record<string, number> = {};
  for (const entry of loaded) {
    if (entry.catalog) nextCatalogs.set(entry.projectId, entry.catalog);
    if (entry.automations) nextAutomations.set(entry.projectId, entry.automations);
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
    if (!overlayLoaded) return;
    void (async () => {
      try {
        const items = (await WorkflowListItems('')) as WorkItem[] | null;
        if (Array.isArray(items)) runs = items;
      } catch (err) {
        console.warn('workflows: run refresh failed:', err);
      }
    })();
  }, REFRESH_DEBOUNCE_MS);
}

/**
 * Load one run's detail. Single-flight per run so a tree that expands several
 * child rows at once does not fan out duplicate reads.
 */
export async function loadWorkflowDetail(itemId: string, force = false): Promise<WorkflowItemDetail | null> {
  if (!itemId) return null;
  if (!force) {
    const cached = details.get(itemId);
    if (cached) return cached;
    const pending = detailLoads.get(itemId);
    if (pending) return pending;
  }
  const load = (async () => {
    try {
      const detail = await WorkflowGetItem(itemId);
      details = new Map(details).set(itemId, detail);
      return detail;
    } catch (err) {
      addToast('error', userFacingError(err, 'Could not load this run.'));
      return null;
    } finally {
      detailLoads.delete(itemId);
    }
  })();
  detailLoads.set(itemId, load);
  return load;
}

/**
 * Drop every cached detail except the given roots and whatever they call.
 * `keep` is the root plus its already-loaded descendants, walked here so a
 * caller only has to name the run it is looking at.
 */
export function retainWorkflowDetails(rootItemId: string | null): void {
  if (details.size === 0) return;
  if (!rootItemId) {
    details = new Map();
    return;
  }
  const keep = new Set<string>();
  const walk = (itemId: string): void => {
    if (keep.has(itemId)) return;
    keep.add(itemId);
    for (const child of details.get(itemId)?.children ?? []) walk(child.itemId);
  };
  walk(rootItemId);
  // The decision is "does the cache hold anything outside `keep`", never a size
  // comparison: `keep` names the root whether or not its detail has landed yet,
  // so a size check would keep rewriting an already-minimal cache — and this
  // runs inside an $effect that reads `details`, which turns a redundant write
  // into an infinite effect loop.
  let drops = false;
  for (const itemId of details.keys()) {
    if (!keep.has(itemId)) {
      drops = true;
      break;
    }
  }
  if (!drops) return;
  const next = new Map<string, WorkflowItemDetail>();
  for (const [itemId, detail] of details) {
    if (keep.has(itemId)) next.set(itemId, detail);
  }
  details = next;
}

export function applyWorkflowItemState(event: WorkflowItemStateEvent): void {
  if (!event || typeof event.itemId !== 'string' || event.itemId === '') return;
  const known = runs.some((item) => item.id === event.itemId);
  runs = known ? patchWorkflowItems(runs, event) : runs;
  // A transition on a run the cache has never seen is a fresh start (or a
  // called run appearing under its parent); only the backend knows its row.
  if (!known) refreshWorkflowRunsSoon();
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

export function applyWorkflowEngineState(event: WorkflowEngineStateEvent): void {
  if (!event || typeof event.paused !== 'boolean') return;
  paused = event.paused;
}

/** A definition file was written through the app (studio save path). */
export function applyWorkflowDefinitionsChanged(): void {
  if (!overlayLoaded) return;
  void loadProjectScopedData([...catalogs.keys()].length > 0 ? [...catalogs.keys()] : lastProjectIds);
}

export function resetWorkflowRunsForTest(): void {
  if (refreshTimer !== null) {
    clearTimeout(refreshTimer);
    refreshTimer = null;
  }
  inFlight = null;
  detailLoads.clear();
  lastProjectIds = [];
  runs = [];
  costs = {};
  catalogs = new Map();
  automations = new Map();
  details = new Map();
  livePhases = new Map();
  receipts = new Map();
  paused = false;
  overlayLoaded = false;
  loading = false;
  loadError = null;
}
