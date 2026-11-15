import { applyProviderUpdatesEvent, type ProviderUpdateStatus } from '../../lib/stores/providerUpdates.svelte';
import { wailsEventOn } from '../../lib/stores/wailsEvents';

/** Subscribe the provider update store to its channel, as stores/events.ts does. */
export function listenProviderUpdates(): () => void {
  return wailsEventOn<ProviderUpdateStatus[] | null>('provider:updates', applyProviderUpdatesEvent);
}
