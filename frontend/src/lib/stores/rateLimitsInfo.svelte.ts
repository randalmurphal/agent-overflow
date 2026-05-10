// Per-provider rate-limit snapshot, hydrated from the
// `provider:usage` event with `action: 'rate_limits'`. The composer
// toolbar's RateLimitMeter components read from this store via
// `getProviderRateLimit(provider, windowMins)`.
//
// The store is process-global rather than per-pane because:
//   1. Rate limits are an account property, not a thread property —
//      every Claude pane on this machine sees the same 5h/7d ring,
//      every Codex pane sees the same Codex limits.
//   2. The user expectation is "values persist until a new update
//      arrives, even across thread switches and turn completions."
//      Per-pane state was prone to wipe-on-replaceThread bugs and
//      did not survive thread navigation; a global store has only
//      one writer (this module) and never resets in production.
//   3. Mirrors `accountInfo.svelte.ts` (the planType store) so
//      the popover's percent + plan + reset readouts come from
//      symmetric sources.
//
// Shape: `Map<provider, Map<windowMins, RateLimitEntry>>`. Inner map
// is keyed by `windowMins` (300 = 5h, 10080 = 7d) so Claude's
// single-window updates merge into the right slot without clobbering
// the other window. Codex emits both windows in one snapshot —
// either provider lands cleanly through the same merge path.

import type { RateLimitEntry, RateLimitsSnapshot } from '../types/events';

type Provider = 'claude' | 'codex';

let limitsByProvider: Map<Provider, Map<number, RateLimitEntry>> = $state(new Map());

export function setProviderRateLimits(snapshot: RateLimitsSnapshot): void {
  // Empty snapshots arrive on edge-case wires (no entries known yet).
  // Treat as a no-op rather than wiping — the global store's contract
  // is "last-known good value persists until a non-empty update."
  if (!snapshot?.limits?.length) return;
  if (snapshot.provider !== 'claude' && snapshot.provider !== 'codex') return;
  const provider = snapshot.provider as Provider;

  const existing = limitsByProvider.get(provider) ?? new Map<number, RateLimitEntry>();
  const merged = new Map(existing);
  for (const entry of snapshot.limits) {
    // windowMins=0 = parser fallback for unknown rate-limit types
    // (e.g. Claude's `thirty_day` if it ever appeared on the wire).
    // Don't pollute the map with an unrenderable slot.
    if (entry.windowMins > 0) {
      merged.set(entry.windowMins, entry);
    }
  }

  // Reassign the outer Map so $derived consumers re-run. Svelte 5 runes
  // track Map identity, not in-place mutation.
  const next = new Map(limitsByProvider);
  next.set(provider, merged);
  limitsByProvider = next;
}

export function getProviderRateLimit(
  provider: Provider | undefined,
  windowMins: number,
): RateLimitEntry | null {
  if (!provider) return null;
  return limitsByProvider.get(provider)?.get(windowMins) ?? null;
}

// Test-only reset. Production code never clears the global store —
// rate-limit data is intended to live as long as the app runs, since
// it represents the most recent observation of an account-wide limit.
export function resetForTest(): void {
  limitsByProvider = new Map();
}
