import type {
  WorkItem,
  WorkflowDefinitionCatalog,
  WorkflowDefinitionListing,
  WorkflowDefinitionView,
  WorkflowItemStateEvent,
  WorkflowResolvedReceipt,
} from '../types/workflow';
import { parseWorkflowDisposition } from '../types/workflow';

export interface WorkflowProjectLoad {
  projectId: string;
  items: WorkItem[];
  costs: Record<string, number | undefined>;
  catalog: WorkflowDefinitionCatalog;
}

export interface WorkflowMergedLoad {
  items: WorkItem[];
  costs: Record<string, number>;
  definitions: WorkflowDefinitionView[];
}

export interface WorkflowDataBindings {
  listItems(projectId: string): Promise<WorkItem[]>;
  listItemCosts(projectId: string): Promise<Record<string, number | undefined>>;
  listDefinitions(projectId: string): Promise<WorkflowDefinitionCatalog>;
}

export function workflowDefinitionMeta(definition: WorkflowDefinitionListing): string {
  const phases = `${definition.phaseCount} ${definition.phaseCount === 1 ? 'phase' : 'phases'}`;
  const humanGateCount = definition.humanGateCount ?? 0;
  if (humanGateCount <= 0) return phases;
  const gates = `${humanGateCount} human ${humanGateCount === 1 ? 'gate' : 'gates'}`;
  return `${phases} · ${gates}`;
}

// Bare relative age for workflow meta copy ("6m", "7h", "3d"). Callers append
// "ago" only where the spec copy carries it (`spawned 6m ago`, `finished 2h
// ago`) and use the bare form elsewhere (`queued 3h`, `parked 7h`); "<1m"
// composes in both forms, unlike a "just now" phrasing.
export function workflowAge(timestampMs: number): string {
  const minutes = Math.max(0, Math.floor((Date.now() - timestampMs) / 60_000));
  if (minutes < 1) return '<1m';
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return `${hours}h`;
  return `${Math.floor(hours / 24)}d`;
}

export async function loadWorkflowProject(
  projectId: string,
  bindings: WorkflowDataBindings,
): Promise<WorkflowProjectLoad> {
  const [items, costs, catalog] = await Promise.all([
    bindings.listItems(projectId),
    bindings.listItemCosts(projectId),
    bindings.listDefinitions(projectId),
  ]);
  return { projectId, items, costs, catalog };
}

export async function loadWorkflowSidebar(
  projectIds: readonly string[],
  bindings: Pick<WorkflowDataBindings, 'listItems' | 'listDefinitions'>,
): Promise<WorkflowMergedLoad> {
  const items = await bindings.listItems('');
  const itemsByProject = new Map<string, WorkItem[]>();
  for (const item of items) {
    if (!item.projectId || !isWorkflowSidebarRun(item)) continue;
    const projectItems = itemsByProject.get(item.projectId);
    if (projectItems) projectItems.push(item);
    else itemsByProject.set(item.projectId, [item]);
  }
  const definitionProjectIds = new Set(projectIds.filter(Boolean));
  for (const projectId of itemsByProject.keys()) definitionProjectIds.add(projectId);
  const loads = await Promise.all([...definitionProjectIds].map(async (projectId): Promise<WorkflowProjectLoad> => ({
    projectId,
    items: itemsByProject.get(projectId) ?? [],
    costs: {},
    catalog: await bindings.listDefinitions(projectId),
  })));
  return mergeWorkflowProjectLoads(loads);
}

export function mergeWorkflowProjectLoads(loads: WorkflowProjectLoad[]): WorkflowMergedLoad {
  const items = loads.flatMap((load) => load.items);
  const costs: Record<string, number> = {};
  const definitions: WorkflowDefinitionView[] = [];
  for (const load of loads) {
    for (const [itemId, cost] of Object.entries(load.costs)) {
      if (typeof cost === 'number' && Number.isFinite(cost)) costs[itemId] = cost;
    }
    for (const definition of load.catalog.workflows) {
      definitions.push({ projectId: load.projectId, catalog: load.catalog, definition });
    }
  }
  items.sort((left, right) => left.createdAt - right.createdAt || left.id.localeCompare(right.id));
  return { items, costs, definitions };
}

export function patchWorkflowItems(items: WorkItem[], event: WorkflowItemStateEvent): WorkItem[] {
  return items.map((item) => item.id === event.itemId
    ? { ...item, state: event.to, reason: event.reason ?? '' } as WorkItem
    : item);
}

// A disposition receipt is the resolution record for any state — the
// frontend mirror of the store's unresolved predicate. Cancellation
// resolves without a receipt.
export function isWorkflowResolved(item: WorkItem): boolean {
  return item.state === 'cancelled' || parseWorkflowDisposition(item.disposition) !== null;
}

export function isWorkflowParked(item: WorkItem): boolean {
  if (isWorkflowResolved(item)) return false;
  return item.state === 'needs-human' || item.state === 'failed' || item.state === 'done';
}

export function isWorkflowSidebarRun(item: WorkItem): boolean {
  return !isWorkflowResolved(item);
}

function sidebarStateRank(item: WorkItem): number {
  if (item.state === 'needs-human') return 0;
  if (item.state === 'failed') return 1;
  if (item.state === 'running') return 2;
  return 3;
}

export function workflowSidebarRuns(items: WorkItem[], projectId: string): WorkItem[] {
  return items
    .filter((item) => item.projectId === projectId && isWorkflowSidebarRun(item))
    .sort((left, right) => {
      const rank = sidebarStateRank(left) - sidebarStateRank(right);
      if (rank !== 0) return rank;
      const leftAt = left.endedAt || left.startedAt || left.createdAt;
      const rightAt = right.endedAt || right.startedAt || right.createdAt;
      return leftAt - rightAt || left.id.localeCompare(right.id);
    });
}

export function workflowSweepItems(
  items: WorkItem[],
  receipts: ReadonlyMap<string, WorkflowResolvedReceipt>,
): WorkItem[] {
  return items
    .filter((item) => isWorkflowParked(item) || receipts.has(item.id))
    .sort((left, right) => (left.endedAt || left.createdAt) - (right.endedAt || right.createdAt));
}

export function nextWorkflowSweepIndex(
  items: WorkItem[],
  currentIndex: number,
  direction: -1 | 1,
  receipts: ReadonlyMap<string, WorkflowResolvedReceipt>,
  skipResolved: boolean,
): number {
  if (items.length === 0) return -1;
  for (let offset = 1; offset <= items.length; offset += 1) {
    const index = (currentIndex + direction * offset + items.length) % items.length;
    if (!skipResolved || !receipts.has(items[index].id)) return index;
  }
  return -1;
}
