// Refresh signals for usage-display surfaces (composer usage chip,
// sidebar usage footer, usage modal). Bumped by the
// provider:turn_completed listener — the moment new usage_ledger rows
// may exist — so consumers refetch GetUsageStats via a `$derived`/
// `$effect` on the version instead of subscribing to provider events
// themselves. Deliberately bare counters: the payload is always
// refetched from the backend (SQLite is the source of truth), never
// pushed through this store.
//
// Two signals exist because their consumers have different scopes:
//   - `version` (global) is read by surfaces that aggregate across every
//     thread (sidebar footer, usage modal) — any turn completing
//     anywhere may change their totals.
//   - `perThreadVersion` is read by the composer usage chip, which is
//     scoped to a single thread. Without it, a chip for thread A would
//     refetch every time an unrelated thread B's turn settles.

import { SvelteMap } from 'svelte/reactivity';

let version = $state(0);

const perThreadVersion = new SvelteMap<string, number>();

// Plain (non-reactive) gate for `perThreadVersion` key creation. Svelte's
// SvelteMap subscribes a reader of a MISSING key to the map's whole-map
// version signal (so it can notice if that key is ever added) — see
// svelte/reactivity's Map#get. If a chip for thread A read its own
// not-yet-created key, it would stay coupled to every OTHER thread's
// first-ever bump until thread A itself bumped once. Routing every touch
// of a thread id through `ensureThreadEntry` first means the key always
// exists before a reactive read ever sees it, so that read depends only
// on that thread's own entry — never the shared version.
const knownThreadIds = new Set<string>();

function ensureThreadEntry(threadId: string): void {
  if (knownThreadIds.has(threadId)) return;
  knownThreadIds.add(threadId);
  perThreadVersion.set(threadId, 0);
}

export function getUsageRefreshVersion(): number {
  return version;
}

export function getThreadUsageRefreshVersion(threadId: string): number {
  ensureThreadEntry(threadId);
  return perThreadVersion.get(threadId) ?? 0;
}

export function bumpUsageRefresh(threadId: string): void {
  version += 1;
  ensureThreadEntry(threadId);
  perThreadVersion.set(threadId, (perThreadVersion.get(threadId) ?? 0) + 1);
}

// Test-only reset.
export function resetUsageRefreshForTest(): void {
  version = 0;
  knownThreadIds.clear();
  perThreadVersion.clear();
}
