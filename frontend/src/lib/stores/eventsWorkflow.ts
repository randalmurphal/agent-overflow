// The typed `workflow:*` channel, fanned into the run cache. Registered from
// events.ts alongside every other domain module; the cache
// (workflowRuns.svelte.ts) owns the state, this file only decodes and routes.

import type {
  WorkflowEngineStateEvent,
  WorkflowErrorEvent,
  WorkflowItemStateEvent,
  WorkflowPhaseStateEvent,
} from '../types/workflow';
import { addToast } from './toast.svelte';
import {
  applyWorkflowDefinitionsChanged,
  applyWorkflowEngineState,
  applyWorkflowItemState,
  applyWorkflowPhaseState,
} from './workflowRuns.svelte';

const MAX_ERROR_DEDUPE_KEYS = 100;
const shownErrors = new Set<string>();

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

export function applyWorkflowItemStateEvent(event: WorkflowItemStateEvent): void {
  applyWorkflowItemState(event);
}

export function applyWorkflowPhaseStateEvent(event: WorkflowPhaseStateEvent): void {
  applyWorkflowPhaseState(event);
}

export function applyWorkflowEngineStateEvent(event: WorkflowEngineStateEvent): void {
  applyWorkflowEngineState(event);
}

export function applyWorkflowDefinitionsChangedEvent(): void {
  applyWorkflowDefinitionsChanged();
}

export function resetWorkflowEventStateForTest(): void {
  shownErrors.clear();
}
