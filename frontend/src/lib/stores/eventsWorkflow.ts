import type {
  WorkflowErrorEvent,
  WorkflowItemStateEvent,
  WorkflowPhaseStateEvent,
  WorkflowQueueStateEvent,
} from '../types/workflow';
import { addToast } from './toast.svelte';
import {
  applyWorkflowItemState,
  applyWorkflowDefinitionsChanged,
  applyWorkflowPhaseState,
  applyWorkflowQueueState,
} from './workflowsPane.svelte';
import {
  applyWorkflowSidebarItemState,
  applyWorkflowSidebarPhaseState,
  applyWorkflowSidebarQueueState,
} from './workflowsSidebar.svelte';

const MAX_ERROR_DEDUPE_KEYS = 100;
const shownErrors = new Set<string>();

export function applyWorkflowItemStateEvent(event: WorkflowItemStateEvent): void {
  if (!event?.itemId || !event.projectId || !event.to) return;
  applyWorkflowItemState(event);
  applyWorkflowSidebarItemState(event);
}

export function applyWorkflowQueueStateEvent(event: WorkflowQueueStateEvent): void {
  if (!event || typeof event.active !== 'boolean' || !Number.isFinite(event.globalConcurrency)) return;
  if (event.runningCount !== undefined && (!Number.isFinite(event.runningCount) || event.runningCount < 0)) return;
  if (event.slotCapacity !== undefined && (!Number.isFinite(event.slotCapacity) || event.slotCapacity < 0)) return;
  if (event.projects !== undefined && (!Array.isArray(event.projects) || event.projects.some((project) =>
    !project?.projectId || typeof project.paused !== 'boolean' ||
    !Number.isFinite(project.concurrency) || project.concurrency < 0 || project.concurrency > 32 ||
    !Number.isFinite(project.runningCount) || project.runningCount < 0
  ))) return;
  applyWorkflowQueueState(event);
  applyWorkflowSidebarQueueState();
}

export function applyWorkflowDefinitionsChangedEvent(): void {
  applyWorkflowDefinitionsChanged();
}

export function applyWorkflowPhaseStateEvent(event: WorkflowPhaseStateEvent): void {
  if (!event?.itemId || !event.phaseId || !Number.isFinite(event.attempt) || !event.status) return;
  applyWorkflowPhaseState(event);
  applyWorkflowSidebarPhaseState(event);
}

export function applyWorkflowErrorEvent(event: WorkflowErrorEvent): void {
  if (!event || typeof event.error !== 'string' || event.error.trim() === '') return;
  const message = event.error.trim().slice(0, 240);
  const key = `${event.itemId ?? ''}\n${message}`;
  if (shownErrors.has(key)) return;
  shownErrors.add(key);
  if (shownErrors.size > MAX_ERROR_DEDUPE_KEYS) {
    const oldest = shownErrors.values().next().value;
    if (oldest) shownErrors.delete(oldest);
  }
  addToast('error', message);
}

export function resetWorkflowEventStateForTest(): void {
  shownErrors.clear();
}
