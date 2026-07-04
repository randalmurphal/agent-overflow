<script lang="ts">
  // Lifetime-of-selection totals row for the usage modal: one bucket
  // (groupBy '') over the shared period + provider filter. Refetches on
  // provider/period change and usageRefresh bumps.

  import { UsageQuery } from '../../stores/bindings';
  import { getUsageRefreshVersion } from '../../stores/usageRefresh.svelte';
  import { getUsagePeriod, periodFromMillis } from '../../stores/usagePeriod.svelte';
  import { createUsageStats, localTzOffsetMinutes } from '../../stores/usageQuery.svelte';
  import { formatTokens } from '../../utils/format';
  import { formatUsageCostOrNull } from '../../utils/usageDisplay';

  interface Props {
    /** '' = all providers, else 'claude' | 'codex'. */
    provider: string;
  }

  let { provider }: Props = $props();

  const tzOffsetMinutes = localTzOffsetMinutes();

  const stats = createUsageStats(() => {
    const currentProvider = provider;
    const fromMillis = periodFromMillis(getUsagePeriod(), Date.now());
    getUsageRefreshVersion();
    return new UsageQuery({ groupBy: '', fromMillis, provider: currentProvider, tzOffsetMinutes });
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
      { label: 'Turns', value: String(t?.turnCount ?? 0) },
    ];
  });
</script>

<div
  class="grid grid-cols-3 sm:grid-cols-6 gap-3"
  data-testid="usage-totals-row"
>
  {#each tiles as tile (tile.label)}
    <div class="flex flex-col gap-0.5">
      <span class="text-[0.625rem] uppercase tracking-[0.12em] text-fg-subtle">{tile.label}</span>
      <span class="text-sm font-medium text-fg tabular-nums" data-testid="usage-totals-value">
        {tile.value}
      </span>
    </div>
  {/each}
</div>
