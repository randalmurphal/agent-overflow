// Latest host system stats (CPU%, memory used/total, WSL flag) pushed
// from the Go backend every ~2s on the `system:stats` channel.
//
// PHASE 7, HOME-ONLY: one slot, last writer wins, so with two backends
// attached the footer shows whichever machine pushed most recently. The
// event carries its origin (`wailsEventOn`'s second argument) and keying
// this by backend is a one-line change — but WHICH machine's load a single
// footer should show is a design question remote-access §10 owns, not a
// keying one, so it stays as it is until that answers.
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
