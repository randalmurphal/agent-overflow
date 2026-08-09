import { applyPRUpdatedEvent, type PRUpdatedEvent } from './prReviewStore.svelte';

// Routed by PR key, not subscription id: one pump serves every pane on a
// PR, and the store applies to the entity every one of them derives from.
export function applyPRReviewUpdated(event: PRUpdatedEvent | null | undefined): void {
  if (!event?.prKey) return;
  applyPRUpdatedEvent(event);
}
