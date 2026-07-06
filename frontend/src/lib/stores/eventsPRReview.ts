import { applyPRUpdatedEvent, type PRUpdatedEvent } from './reviewPane.svelte';

export function applyPRReviewUpdated(event: PRUpdatedEvent | null | undefined): void {
  if (!event?.subscriptionId) return;
  applyPRUpdatedEvent(event);
}
