import type { EventOrigin } from '../transport/handle';
import { backendKeyForOrigin } from '../transport/backends';
import {
  applyPRCILogEvent,
  applyPRCIUpdatedEvent,
  applyPRUpdatedEvent,
  type PRUpdatedEvent,
} from './prReviewStore.svelte';
import type { PRCILogEvent, PRCIUpdatedEvent } from './prReviewCI.svelte';
import { applyPipelineCILog } from './eventsPulls';

// Routed by PR key, not subscription id: one pump serves every pane on a
// PR, and the store applies to the entity every one of them derives from.
export function applyPRReviewUpdated(event: PRUpdatedEvent | null | undefined, origin?: EventOrigin): void {
  if (!event?.prKey) return;
  applyPRUpdatedEvent(event, backendKeyForOrigin(origin?.backendId ?? ''));
}

export function applyPRReviewCIUpdated(event: PRCIUpdatedEvent | null | undefined, origin?: EventOrigin): void {
  if (!event?.prKey) return;
  applyPRCIUpdatedEvent(event, backendKeyForOrigin(origin?.backendId ?? ''));
}

export function applyPRReviewCILog(event: PRCILogEvent | null | undefined, origin?: EventOrigin): void {
  if (!event) return;
  if (!event.prKey) {
    if (event.pipelineKey) applyPipelineCILog({ ...event, pipelineKey: event.pipelineKey }, origin);
    return;
  }
  applyPRCILogEvent(event, backendKeyForOrigin(origin?.backendId ?? ''));
}
