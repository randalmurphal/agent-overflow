<script lang="ts">
  // Lifetime-of-selection totals row for the usage modal: one bucket
  // (groupBy '') over the shared period + provider/project filters.
  // Refetches on filter/period change and usageRefresh bumps.

  import { UsageQuery } from '../../stores/bindings';
  import { getUsageRefreshVersion } from '../../stores/usageRefresh.svelte';
  import { getUsagePeriod, periodFromMillis } from '../../stores/usagePeriod.svelte';
  import { createUsageStats, localTzOffsetMinutes } from '../../stores/usageQuery.svelte';
  import { formatTokens } from '../../utils/format';
  import { formatUsageCostOrNull } from '../../utils/usageDisplay';

  interface Props {
    /** '' = all providers, else 'claude' | 'codex'. */
    provider: string;
    /** '' = all projects, else a project id. */
    projectId: string;
  }

  let { provider, projectId }: Props = $props();

  const tzOffsetMinutes = localTzOffsetMinutes();

  const stats = createUsageStats(() => {
    const currentProvider = provider;
    const currentProjectId = projectId;
    const fromMillis = periodFromMillis(getUsagePeriod(), Date.now());
    getUsageRefreshVersion();
    return new UsageQuery({
      groupBy: '',
      fromMillis,
      provider: currentProvider,
      projectId: currentProjectId,
      tzOffsetMinutes,
    });
  });

  let totals = $derived(stats.buckets?.[0] ?? null);

  interface Tile {
    label: string;
    value: string;
  }

  let tiles: Tile[] = $derived.by(() => {
    const t = totals;
    const costLabel = t ? formatUsageCostOrNull(t.costUsd, t.unpricedRows) : null;
    return [
      { label: 'In', value: formatTokens(t?.inputTokens ?? 0) },
      { label: 'Out', value: formatTokens(t?.outputTokens ?? 0) },
      { label: 'Cache Read', value: formatTokens(t?.cacheReadInputTokens ?? 0) },
      { label: 'Cache Write', value: formatTokens(t?.cacheCreationInputTokens ?? 0) },
      { label: 'Cost', value: costLabel ?? '—' },
      { label: 'Sessions', value: String(t?.sessionCount ?? 0) },
    ];
  });
</script>

<!-- One line, tiles at their natural width: equal grid columns at the
     modal's md width force the longer labels (CACHE WRITE) to wrap
     mid-label, while the natural widths sum well under the modal. -->
<div
  class="flex items-start justify-between gap-3"
  data-testid="usage-totals-row"
>
  {#each tiles as tile (tile.label)}
    <div class="flex flex-col gap-0.5">
      <span class="text-[0.625rem] uppercase tracking-[0.12em] text-fg-subtle whitespace-nowrap">{tile.label}</span>
      <span class="text-sm font-medium text-fg tabular-nums" data-testid="usage-totals-value">
        {tile.value}
      </span>
    </div>
  {/each}
</div>
