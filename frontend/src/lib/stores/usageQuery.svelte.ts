// Shared fetch scaffold for the usage-stat surfaces (composer usage
// chip, sidebar usage footer, usage modal heatmap/totals/model-table/
// top-projects). Each consumer differs only in which `UsageQuery` it
// builds and which reactive state it reads to decide when to refetch
// — that selection logic stays in the caller's `getQuery` closure.
// This helper owns the repeated GetUsageStats fetch/error/race-guard
// mechanics that used to be hand-rolled per component (with two
// different race-guard idioms: a `cancelled` flag in most places, a
// `fetchSeq` counter in the usage chip).
//
// `getQuery` runs inside the `$effect` below, so whatever reactive
// state it reads (getUsagePeriod(), getUsageRefreshVersion(),
// getThreadUsageRefreshVersion(id), a provider filter prop, …)
// registers as a dependency and drives the refetch, exactly as if the
// caller still wrote its own effect. Returning `null` skips the fetch
// entirely and resets `buckets` to `null` — which is what makes this
// usable for lazy fetches too: the usage chip's per-model query
// returns null until its popover is open.

import { GetUsageStats, type UsageBucket, type UsageQuery } from './bindings';

export interface UsageStats {
  readonly buckets: UsageBucket[] | null;
}

export function createUsageStats(getQuery: () => UsageQuery | null): UsageStats {
  let buckets = $state<UsageBucket[] | null>(null);

  $effect(() => {
    const query = getQuery();
    if (!query) {
      buckets = null;
      return;
    }
    let cancelled = false;
    (async () => {
      try {
        const result = await GetUsageStats(query);
        if (!cancelled) buckets = result;
      } catch (err) {
        console.error('usage stats fetch failed', err);
        if (!cancelled) buckets = [];
      }
    })();
    return () => {
      cancelled = true;
    };
  });

  return {
    get buckets() {
      return buckets;
    },
  };
}

/**
 * Local timezone offset in minutes EAST of UTC, matching UsageQuery's
 * `tzOffsetMinutes` convention. `Date.prototype.getTimezoneOffset()`
 * returns minutes WEST of UTC (e.g. +300 for EST), so this negates it.
 */
export function localTzOffsetMinutes(): number {
  return -new Date().getTimezoneOffset();
}
