import type { EventOrigin } from '../transport/handle';
import { backendKeyForOrigin } from '../transport/backends';
import type { PRListSnapshot } from './bindings';
import {
  applyPipelineCILogEvent,
  applyPipelineUpdatedEvent,
  applyPullsUpdatedEvent,
  type PipelineCILogEvent,
  type PipelineUpdatedEvent,
} from './pullsStore.svelte';

// The Pull requests page's channels: the list pump's snapshot and the
// pipeline pumps' frames, routed to the page store with the computer that
// sent them. A pipeline's followed logs ride pr:ci_log with a pipelineKey
// and no prKey (eventsPRReview.ts forwards them here).

export function applyPullsUpdated(snapshot: PRListSnapshot | null | undefined, origin?: EventOrigin): void {
  if (!snapshot || !Array.isArray(snapshot.projects)) return;
  applyPullsUpdatedEvent(snapshot, backendKeyForOrigin(origin?.backendId ?? ''));
}

export function applyPipelineUpdated(event: PipelineUpdatedEvent | null | undefined, origin?: EventOrigin): void {
  if (!event?.key) return;
  applyPipelineUpdatedEvent(event, backendKeyForOrigin(origin?.backendId ?? ''));
}

export function applyPipelineCILog(event: PipelineCILogEvent, origin?: EventOrigin): void {
  if (!event.pipelineKey) return;
  applyPipelineCILogEvent(event, backendKeyForOrigin(origin?.backendId ?? ''));
}
