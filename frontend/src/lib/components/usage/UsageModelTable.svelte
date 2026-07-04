<script lang="ts">
  // Per-model usage table for the usage modal: groupBy 'model' over the
  // shared period + provider filter, sorted by cost desc. Refetches on
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
    return new UsageQuery({ groupBy: 'model', fromMillis, provider: currentProvider, tzOffsetMinutes });
  });

  let rows = $derived([...(stats.buckets ?? [])].sort((a, b) => b.costUsd - a.costUsd));
</script>

<div class="flex flex-col gap-1.5" data-testid="usage-model-table">
  <h3 class="text-[0.625rem] uppercase tracking-[0.12em] text-fg-subtle">By Model</h3>
  {#if rows.length === 0}
    <p class="text-xs text-fg-muted" data-testid="usage-model-table-empty">No model usage in this period.</p>
  {:else}
    <table class="w-full text-xs">
      <tbody>
        {#each rows as row (row.bucket)}
          <tr class="border-t border-border-subtle first:border-t-0" data-testid="usage-model-row">
            <td class="py-1 pr-2 text-fg truncate max-w-0 w-full" data-testid="usage-model-row-name">
              {row.bucket || 'unknown'}
            </td>
            <td class="py-1 px-2 text-right tabular-nums text-fg-muted whitespace-nowrap">
              {formatTokens(row.inputTokens + row.outputTokens)}
            </td>
            <td class="py-1 pl-2 text-right tabular-nums text-fg whitespace-nowrap" data-testid="usage-model-row-cost">
              {formatUsageCostOrNull(row.costUsd, row.unpricedRows) ?? '—'}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>
