import { GetRateLimitsSnapshots } from './bindings';
import { setProviderRateLimits } from './rateLimitsInfo.svelte';
import { getAttachedBackends } from './attachedBackends.svelte';
import { withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';

// Quotas describe accounts on their source computer. A snapshot contains no
// machine ID itself; the connection supplies that part of its identity.
export async function hydrateRateLimitsSnapshots(backend?: BackendKey): Promise<void> {
  const computers = backend === undefined ? getAttachedBackends().map((entry) => entry.id) : [backend];
  const results = await Promise.allSettled(computers.map(async (key) => {
    const snapshots = await withBackendTarget(key, () => GetRateLimitsSnapshots());
    for (const snapshot of snapshots) setProviderRateLimits(snapshot, key);
  }));
  for (const result of results) if (result.status === 'rejected') console.warn('Could not read computer quotas:', result.reason);
}
