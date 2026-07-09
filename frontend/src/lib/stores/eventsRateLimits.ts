import { GetRateLimitsSnapshots } from './bindings';
import { setProviderRateLimits } from './rateLimitsInfo.svelte';

// Rate limits are account-scoped and are not persisted on thread rows. Pull
// the backend's retained last-known snapshots both on initial listener setup
// and after a provider:usage transport gap.
export async function hydrateRateLimitsSnapshots(): Promise<void> {
  const snapshots = await GetRateLimitsSnapshots();
  for (const snapshot of snapshots) {
    setProviderRateLimits(snapshot);
  }
}
