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
    // Defense-in-depth: each provider's parser drops snapshots for
    // unknown windows, but multiple parsers feed this same map (Claude,
    // Codex, plus any future provider). Refuse `windowMins<=0` here so
    // a parser regression upstream can't pollute the global store with
    // a slot the rings can't render.
    if (entry.windowMins <= 0) continue;

    // Stale-event defense around window-reset boundaries.
    //
    // Multiple sessions emit `rate_limit_event` independently, and a
    // long-running Claude session can keep emitting its in-process
    // pre-reset reading for several requests after a fresher session
    // has already observed the new (post-reset) reading. The events
    // arrive interleaved on the wire, so without this guard the ring
    // visibly oscillates between the old high percentage and the new
    // low one until every session catches up.
    //
    // `resetsAt` is the next reset boundary as the wire saw it: a
    // pre-reset event reports the boundary that's about to fire,
    // a post-reset event reports the boundary 5h/7d later. The newer
    // window's `resetsAt` strictly dominates. Drop incoming entries
    // whose `resetsAt` is older than what we've already stored.
    //
    // Equal `resetsAt` (= same window, fresher reading) DOES update —
    // usedPercent climbs monotonically inside a window, so the latest
    // event always carries the most current reading.
    const prior = merged.get(entry.windowMins);
    if (prior && prior.resetsAt > entry.resetsAt) continue;

    merged.set(entry.windowMins, entry);
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
