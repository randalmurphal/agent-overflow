import type {
  WorkflowErrorEvent,
  WorkflowItemStateEvent,
  WorkflowPhaseStateEvent,
  WorkflowQueueStateEvent,
} from '../types/workflow';
import { addToast } from './toast.svelte';
import {
  applyWorkflowItemState,
  applyWorkflowPhaseState,
  applyWorkflowQueueState,
} from './workflowsPane.svelte';

const MAX_ERROR_DEDUPE_KEYS = 100;
const shownErrors = new Set<string>();

export function applyWorkflowItemStateEvent(event: WorkflowItemStateEvent): void {
  if (!event?.itemId || !event.to) return;
  applyWorkflowItemState(event);
}

export function applyWorkflowQueueStateEvent(event: WorkflowQueueStateEvent): void {
  if (!event || typeof event.active !== 'boolean' || !Number.isFinite(event.globalConcurrency)) return;
  applyWorkflowQueueState(event);
}

export function applyWorkflowPhaseStateEvent(event: WorkflowPhaseStateEvent): void {
  if (!event?.itemId || !event.phaseId || !Number.isFinite(event.attempt) || !event.status) return;
  applyWorkflowPhaseState(event);
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
