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
import type { EventOrigin } from '../transport/handle';
import { backendKeyForOrigin } from '../transport/backends';
import { noteWorkflowItem, noteThread } from '../transport/entityIndex';
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

export function applyWorkflowErrorEvent(event: WorkflowErrorEvent, origin?: EventOrigin): void {
  if (!event || typeof event.error !== 'string' || event.error.trim() === '') return;
  const message = event.error.trim().slice(0, 240);
  const key = `${backendKeyForOrigin(origin?.backendId ?? '')}\n${event.itemId ?? ''}\n${message}`;
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
export function applyWorkflowItemStateEvent(event: WorkflowItemStateEvent, origin?: EventOrigin): void {
  noteEventOwner(event, origin);
  applyWorkflowItemState(event);
  applyWorkflowRunMapItemState(event);
}

export function applyWorkflowPhaseStateEvent(event: WorkflowPhaseStateEvent, origin?: EventOrigin): void {
  noteEventOwner(event, origin);
  applyWorkflowPhaseState(event);
  applyWorkflowRunMapPhaseState(event);
}

export function applyWorkflowSoftStopEvent(event: WorkflowSoftStopEvent, origin?: EventOrigin): void {
  noteEventOwner(event, origin);
  applyWorkflowSoftStop(event);
  applyWorkflowRunMapSoftStop(event);
}

export function applyWorkflowEngineStateEvent(event: WorkflowEngineStateEvent, origin?: EventOrigin): void {
  applyWorkflowEngineState(event, backendKeyForOrigin(origin?.backendId ?? ''));
}

function noteEventOwner(event: { itemId?: string; threadId?: string }, origin?: EventOrigin): void {
  const backend = backendKeyForOrigin(origin?.backendId ?? '');
  if (event?.itemId) noteWorkflowItem(event.itemId, backend);
  if (event?.threadId) noteThread(event.threadId, backend);
}

export function applyWorkflowDefinitionsChangedEvent(): void {
  applyWorkflowDefinitionsChanged();
}

export function resetWorkflowEventStateForTest(): void {
  shownErrors.clear();
}
