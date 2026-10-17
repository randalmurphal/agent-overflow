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
  // While a reply is pending the footer holds the last answered shape for
  // this selection and period (one row when none is recorded, nothing
  // when the last reply had no usage), so the sidebar above it does not
  // shift when usage arrives. A reply that differs resizes it once.

  import { UsageQuery } from '../../stores/bindings';
  import { getUsageRefreshVersion } from '../../stores/usageRefresh.svelte';
  import { cycleUsagePeriod, getUsagePeriod, periodFromMillis } from '../../stores/usagePeriod.svelte';
  import { createUsageStats, localTzOffsetMinutes } from '../../stores/usageQuery.svelte';
  import { telemetrySelection, telemetrySelectionLabel } from '../../stores/telemetryComputers.svelte';
  import { getUsageFooterShape, recordUsageFooterShape, usageFooterShapeKey } from '../../stores/usageFooterShape.svelte';
  import { formatTokens } from '../../utils/format';
  import { formatUsageCostOrNull } from '../../utils/usageDisplay';
  import LazyOverlay from '../primitives/LazyOverlay.svelte';

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

  // The query's own effect runs first and clears a superseded reply, so
  // a reply is never recorded under the next selection or period.
  const shapeKey = $derived(usageFooterShapeKey(telemetrySelection('usage'), getUsagePeriod()));
  let lastAnswered = $state<number | null>(null);
  $effect(() => {
    if (stats.buckets === null || stats.pending.length > 0) return;
    // An unavailable report holds one row, like a provider's.
    const answered = rows.length > 0 ? rows.length : stats.unavailable.length > 0 ? 1 : 0;
    recordUsageFooterShape(shapeKey, answered);
    lastAnswered = answered;
  });
  const waiting = $derived(stats.buckets === null || stats.loading || stats.pending.length > 0);
  const shape = $derived(getUsageFooterShape(shapeKey) ?? lastAnswered ?? 1);

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

{#if rows.length > 0 || stats.unavailable.length > 0 || telemetrySelection('usage') !== null || (waiting && shape > 0)}
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
      {#if telemetrySelection('usage') !== null}
        <span class="truncate text-fg-subtle">{telemetrySelectionLabel('usage')}</span>
      {/if}
      {#if stats.unavailable.length > 0}
        <span class="truncate text-fg-subtle" title={`Unavailable: ${stats.unavailable.join(', ')}`}>{rows.length > 0 ? 'Partial usage' : 'Usage unavailable'}</span>
      {/if}
      {#if rows.length === 0 && stats.unavailable.length === 0}
        <span class="text-fg-subtle" data-testid="usage-footer-empty">{waiting ? 'Loading usage…' : 'No usage in this selection'}</span>
        {#if waiting}
          {#each { length: Math.max(shape - 1, 0) }, i (i)}<span data-testid="usage-footer-placeholder-row">&nbsp;</span>{/each}
        {/if}
      {/if}
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

<LazyOverlay
  load={() => import('../usage/UsageModal.svelte')}
  active={modalOpen}
  props={{
    open: modalOpen,
    onClose: () => {
      modalOpen = false;
    },
  }}
/>
