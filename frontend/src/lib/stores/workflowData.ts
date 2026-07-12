import type {
  WorkItem,
  WorkflowDefinitionCatalog,
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
  items.sort((left, right) => left.sortPosition - right.sortPosition || left.createdAt - right.createdAt);
  return { items, costs, definitions };
}

export function patchWorkflowItems(items: WorkItem[], event: WorkflowItemStateEvent): WorkItem[] {
  return items.map((item) => item.id === event.itemId
    ? { ...item, state: event.to, reason: event.reason ?? '' } as WorkItem
    : item);
}

export function isWorkflowParked(item: WorkItem): boolean {
  return item.state === 'needs-human'
    || item.state === 'failed'
    || (item.state === 'done' && parseWorkflowDisposition(item.disposition) === null);
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
