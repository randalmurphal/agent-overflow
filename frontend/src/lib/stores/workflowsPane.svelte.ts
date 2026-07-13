import type { Thread } from '../types/models';
import type {
  WorkItem,
  WorkflowDefinitionView,
  WorkflowItemDetail,
  WorkflowItemStateEvent,
  WorkflowPaneLevel,
  WorkflowPhaseStateEvent,
  WorkflowQueueStateEvent,
  WorkflowResolvedReceipt,
  WorkflowsPaneTarget,
} from '../types/workflow';
import { parsePatchFiles, type PatchFile } from '../utils/patchFiles';
import { userFacingError } from '../utils/userFacingError';
import {
  GetBranchBaseDiff,
  WorkflowGetItem,
  WorkflowListDefinitions,
  WorkflowListItemCosts,
  WorkflowListItems,
} from './bindings';
import { getProjects } from './projects.svelte';
import {
  addPaneLayoutItem,
  averagePaneWidthPx,
  getPaneLayoutItems,
} from './paneLayout.svelte';
import { focusPane, revealPane } from './panes.svelte';
import { addToast } from './toast.svelte';
import { getSettings } from './settings.svelte';
import {
  mergeWorkflowProjectLoads,
  loadWorkflowProject,
  nextWorkflowSweepIndex,
  patchWorkflowItems,
  workflowSweepItems,
  type WorkflowProjectLoad,
} from './workflowData';
import { closeCompanionsForSource } from './companionPanes.svelte';

export const WORKFLOWS_PANE_ID = 'workflows';

export interface WorkflowIntakePrefill {
  projectId?: string;
  goal?: string;
  workflowId?: string;
  baseBranch?: string;
  seeds?: Record<string, unknown>;
  stepMode?: boolean;
}

let stack: WorkflowPaneLevel[] = $state([{ kind: 'overview' }]);
let projectFilter: string | null = $state(null);
let items: WorkItem[] = $state([]);
let costs: Record<string, number> = $state({});
let definitions: WorkflowDefinitionView[] = $state([]);
let detail: WorkflowItemDetail | null = $state(null);
let diffFiles: PatchFile[] = $state([]);
let loading = $state(false);
let error: string | null = $state(null);
let queueState: WorkflowQueueStateEvent | null = $state(null);
let receipts: Map<string, WorkflowResolvedReceipt> = $state(new Map());
let sweepIndex = $state(-1);
let intakeOpen = $state(false);
let intakePrefill: WorkflowIntakePrefill | null = $state(null);
let armedAction: string | null = $state(null);
let requestVersion = 0;
let autoAdvanceTimer: ReturnType<typeof setTimeout> | null = null;
let paneActive = $state(false);
let diffLoading = $state(false);
let diffError: string | null = $state(null);
let diffRequestedItemId: string | null = null;
let runRefreshInFlight = false;
let runRefreshQueued = false;
let refreshGeneration = 0;
let overviewRefreshInFlight = false;
let overviewRefreshQueued = false;
const overviewRefreshProjectIds = new Set<string>();
let overviewLoadCount = 0;
let overviewDetailRefreshItemId: string | null = null;
const overviewItemEventCaptures = new Set<WorkflowItemStateEvent[]>();
let persistenceHandler: (() => void) | null = null;

export function getWorkflowStack(): readonly WorkflowPaneLevel[] { return stack; }
export function getWorkflowCurrentLevel(): WorkflowPaneLevel { return stack[stack.length - 1]; }
export function getWorkflowProjectFilter(): string | null { return projectFilter; }
export function getWorkflowItems(): readonly WorkItem[] { return items; }
export function getWorkflowCosts(): Readonly<Record<string, number>> { return costs; }
export function getWorkflowDefinitions(): readonly WorkflowDefinitionView[] { return definitions; }
export function getWorkflowDetail(): WorkflowItemDetail | null { return detail; }
export function getWorkflowDiffFiles(): readonly PatchFile[] { return diffFiles; }
export function isWorkflowLoading(): boolean { return loading; }
export function getWorkflowError(): string | null { return error; }
export function getWorkflowQueueState(): WorkflowQueueStateEvent {
  const settings = getSettings();
  return queueState ?? {
    active: settings.workflowQueueActive,
    globalConcurrency: settings.workflowConcurrency,
  };
}
export function isWorkflowDiffLoading(): boolean { return diffLoading; }
export function getWorkflowDiffError(): string | null { return diffError; }
export function isWorkflowDiffLoaded(): boolean {
  return detail !== null && diffRequestedItemId === detail.item.id && !diffLoading;
}
export function getWorkflowReceipts(): ReadonlyMap<string, WorkflowResolvedReceipt> { return receipts; }
export function isWorkflowIntakeOpen(): boolean { return intakeOpen; }
export function getWorkflowIntakePrefill(): WorkflowIntakePrefill | null { return intakePrefill; }
export function getWorkflowArmedAction(): string | null { return armedAction; }
export function isWorkflowsPaneActive(): boolean { return paneActive; }

export function setWorkflowsPanePersistenceHandler(handler: (() => void) | null): void {
  persistenceHandler = handler;
}

function requestWorkflowsPanePersistence(): void {
  persistenceHandler?.();
}

export function activateWorkflowsPane(): boolean {
  const changed = !paneActive;
  paneActive = true;
  return changed;
}

export function deactivateWorkflowsPane(): void {
  if (!paneActive) return;
  paneActive = false;
  refreshGeneration += 1;
  closeCompanionsForSource(WORKFLOWS_PANE_ID);
  requestVersion += 1;
  runRefreshInFlight = false;
  runRefreshQueued = false;
  overviewRefreshInFlight = false;
  overviewRefreshQueued = false;
  overviewRefreshProjectIds.clear();
  overviewLoadCount = 0;
  overviewDetailRefreshItemId = null;
  overviewItemEventCaptures.clear();
  if (autoAdvanceTimer) clearTimeout(autoAdvanceTimer);
  autoAdvanceTimer = null;
  items = [];
  costs = {};
  definitions = [];
  detail = null;
  diffFiles = [];
  diffLoading = false;
  diffError = null;
  diffRequestedItemId = null;
  receipts = new Map();
  sweepIndex = -1;
  intakeOpen = false;
  intakePrefill = null;
  armedAction = null;
  loading = false;
  error = null;
}

function resetLevelTransientState(): void {
  closeCompanionsForSource(WORKFLOWS_PANE_ID);
  armedAction = null;
  diffFiles = [];
  diffLoading = false;
  diffError = null;
  diffRequestedItemId = null;
  if (autoAdvanceTimer) clearTimeout(autoAdvanceTimer);
  autoAdvanceTimer = null;
}

export function setWorkflowProjectFilter(projectId: string | null): void {
  projectFilter = projectId || null;
  receipts = new Map();
  sweepIndex = -1;
  requestWorkflowsPanePersistence();
  void loadWorkflowOverview();
}

export function setWorkflowArmedAction(action: string | null): void { armedAction = action; }
export function openWorkflowIntake(prefill: WorkflowIntakePrefill | null = null): void {
  intakePrefill = prefill;
  intakeOpen = true;
}
export function closeWorkflowIntake(): void { intakeOpen = false; intakePrefill = null; }

export function pushWorkflowLevel(level: WorkflowPaneLevel): void {
  resetLevelTransientState();
  stack = [...stack, level];
  requestWorkflowsPanePersistence();
  void loadWorkflowCurrentLevel();
}

export function popWorkflowLevel(): boolean {
  if (stack.length <= 1) return false;
  resetLevelTransientState();
  stack = stack.slice(0, -1);
  if (getWorkflowCurrentLevel().kind !== 'run') detail = null;
  requestWorkflowsPanePersistence();
  void loadWorkflowCurrentLevel();
  return true;
}

export function popWorkflowTo(index: number): void {
  if (index < 0 || index >= stack.length - 1) return;
  resetLevelTransientState();
  stack = stack.slice(0, index + 1);
  if (getWorkflowCurrentLevel().kind !== 'run') detail = null;
  requestWorkflowsPanePersistence();
  void loadWorkflowCurrentLevel();
}

export function consumeWorkflowEscape(): boolean {
  if (armedAction) {
    armedAction = null;
    return true;
  }
  if (intakeOpen) {
    closeWorkflowIntake();
    return true;
  }
  return popWorkflowLevel();
}

function targetStack(target: WorkflowsPaneTarget): WorkflowPaneLevel[] {
  if (target.kind === 'overview') return [{ kind: 'overview' }];
  const workflowLabel = target.label ?? ('workflowLabel' in target ? target.workflowLabel : undefined) ?? target.workflowId;
  const workflow: WorkflowPaneLevel = {
    kind: 'workflow', projectId: target.projectId, workflowId: target.workflowId, label: workflowLabel,
  };
  if (target.kind === 'workflow') return [{ kind: 'overview' }, workflow];
  return [{ kind: 'overview' }, workflow, {
    kind: 'run', projectId: target.projectId, workflowId: target.workflowId,
    workflowLabel, itemId: target.itemId, label: target.label ?? target.itemId,
    sweep: target.kind === 'sweep-at-run',
  }];
}

function retargetWorkflowsPane(target: WorkflowsPaneTarget): Promise<void> {
  resetLevelTransientState();
  stack = targetStack(target);
  if (target.kind !== 'sweep-at-run') {
    receipts = new Map();
    sweepIndex = -1;
  }
  requestWorkflowsPanePersistence();
  return loadWorkflowCurrentLevel();
}

export function openWorkflowsPane(target: WorkflowsPaneTarget = { kind: 'overview' }): Promise<void> {
  activateWorkflowsPane();
  if (!getPaneLayoutItems().some((item) => item.kind === 'workflows')) {
    addPaneLayoutItem({
      id: WORKFLOWS_PANE_ID,
      paneId: WORKFLOWS_PANE_ID,
      kind: 'workflows',
      widthPx: averagePaneWidthPx(),
    });
  }
  const loaded = retargetWorkflowsPane(target);
  focusPane(WORKFLOWS_PANE_ID);
  revealPane(WORKFLOWS_PANE_ID);
  return loaded;
}

function selectedProjectIds(): string[] {
  const known = getProjects().map((entry) => entry.project.id);
  const selected = new Set(projectFilter ? [projectFilter] : known);
  for (const level of stack) {
    if (level.kind === 'workflow' || level.kind === 'run') selected.add(level.projectId);
  }
  return [...selected];
}

export async function loadWorkflowOverview(): Promise<void> {
  if (!paneActive) return;
  const generation = refreshGeneration;
  const version = ++requestVersion;
  const capturedEvents: WorkflowItemStateEvent[] = [];
  overviewItemEventCaptures.add(capturedEvents);
  overviewLoadCount += 1;
  loading = true;
  error = null;
  try {
    const loads = await Promise.all(selectedProjectIds().map((projectId): Promise<WorkflowProjectLoad> =>
      loadWorkflowProject(projectId, {
        listItems: async (id) => WorkflowListItems(id) as unknown as Promise<WorkItem[]>,
        listItemCosts: WorkflowListItemCosts,
        listDefinitions: WorkflowListDefinitions,
      }),
    ));
    if (version !== requestVersion) return;
    const merged = mergeWorkflowProjectLoads(loads);
    items = merged.items;
    for (const event of capturedEvents) items = patchWorkflowItems(items, event);
    costs = merged.costs;
    definitions = merged.definitions;
  } catch (cause) {
    if (version !== requestVersion) return;
    error = userFacingError(cause, 'Could not load workflows.');
    addToast('error', 'Could not load workflows');
  } finally {
    overviewItemEventCaptures.delete(capturedEvents);
    if (generation === refreshGeneration) {
      overviewLoadCount -= 1;
      if (version === requestVersion) loading = false;
      if (overviewLoadCount === 0 && overviewRefreshQueued && !overviewRefreshInFlight && paneActive) {
        scheduleOverviewRefresh();
      }
    }
  }
}

async function refreshWorkflowProjects(projectIds: readonly string[]): Promise<void> {
  const selected = new Set(selectedProjectIds());
  const affected = [...new Set(projectIds.filter((projectId) => selected.has(projectId)))];
  if (affected.length === 0) return;
  const generation = refreshGeneration;
  const version = ++requestVersion;
  const capturedEvents: WorkflowItemStateEvent[] = [];
  overviewItemEventCaptures.add(capturedEvents);
  overviewLoadCount += 1;
  loading = true;
  error = null;
  try {
    const loads = await Promise.all(affected.map(async (projectId) => {
      const [projectItems, projectCosts] = await Promise.all([
        WorkflowListItems(projectId) as unknown as Promise<WorkItem[]>,
        WorkflowListItemCosts(projectId),
      ]);
      return { projectId, items: projectItems, costs: projectCosts };
    }));
    if (version !== requestVersion) return;
    const affectedSet = new Set(affected);
    const replacedItemIds = new Set(
      items.filter((item) => affectedSet.has(item.projectId)).map((item) => item.id),
    );
    items = [
      ...items.filter((item) => !affectedSet.has(item.projectId)),
      ...loads.flatMap((load) => load.items),
    ].sort((left, right) => left.sortPosition - right.sortPosition || left.createdAt - right.createdAt);
    for (const event of capturedEvents) items = patchWorkflowItems(items, event);
    const nextCosts = { ...costs };
    for (const itemId of replacedItemIds) delete nextCosts[itemId];
    for (const load of loads) {
      for (const [itemId, cost] of Object.entries(load.costs)) {
        if (typeof cost === 'number' && Number.isFinite(cost)) nextCosts[itemId] = cost;
      }
    }
    costs = nextCosts;
  } catch (cause) {
    if (version !== requestVersion) return;
    error = userFacingError(cause, 'Could not refresh workflows.');
    addToast('error', 'Could not refresh workflows');
  } finally {
    overviewItemEventCaptures.delete(capturedEvents);
    if (generation === refreshGeneration) {
      overviewLoadCount -= 1;
      if (version === requestVersion) loading = false;
    }
  }
}

function scheduleOverviewRefresh(projectId: string | null = null, detailItemId: string | null = null): void {
  if (!paneActive) return;
  if (projectId) overviewRefreshProjectIds.add(projectId);
  if (detailItemId && detail?.item.id === detailItemId) overviewDetailRefreshItemId = detailItemId;
  if (overviewRefreshInFlight || overviewLoadCount > 0) {
    overviewRefreshQueued = true;
    return;
  }
  overviewRefreshInFlight = true;
  const generation = refreshGeneration;
  void (async () => {
    do {
      overviewRefreshQueued = false;
      const projectIds = [...overviewRefreshProjectIds];
      overviewRefreshProjectIds.clear();
      const itemId = overviewDetailRefreshItemId;
      overviewDetailRefreshItemId = null;
      await refreshWorkflowProjects(projectIds);
      if (generation !== refreshGeneration) return;
      if (itemId && paneActive && detail?.item.id === itemId) {
        await loadRun(itemId, false);
        if (diffRequestedItemId === itemId) await loadWorkflowDiff();
      }
    } while ((overviewRefreshQueued || overviewRefreshProjectIds.size > 0) && paneActive && generation === refreshGeneration);
  })().finally(() => {
    if (generation !== refreshGeneration) return;
    overviewRefreshInFlight = false;
    if ((overviewRefreshQueued || overviewRefreshProjectIds.size > 0) && paneActive) scheduleOverviewRefresh();
    else if (runRefreshQueued && paneActive && detail) scheduleRunRefresh(detail.item.id);
  });
}

async function loadRun(itemId: string, clearCurrent = true): Promise<void> {
  if (!paneActive) return;
  const version = ++requestVersion;
  loading = true;
  error = null;
  if (clearCurrent) {
    detail = null;
    diffFiles = [];
    diffError = null;
    diffRequestedItemId = null;
  }
  try {
    const loaded = await WorkflowGetItem(itemId) as unknown as WorkflowItemDetail;
    if (version !== requestVersion) return;
    detail = loaded;
    const currentLevel = getWorkflowCurrentLevel();
    if (currentLevel.kind === 'run' && currentLevel.sweep) {
      beginWorkflowSweep(itemId);
      if (getWorkflowSweep().items.length === 0) {
        stack = [...stack.slice(0, 1), { kind: 'all-clear' }];
        requestWorkflowsPanePersistence();
      }
    }
  } catch (cause) {
    if (version !== requestVersion) return;
    error = userFacingError(cause, 'Could not load this run.');
    addToast('error', 'Could not load this run');
  } finally {
    if (version === requestVersion) loading = false;
  }
}

export async function loadWorkflowDiff(): Promise<void> {
  const loaded = detail;
  if (!paneActive || !loaded || (loaded.item.reason !== 'gate' && loaded.item.state !== 'done')) return;
  const newestThread = [...loaded.phases].reverse().find((phase) => Boolean(phase.threadId));
  if (!newestThread?.threadId || !loaded.item.baseBranch) return;
  const version = requestVersion;
  const itemId = loaded.item.id;
  diffRequestedItemId = itemId;
  diffLoading = true;
  diffError = null;
  try {
    const patch = await GetBranchBaseDiff(newestThread.threadId, loaded.item.baseBranch) as string;
    if (paneActive && version === requestVersion && detail?.item.id === itemId) {
      diffFiles = parsePatchFiles(patch ?? '');
    }
  } catch (cause) {
    if (paneActive && version === requestVersion && detail?.item.id === itemId) {
      diffError = userFacingError(cause, 'Could not load workflow changes.');
      addToast('error', 'Could not load workflow changes');
    }
  } finally {
    if (paneActive && version === requestVersion && detail?.item.id === itemId) diffLoading = false;
  }
}

function scheduleRunRefresh(itemId: string): void {
  if (!paneActive || detail?.item.id !== itemId) return;
  if (overviewRefreshInFlight || overviewLoadCount > 0) {
    runRefreshQueued = true;
    return;
  }
  if (runRefreshInFlight) {
    runRefreshQueued = true;
    return;
  }
  runRefreshInFlight = true;
  const generation = refreshGeneration;
  void (async () => {
    do {
      runRefreshQueued = false;
      await loadRun(itemId, false);
      if (generation !== refreshGeneration) return;
      if (diffRequestedItemId === itemId) await loadWorkflowDiff();
    } while (runRefreshQueued && paneActive && detail?.item.id === itemId && generation === refreshGeneration);
  })().finally(() => {
    if (generation !== refreshGeneration) return;
    runRefreshInFlight = false;
    if (runRefreshQueued && paneActive && detail?.item.id === itemId) scheduleRunRefresh(itemId);
  });
}

export async function loadWorkflowCurrentLevel(): Promise<void> {
  if (!paneActive) return;
  const level = getWorkflowCurrentLevel();
  if (level.kind === 'overview' || level.kind === 'workflow') {
    await loadWorkflowOverview();
    if (getWorkflowCurrentLevel() !== level) return;
  } else if (level.kind === 'run') {
    if (items.length === 0) {
      await loadWorkflowOverview();
      if (getWorkflowCurrentLevel() !== level) return;
    }
    await loadRun(level.itemId);
    if (getWorkflowCurrentLevel() !== level) return;
  }
}

export function applyWorkflowItemState(event: WorkflowItemStateEvent): void {
  if (!paneActive) return;
  for (const capture of overviewItemEventCaptures) capture.push(event);
  items = patchWorkflowItems(items, event);
  if (detail?.item.id === event.itemId) {
    detail = { ...detail, item: { ...detail.item, state: event.to, reason: event.reason ?? '' } };
  }
  scheduleOverviewRefresh(event.projectId, event.itemId);
}

export async function reloadWorkflowsPaneAfterGap(): Promise<void> {
  if (!paneActive) return;
  await loadWorkflowOverview();
  const level = getWorkflowCurrentLevel();
  if (level.kind === 'run') await loadRun(level.itemId, false);
}

export function applyWorkflowPhaseState(event: WorkflowPhaseStateEvent): void {
  if (!paneActive) return;
  const level = getWorkflowCurrentLevel();
  if (level.kind === 'run' && level.itemId === event.itemId) scheduleRunRefresh(event.itemId);
}

export function applyWorkflowQueueState(event: WorkflowQueueStateEvent): void {
  queueState = event;
}

export function reconcileWorkflowQueueOrder(projectId: string, orderedIds: string[]): void {
  const positions = new Map(orderedIds.map((id, index) => [id, index]));
  items = items.map((item) => item.projectId === projectId && positions.has(item.id)
    ? { ...item, sortPosition: positions.get(item.id) ?? item.sortPosition } as WorkItem
    : item).sort((left, right) => left.sortPosition - right.sortPosition);
}

export function beginWorkflowSweep(itemId: string): void {
  const sweep = workflowSweepItems(items, receipts);
  sweepIndex = sweep.findIndex((item) => item.id === itemId);
  if (sweepIndex < 0 && sweep.length > 0) sweepIndex = 0;
}

export function getWorkflowSweep(): { items: WorkItem[]; index: number } {
  const sweep = workflowSweepItems(items, receipts);
  const currentLevel = getWorkflowCurrentLevel();
  const currentId = currentLevel.kind === 'run' ? currentLevel.itemId : '';
  const index = sweep.findIndex((item) => item.id === currentId);
  const fallback = sweep.length === 0 ? -1 : Math.min(Math.max(sweepIndex, 0), sweep.length - 1);
  return { items: sweep, index: index >= 0 ? index : fallback };
}

export function stepWorkflowSweep(direction: -1 | 1, skipResolved = false): boolean {
  if (autoAdvanceTimer) clearTimeout(autoAdvanceTimer);
  autoAdvanceTimer = null;
  closeCompanionsForSource(WORKFLOWS_PANE_ID);
  const sweep = workflowSweepItems(items, receipts);
  const current = getWorkflowSweep().index;
  const next = nextWorkflowSweepIndex(sweep, current < 0 ? 0 : current, direction, receipts, skipResolved);
  if (next < 0) {
    stack = [...stack.slice(0, 1), { kind: 'all-clear' }];
    requestWorkflowsPanePersistence();
    return false;
  }
  const item = sweep[next];
  const definition = definitions.find((entry) => entry.projectId === item.projectId && entry.definition.id === item.workflowId);
  stack = targetStack({
    kind: 'sweep-at-run', projectId: item.projectId, workflowId: item.workflowId, itemId: item.id,
    workflowLabel: definition?.definition.name ?? item.workflowId, label: item.goal,
  });
  sweepIndex = next;
  requestWorkflowsPanePersistence();
  void loadRun(item.id);
  return true;
}

export function recordWorkflowReceipt(receipt: WorkflowResolvedReceipt, autoAdvance = true): void {
  receipts = new Map(receipts).set(receipt.itemId, receipt);
  const currentLevel = getWorkflowCurrentLevel();
  if (!autoAdvance || currentLevel.kind !== 'run' || !currentLevel.sweep) return;
  if (autoAdvanceTimer) clearTimeout(autoAdvanceTimer);
  autoAdvanceTimer = setTimeout(() => {
    autoAdvanceTimer = null;
    stepWorkflowSweep(1, true);
  }, 650);
}

export function workflowAllClearSummary(): { count: number; costUsd: number; byKind: Record<string, number> } {
  const byKind: Record<string, number> = {};
  let costUsd = 0;
  for (const receipt of receipts.values()) {
    byKind[receipt.kind] = (byKind[receipt.kind] ?? 0) + 1;
    costUsd += receipt.costUsd;
  }
  return { count: receipts.size, costUsd, byKind };
}

export interface PersistedWorkflowsPaneState {
  stack: WorkflowPaneLevel[];
  projectFilter: string | null;
}

export function parsePersistedWorkflowsPaneState(value: unknown): PersistedWorkflowsPaneState | null {
  if (!value || typeof value !== 'object') return null;
  const record = value as Record<string, unknown>;
  if (!Array.isArray(record.stack) || record.stack.length === 0 || record.stack.length > 3) return null;
  const parsed: WorkflowPaneLevel[] = [];
  for (const candidate of record.stack) {
    if (!candidate || typeof candidate !== 'object') return null;
    const level = candidate as Record<string, unknown>;
    if (level.kind === 'overview' || level.kind === 'all-clear') {
      parsed.push({ kind: level.kind });
      continue;
    }
    if (level.kind === 'workflow') {
      if (typeof level.projectId !== 'string' || typeof level.workflowId !== 'string' || typeof level.label !== 'string') return null;
      parsed.push({ kind: 'workflow', projectId: level.projectId, workflowId: level.workflowId, label: level.label });
      continue;
    }
    if (level.kind === 'run') {
      if (typeof level.projectId !== 'string' || typeof level.workflowId !== 'string' ||
        typeof level.workflowLabel !== 'string' || typeof level.itemId !== 'string' ||
        typeof level.label !== 'string' || typeof level.sweep !== 'boolean') return null;
      parsed.push({
        kind: 'run', projectId: level.projectId, workflowId: level.workflowId,
        workflowLabel: level.workflowLabel, itemId: level.itemId, label: level.label, sweep: level.sweep,
      });
      continue;
    }
    return null;
  }
  if (parsed[0]?.kind !== 'overview') return null;
  if (parsed.length === 2 && parsed[1].kind !== 'workflow' && parsed[1].kind !== 'all-clear') return null;
  if (parsed.length === 3) {
    const workflow = parsed[1];
    const run = parsed[2];
    if (workflow.kind !== 'workflow' || run.kind !== 'run' ||
      workflow.projectId !== run.projectId || workflow.workflowId !== run.workflowId) return null;
  }
  const filter = record.projectFilter;
  if (filter !== null && filter !== undefined && typeof filter !== 'string') return null;
  return { stack: parsed, projectFilter: typeof filter === 'string' && filter ? filter : null };
}

export function getPersistedWorkflowsPaneState(): PersistedWorkflowsPaneState {
  return { stack: stack.map((level) => ({ ...level })), projectFilter };
}

export async function restoreWorkflowsPaneState(snapshot: PersistedWorkflowsPaneState | null): Promise<void> {
  stack = snapshot?.stack?.length ? snapshot.stack.map((level) => ({ ...level })) : [{ kind: 'overview' }];
  projectFilter = snapshot?.projectFilter || null;
  receipts = new Map();
  sweepIndex = -1;
  if (stack[1]?.kind === 'all-clear') stack = [{ kind: 'overview' }];
  for (let index = 1; index < stack.length; index += 1) {
    const level = stack[index];
    try {
      if (level.kind === 'workflow') {
        const catalog = await WorkflowListDefinitions(level.projectId);
        if (!catalog.workflows.some((workflow) => workflow.id === level.workflowId)) {
          stack = stack.slice(0, index);
          break;
        }
      } else if (level.kind === 'run') {
        const loaded = await WorkflowGetItem(level.itemId);
        if (loaded.item.projectId !== level.projectId || loaded.item.workflowId !== level.workflowId) {
          stack = stack.slice(0, index);
          break;
        }
      }
    } catch (cause) {
      const message = cause instanceof Error ? cause.message : String(cause);
      if (/no rows in result set/i.test(message)) {
        stack = stack.slice(0, index);
      } else {
        error = userFacingError(cause, 'Could not restore the workflows pane.');
        addToast('error', error);
      }
      break;
    }
  }
  requestWorkflowsPanePersistence();
  if (paneActive) await loadWorkflowCurrentLevel();
}

export function resetWorkflowsPane(): void {
  requestVersion += 1;
  refreshGeneration += 1;
  stack = [{ kind: 'overview' }];
  projectFilter = null;
  items = [];
  costs = {};
  definitions = [];
  detail = null;
  diffFiles = [];
  diffLoading = false;
  diffError = null;
  diffRequestedItemId = null;
  loading = false;
  error = null;
  queueState = null;
  receipts = new Map();
  sweepIndex = -1;
  intakeOpen = false;
  intakePrefill = null;
  armedAction = null;
  if (autoAdvanceTimer) clearTimeout(autoAdvanceTimer);
  autoAdvanceTimer = null;
  paneActive = false;
  runRefreshInFlight = false;
  runRefreshQueued = false;
  overviewRefreshInFlight = false;
  overviewRefreshQueued = false;
  overviewRefreshProjectIds.clear();
  overviewLoadCount = 0;
  overviewDetailRefreshItemId = null;
  overviewItemEventCaptures.clear();
}

export function workflowThreadFromWire(thread: unknown): Thread {
  return thread as Thread;
}
