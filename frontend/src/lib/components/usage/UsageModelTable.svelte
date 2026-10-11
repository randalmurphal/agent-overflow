<script lang="ts">
  // Per-model usage table for the usage modal: groupBy 'model' over the
  // shared period + provider/project filters, sorted by cost desc.
  // Refetches on filter/period change and usageRefresh bumps.

  import { UsageQuery } from '../../stores/bindings';
  import { getUsageRefreshVersion } from '../../stores/usageRefresh.svelte';
  import { getUsagePeriod, periodFromMillis } from '../../stores/usagePeriod.svelte';
  import { createUsageStats, localTzOffsetMinutes } from '../../stores/usageQuery.svelte';
  import { formatTokens } from '../../utils/format';
  import { displayUsageModelLabel } from '../../utils/modelLabels';
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
      groupBy: 'model',
      fromMillis,
      provider: currentProvider,
      projectId: currentProjectId,
      tzOffsetMinutes,
    });
  });

  let rows = $derived(
    [...(stats.buckets ?? [])]
      .sort((a, b) => b.costUsd - a.costUsd)
      .map((bucket) => ({
        key: bucket.bucket,
        // Friendly picker-style name; the raw ledger slug stays
        // reachable via the title tooltip.
        name: bucket.bucket ? displayUsageModelLabel(bucket.bucket) : 'unknown',
        slug: bucket.bucket,
        tokens: formatTokens(bucket.outputTokens),
        cost: formatUsageCostOrNull(bucket.costUsd, bucket.unpricedRows),
      })),
  );
</script>

<div class="flex flex-col gap-1.5" data-testid="usage-model-table">
  <h3 class="text-[0.625rem] uppercase tracking-[0.12em] text-fg-subtle">By Model</h3>
  {#if rows.length === 0}
    <p class="text-xs text-fg-muted" data-testid="usage-model-table-empty">No model usage in this period.</p>
  {:else}
    <table class="w-full text-xs">
      <tbody>
        {#each rows as row (row.key)}
          <tr class="border-t border-border-subtle first:border-t-0" data-testid="usage-model-row">
            <td
              class="py-1 pr-2 text-fg truncate max-w-0 w-full"
              title={row.slug}
              data-testid="usage-model-row-name"
            >
              {row.name}
            </td>
            <td class="py-1 px-2 text-right tabular-nums text-fg-muted whitespace-nowrap">
              {row.tokens}
            </td>
            <td class="py-1 pl-2 text-right tabular-nums text-fg whitespace-nowrap" data-testid="usage-model-row-cost">
              {row.cost ?? '—'}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>
