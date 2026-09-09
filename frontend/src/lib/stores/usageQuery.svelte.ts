// Mounted usage surfaces read persisted totals through one serialized refresh.
// Repeated progress events coalesce without starving a slow RPC. Query/owner
// changes invalidate old replies; reconnect snapshots use the same reader.
import { untrack } from 'svelte';
import { errString } from '../utils/errors';
import { GetUsageStats, type UsageBucket, type UsageQuery } from './bindings';
import { selectedTelemetryComputers, missingTelemetrySelection } from './telemetryComputers.svelte';
import { resolveThreadBackend, projectBackend } from '../transport/entityIndex';
import { requireEntityBackend, withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import { combineUsageBuckets } from '../utils/usageBuckets';
import { createRefreshScheduler } from '../utils/refreshScheduler';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { onThreadUsageRefresh } from './usageRefresh.svelte';
import { onBackendRecovery, holdBackendRecovery } from './transportRecovery';

export interface UsageStats {
  readonly buckets: UsageBucket[] | null;
  readonly unavailable: readonly string[];
  readonly loading: boolean;
  readonly error: string | null;
}

export function createUsageStats(getQuery: () => UsageQuery | null): UsageStats {
  let buckets = $state<UsageBucket[] | null>(null);
  let unavailable = $state<string[]>([]);
  let loading = $state(false);
  let error = $state<string | null>(null);
  let reportedError = $state<string | null>(null);
  let previousKey = '';
  let query: UsageQuery | null = null;
  let targets: { key: BackendKey; name: string }[] = [];
  let offline: string[] = [];

  const scheduler = createRefreshScheduler({
    name: 'usage stats refresh', delayMs: 100, maxWaitMs: 500, minIntervalMs: 250,
    async run(token) {
      if (!query) return;
      const captured = query;
      const owners = targets;
      const missing = offline;
      loading = true;
      const work = Promise.allSettled(owners.map((owner) => withBackendTarget(owner.key, () => GetUsageStats(captured))));
      for (const owner of owners) holdBackendRecovery(owner.key, work);
      const results = await work;
      if (!token.isCurrent()) return;
      const rows: UsageBucket[] = [];
      const failed: string[] = [];
      results.forEach((result, i) => {
        if (result.status === 'fulfilled') rows.push(...(result.value ?? []));
        else {
          failed.push(owners[i].name);
          reportFrontendDiagnostic('usage stats fetch failed', errString(result.reason));
        }
      });
      // A failed refresh must not turn a known total into zero.
      if (!captured.threadId || failed.length === 0) buckets = combineUsageBuckets(rows);
      unavailable = [...missing, ...failed];
      error = failed.length ? 'Usage could not be refreshed. Showing the last available report.' : null;
      loading = false;
    },
  });

  $effect(() => {
    const requested = getQuery();
    const projectOwner = requested?.projectId ? projectBackend(requested.projectId) : undefined;
    const owner = requested?.threadId ? resolveThreadBackend(requested.threadId) : undefined;
    const computers = requested && !requested.threadId
      ? selectedTelemetryComputers('usage').filter((computer) => projectOwner === undefined || computer.key === projectOwner)
      : null;
    const online = computers?.filter((computer) => computer.connected);
    const missing = computers?.filter((computer) => !computer.connected).map((computer) => computer.name) ?? [];
    if (computers && missingTelemetrySelection('usage') > 0) missing.push('Removed computer selection');
    const key = JSON.stringify([requested, owner, online?.map((computer) => computer.key)]);
    untrack(() => {
      query = requested;
      offline = missing;
      targets = [];
      if (requested) {
        try {
          targets = online ?? [{ key: requireEntityBackend(owner), name: 'Thread computer' }];
        } catch (err) {
          scheduler.reset(); buckets = null; loading = false;
          error = 'Usage is unavailable until the thread computer is known.';
          reportFrontendDiagnostic('usage stats owner unavailable', errString(err));
          return;
        }
      }
      if (key !== previousKey) {
        scheduler.reset(); buckets = null; error = null; reportedError = null;
        unavailable = missing; loading = false;
        previousKey = key;
        if (requested) scheduler.request({ immediate: true });
      } else if (requested) scheduler.request();
    });
  });

  $effect(() => {
    const threadId = getQuery()?.threadId;
    if (threadId) return onThreadUsageRefresh(threadId, (message) => { reportedError = message ?? null; scheduler.request(); });
  });

  $effect(() => {
    const off = onBackendRecovery((backend, phase) => {
      if (!targets.some((target) => target.key === backend)) return;
      if (phase === 'start') { scheduler.reset(); scheduler.request({ immediate: true }); }
      if (phase === 'cancel') { scheduler.reset(); loading = false; }
    });
    return () => { off(); scheduler.dispose(); };
  });

  return {
    get unavailable() { return unavailable; },
    get loading() { return loading; },
    get error() { return reportedError ?? error; },
    get buckets() { return buckets; },
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
