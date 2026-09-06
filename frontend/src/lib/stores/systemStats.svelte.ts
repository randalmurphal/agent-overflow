// Latest host system stats (CPU%, memory used/total, WSL flag) pushed
// from the Go backend every ~2s on the `system:stats` channel.
//
// Consumed by the sidebar's SystemStatsFooter. Null until the first
// event arrives so the footer can hide rather than flashing a
// placeholder.

import type { SystemStatsEvent } from '../types/events';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { onBackendDetached } from '../transport/backends';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';

const stats = createKeyedSignalRegistry<SystemStatsEvent | null>(null);

export function setSystemStats(s: SystemStatsEvent, backend: BackendKey = HOME_BACKEND): void {
  stats.set(backend, s);
}

export function getSystemStats(backend: BackendKey = HOME_BACKEND): SystemStatsEvent | null {
  return stats.get(backend);
}

// Test-only reset. Production code never clears the store — the
// sampler emits on a fixed cadence for the app's lifetime.
export function resetForTest(): void {
  stats.reset();
}

onBackendDetached(({ backendId }) => stats.drop(backendId));
