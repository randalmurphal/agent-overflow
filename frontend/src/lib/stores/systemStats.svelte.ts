// Latest host system stats (CPU%, memory used/total, WSL flag) pushed
// from the Go backend every ~2s on the `system:stats` channel.
// Consumed by the sidebar's SystemStatsFooter. Null until the first
// event arrives so the footer can hide rather than flashing a
// placeholder.

import type { SystemStatsEvent } from '../types/events';

let stats: SystemStatsEvent | null = $state(null);

export function setSystemStats(s: SystemStatsEvent): void {
  stats = s;
}

export function getSystemStats(): SystemStatsEvent | null {
  return stats;
}

// Test-only reset. Production code never clears the store — the
// sampler emits on a fixed cadence for the app's lifetime.
export function resetForTest(): void {
  stats = null;
}
