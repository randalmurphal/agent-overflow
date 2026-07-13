import type {
  WorkItem,
  WorkflowDefinitionView,
  WorkflowItemStateEvent,
  WorkflowPhaseStateEvent,
} from '../types/workflow';
import { SvelteMap } from 'svelte/reactivity';
import { userFacingError } from '../utils/userFacingError';
import { WorkflowListDefinitions, WorkflowListItems } from './bindings';
import { getProjects } from './projects.svelte';
import { addToast } from './toast.svelte';
import {
  isWorkflowSidebarRun,
  loadWorkflowSidebar,
  patchWorkflowItems,
  workflowSidebarRuns,
} from './workflowData';

export interface WorkflowSidebarPhaseProgress {
  current: number;
  total: number;
  phaseId: string;
}

let items: WorkItem[] = $state([]);
let definitions: WorkflowDefinitionView[] = $state([]);
const phaseEvents = new SvelteMap<string, WorkflowPhaseStateEvent>();
let initialized = $state(false);
let loading = $state(false);
let error: string | null = $state(null);
let initializePromise: Promise<void> | null = null;
let refreshInFlight = false;
let refreshQueued = false;
let refreshAfterInitialize = false;
let pendingItemEvents: WorkflowItemStateEvent[] = [];
let itemEventsDuringFetch: WorkflowItemStateEvent[] | null = null;
const emptyItems: WorkItem[] = [];
const emptyDefinitions: WorkflowDefinitionView[] = [];

let runsByProject = $derived.by(() => {
  const grouped = new Map<string, WorkItem[]>();
  for (const item of items) {
    const projectItems = grouped.get(item.projectId);
    if (projectItems) projectItems.push(item);
    else grouped.set(item.projectId, [item]);
  }
  for (const [projectId, projectItems] of grouped) {
    grouped.set(projectId, workflowSidebarRuns(projectItems, projectId));
  }
  return grouped;
});

let definitionsByProject = $derived.by(() => {
  const grouped = new Map<string, WorkflowDefinitionView[]>();
  for (const definition of definitions) {
    const projectDefinitions = grouped.get(definition.projectId);
    if (projectDefinitions) projectDefinitions.push(definition);
    else grouped.set(definition.projectId, [definition]);
  }
  return grouped;
});

export function getWorkflowSidebarItems(): readonly WorkItem[] { return items; }
export function getWorkflowSidebarDefinitions(): readonly WorkflowDefinitionView[] { return definitions; }
export function isWorkflowSidebarInitialized(): boolean { return initialized; }
export function isWorkflowSidebarLoading(): boolean { return loading; }
export function getWorkflowSidebarError(): string | null { return error; }

export function getProjectWorkflowRuns(projectId: string): WorkItem[] {
  return runsByProject.get(projectId) ?? emptyItems;
}

export function getProjectWorkflowDefinitions(projectId: string): WorkflowDefinitionView[] {
  return definitionsByProject.get(projectId) ?? emptyDefinitions;
}

export function getProjectWorkflowAttentionCount(projectId: string): number {
  return getProjectWorkflowRuns(projectId)
    .filter((item) => item.state === 'needs-human' || item.state === 'failed')
    .length;
}

export function getGlobalWorkflowAttentionCount(): number {
  return items.filter((item) => item.state === 'needs-human' || item.state === 'failed').length;
}

export function getWorkflowSidebarPhaseProgress(item: WorkItem): WorkflowSidebarPhaseProgress | null {
  const event = phaseEvents.get(item.id);
  if (!event) return null;
  const definition = getProjectWorkflowDefinitions(item.projectId)
    .find((entry) => entry.definition.id === item.workflowId)?.definition;
  if (!definition) return null;
  const index = definition.phases.findIndex((phase) => phase.id === event.phaseId);
  if (index < 0 || definition.phaseCount <= 0) return null;
  return { current: index + 1, total: definition.phaseCount, phaseId: event.phaseId };
}

async function fetchSidebarData(): Promise<void> {
  const capturedEvents: WorkflowItemStateEvent[] = [];
  itemEventsDuringFetch = capturedEvents;
  try {
    const loaded = await loadWorkflowSidebar(getProjects().map((entry) => entry.project.id), {
      listItems: async (projectId) => WorkflowListItems(projectId) as unknown as Promise<WorkItem[]>,
      listDefinitions: WorkflowListDefinitions,
    });
    items = loaded.items.filter(isWorkflowSidebarRun);
    for (const event of capturedEvents) items = patchWorkflowItems(items, event);
    items = items.filter(isWorkflowSidebarRun);
    definitions = loaded.definitions;

    const runningIds = new Set(items.filter((item) => item.state === 'running').map((item) => item.id));
    for (const itemId of phaseEvents.keys()) {
      if (!runningIds.has(itemId)) phaseEvents.delete(itemId);
    }
  } finally {
    if (itemEventsDuringFetch === capturedEvents) itemEventsDuringFetch = null;
  }
}

export function initializeWorkflowsSidebar(): Promise<void> {
  if (initialized) return Promise.resolve();
  if (initializePromise) return initializePromise;
  loading = true;
  error = null;
  initializePromise = (async () => {
    try {
      await fetchSidebarData();
      const needsRefresh = pendingItemEvents.length > 0 || refreshAfterInitialize;
      for (const event of pendingItemEvents) items = patchWorkflowItems(items, event);
      items = items.filter(isWorkflowSidebarRun);
      pendingItemEvents = [];
      refreshAfterInitialize = false;
      initialized = true;
      if (needsRefresh) scheduleSidebarRefresh();
    } catch (cause) {
      error = userFacingError(cause, 'Could not load workflow sidebar.');
      addToast('error', error);
    } finally {
      loading = false;
      initializePromise = null;
    }
  })();
  return initializePromise;
}

function scheduleSidebarRefresh(): void {
  if (!initialized) {
    if (loading) refreshAfterInitialize = true;
    else void initializeWorkflowsSidebar();
    return;
  }
  if (refreshInFlight) {
    refreshQueued = true;
    return;
  }
  refreshInFlight = true;
  void (async () => {
    do {
      refreshQueued = false;
      try {
        await fetchSidebarData();
        error = null;
      } catch (cause) {
        error = userFacingError(cause, 'Could not refresh workflow sidebar.');
        addToast('error', error);
      }
    } while (refreshQueued);
  })().finally(() => {
    refreshInFlight = false;
    if (refreshQueued) scheduleSidebarRefresh();
  });
}

export function refreshWorkflowsSidebar(): void {
  scheduleSidebarRefresh();
}

export function applyWorkflowSidebarItemState(event: WorkflowItemStateEvent): void {
  if (loading && !initialized) pendingItemEvents.push(event);
  itemEventsDuringFetch?.push(event);
  items = patchWorkflowItems(items, event).filter(isWorkflowSidebarRun);
  if (event.to !== 'running') phaseEvents.delete(event.itemId);
  // The compact event omits timestamps, disposition, worktree, and branch
  // fields that may change on every transition. Patch status immediately,
  // then coalesce an authoritative summary refresh for the remaining fields.
  scheduleSidebarRefresh();
}

export function applyWorkflowSidebarPhaseState(event: WorkflowPhaseStateEvent): void {
  phaseEvents.set(event.itemId, event);
}

export function applyWorkflowSidebarQueueState(): void {
  // Queue events follow reorder and scheduler changes whose updated summary
  // fields are not carried in the compact event payload.
  refreshWorkflowsSidebar();
}

export function resetWorkflowsSidebarForTest(): void {
  items = [];
  definitions = [];
  phaseEvents.clear();
  initialized = false;
  loading = false;
  error = null;
  initializePromise = null;
  refreshInFlight = false;
  refreshQueued = false;
  refreshAfterInitialize = false;
  pendingItemEvents = [];
  itemEventsDuringFetch = null;
}
