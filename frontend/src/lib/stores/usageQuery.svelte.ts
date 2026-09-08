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
import { selectedTelemetryComputers, missingTelemetrySelection } from './telemetryComputers.svelte';
import { resolveThreadBackend, projectBackend } from '../transport/entityIndex';
import { requireEntityBackend, withBackendTarget } from '../transport/backends';
import { combineUsageBuckets } from '../utils/usageBuckets';

export interface UsageStats {
  readonly buckets: UsageBucket[] | null;
  readonly unavailable: readonly string[];
  readonly loading: boolean;
}

export function createUsageStats(getQuery: () => UsageQuery | null): UsageStats {
  let buckets = $state<UsageBucket[] | null>(null);
  let unavailable = $state<string[]>([]);
  let loading = $state(false);
  let previousKey = '';

  $effect(() => {
    const query = getQuery();
    if (!query) {
      buckets = null;
      unavailable = [];
      loading = false;
      return;
    }
    const projectOwner = query.projectId ? projectBackend(query.projectId) : undefined;
    const computers = query.threadId ? null : selectedTelemetryComputers('usage').filter((computer) => projectOwner === undefined || computer.key === projectOwner);
    const online = computers?.filter((computer) => computer.connected);
    unavailable = computers?.filter((computer) => !computer.connected).map((computer) => computer.name) ?? [];
    if (computers && missingTelemetrySelection('usage') > 0) unavailable.push('Removed computer selection');
    const key = JSON.stringify([query, online?.map((computer) => computer.key)]);
    if (key !== previousKey) buckets = null;
    previousKey = key;
    loading = true;
    let cancelled = false;
    (async () => {
      try {
        if (online) {
          const results = await Promise.allSettled(online.map((computer) => withBackendTarget(computer.key, () => GetUsageStats(query))));
          if (cancelled) return;
          const rows: UsageBucket[] = [];
          const failed: string[] = [];
          results.forEach((result, i) => {
            if (result.status === 'fulfilled') rows.push(...(result.value ?? []));
            else failed.push(online[i].name);
          });
          buckets = combineUsageBuckets(rows);
          unavailable = [...unavailable, ...failed];
        } else {
          const owner = requireEntityBackend(resolveThreadBackend(query.threadId));
          const result = await withBackendTarget(owner, () => GetUsageStats(query));
          if (!cancelled) buckets = combineUsageBuckets(result ?? []);
        }
      } catch (err) {
        console.error('usage stats fetch failed', err);
        if (!cancelled) buckets = [];
      } finally {
        if (!cancelled) loading = false;
      }
    })();
    return () => {
      cancelled = true;
    };
  });

  return {
    get unavailable() { return unavailable; },
    get loading() { return loading; },
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
