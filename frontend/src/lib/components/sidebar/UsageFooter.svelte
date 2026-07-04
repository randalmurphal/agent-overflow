<script lang="ts">
  // Sidebar per-provider usage footer. Same visual idiom as
  // SystemStatsFooter, mounted directly above it. Fetches
  // GetUsageStats grouped by provider for the selected period on mount,
  // period change, and usageRefresh bumps.
  //
  // Clicking anywhere on the row opens the usage modal (owned here to
  // keep Sidebar.svelte layout-only); clicking the period label cycles
  // the period instead, via stopPropagation — same nested
  // button-inside-clickable-row idiom as ThreadRow's chevron.
  // Hidden entirely when the selected period has no usage at all.

  import { UsageQuery } from '../../stores/bindings';
  import { getUsageRefreshVersion } from '../../stores/usageRefresh.svelte';
  import { cycleUsagePeriod, getUsagePeriod, periodFromMillis } from '../../stores/usagePeriod.svelte';
  import { createUsageStats, localTzOffsetMinutes } from '../../stores/usageQuery.svelte';
  import { formatTokens } from '../../utils/format';
  import { formatUsageCostOrNull } from '../../utils/usageDisplay';
  import UsageModal from '../usage/UsageModal.svelte';

  interface ProviderRow {
    provider: string;
    /** Pre-joined "862.4k · $118.42" (cost segment omitted when
     *  unpriced-only). Built in script so template whitespace collapsing
     *  can't glue the separator to the token count. */
    valueLabel: string;
  }

  const tzOffsetMinutes = localTzOffsetMinutes();

  const stats = createUsageStats(() => {
    // Read the refresh version so this effect re-runs on turn completion.
    getUsageRefreshVersion();
    return new UsageQuery({
      groupBy: 'provider',
      fromMillis: periodFromMillis(getUsagePeriod(), Date.now()),
      tzOffsetMinutes,
    });
  });

  let rows: ProviderRow[] = $derived(
    (stats.buckets ?? [])
      .filter((b) => b.inputTokens + b.outputTokens > 0 || b.costUsd > 0)
      .map((b) => {
        // Output tokens only — the shared usage-surface token metric
        // (input re-bills the growing context every turn). The presence
        // filter above stays broad so a provider with cost but no
        // output yet still gets its row.
        const tokens = formatTokens(b.outputTokens);
        const cost = formatUsageCostOrNull(b.costUsd, b.unpricedRows);
        return {
          provider: b.bucket,
          valueLabel: cost ? `${tokens} · ${cost}` : tokens,
        };
      }),
  );

  let modalOpen = $state(false);

  function openModal(): void {
    modalOpen = true;
  }

  function handleRowKeydown(e: KeyboardEvent): void {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      openModal();
    }
  }

  function handlePeriodClick(e: MouseEvent): void {
    e.stopPropagation();
    cycleUsagePeriod();
  }
</script>

{#if rows.length > 0}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    class="border-t border-border-subtle px-3 py-1.5 shrink-0 flex items-center justify-between gap-3 text-[0.6875rem] leading-tight text-fg-muted cursor-pointer hover:text-fg transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
    data-testid="sidebar-usage-footer"
    role="button"
    tabindex="0"
    onclick={openModal}
    onkeydown={handleRowKeydown}
  >
    <!-- One line per provider so neither row truncates the other. The
         fixed label column keeps the centered values on a shared axis
         across lines. -->
    <span class="flex flex-col gap-1.5 flex-1 min-w-0">
      {#each rows as row (row.provider)}
        <span class="grid grid-cols-[3.5rem_1fr] items-center gap-2" data-testid="usage-footer-row">
          <span class="text-fg-subtle uppercase tracking-[0.12em]">{row.provider}</span>
          <span class="text-center tabular-nums whitespace-pre truncate">{row.valueLabel}</span>
        </span>
      {/each}
    </span>
    <button
      type="button"
      class="text-fg-subtle uppercase tracking-[0.12em] shrink-0 hover:text-fg cursor-pointer"
      data-testid="usage-footer-period"
      onclick={handlePeriodClick}
    >
      {getUsagePeriod()}
    </button>
  </div>
{/if}

<UsageModal
  open={modalOpen}
  onClose={() => {
    modalOpen = false;
  }}
/>
