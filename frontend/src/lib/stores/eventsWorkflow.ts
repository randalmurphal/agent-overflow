// The typed `workflow:*` channel, fanned into the run cache. Registered from
// events.ts alongside every other domain module; the cache
// (workflowRuns.svelte.ts) owns the state, this file only decodes and routes.

import type {
  WorkflowEngineStateEvent,
  WorkflowErrorEvent,
  WorkflowItemStateEvent,
  WorkflowPhaseStateEvent,
  WorkflowSoftStopEvent,
} from '../types/workflow';
import { addToast } from './toast.svelte';
import {
  applyWorkflowDefinitionsChanged,
  applyWorkflowEngineState,
  applyWorkflowItemState,
  applyWorkflowPhaseState,
  applyWorkflowSoftStop,
} from './workflowRuns.svelte';
import {
  applyWorkflowRunMapItemState,
  applyWorkflowRunMapPhaseState,
  applyWorkflowRunMapSoftStop,
} from './workflowRunMap.svelte';

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

// The three run-record events feed TWO consumers, deliberately: the overlay's
// run cache (list rows, badges, the per-run detail) and the run map's tree
// view, which is entity-keyed and patches in place rather than refetching the
// whole run per event. They read the same frame and answer different
// questions, so neither is derivable from the other.
export function applyWorkflowItemStateEvent(event: WorkflowItemStateEvent): void {
  applyWorkflowItemState(event);
  applyWorkflowRunMapItemState(event);
}

export function applyWorkflowPhaseStateEvent(event: WorkflowPhaseStateEvent): void {
  applyWorkflowPhaseState(event);
  applyWorkflowRunMapPhaseState(event);
}

export function applyWorkflowSoftStopEvent(event: WorkflowSoftStopEvent): void {
  applyWorkflowSoftStop(event);
  applyWorkflowRunMapSoftStop(event);
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
