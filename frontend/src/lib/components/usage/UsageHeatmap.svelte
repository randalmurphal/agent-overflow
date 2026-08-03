<script lang="ts">
  // AO's own ledger as a GitHub-style contribution heatmap: last 26
  // Sunday-start weeks. ALL date math lives in heatmapGrid.ts and all
  // rendering in UsageHeatmapGrid.svelte — this component only owns the
  // fetch and the tooltip wording. Independent of the footer/modal period
  // selector by design (always the fixed 26-week window); only the
  // provider/project filters and usageRefresh bumps trigger a refetch.

  import { UsageQuery } from '../../stores/bindings';
  import { getUsageRefreshVersion } from '../../stores/usageRefresh.svelte';
  import { createUsageStats, localTzOffsetMinutes } from '../../stores/usageQuery.svelte';
  import { formatTokens } from '../../utils/format';
  import { formatUsageCostOrNull } from '../../utils/usageDisplay';
  import UsageHeatmapGrid from './UsageHeatmapGrid.svelte';
  import { buildHeatmapGrid, type HeatmapColumn, type HeatmapCell, type UsageDayBucket } from './heatmapGrid';

  interface Props {
    /** '' = all providers, else 'claude' | 'codex'. */
    provider: string;
    /** '' = all projects, else a project id. */
    projectId: string;
  }

  let { provider, projectId }: Props = $props();

  const HEATMAP_WEEKS = 26;

  const tzOffsetMinutes = localTzOffsetMinutes();

  // The "now" used to build the query window is stashed here so the
  // grid is built against the SAME instant as the query's fromMillis —
  // calling Date.now() again when building the grid could (rarely)
  // straddle a day boundary and misalign the "today" cell.
  let queryNowMs = $state(Date.now());

  const stats = createUsageStats(() => {
    const currentProvider = provider;
    const currentProjectId = projectId;
    // Read the refresh version so this effect re-runs on turn completion.
    getUsageRefreshVersion();
    // Compute from a LOCAL, then publish to the $state. Reading
    // queryNowMs back here would register it as a dependency of this
    // effect while the write above changes it every run — an infinite
    // effect loop (each rerun lands in a new millisecond, re-invalidates,
    // and reruns; 100% CPU with the modal open).
    const nowMs = Date.now();
    queryNowMs = nowMs;
    const fromMillis = nowMs - HEATMAP_WEEKS * 7 * 24 * 60 * 60 * 1000;
    return new UsageQuery({
      groupBy: 'day',
      fromMillis,
      provider: currentProvider,
      projectId: currentProjectId,
      tzOffsetMinutes,
    });
  });

  // Output tokens only, like every AO usage surface's token count: input
  // re-bills the growing context each turn and would drown the produced
  // work. The grid plots whichever axis it is handed (see UsageDayBucket).
  let days: UsageDayBucket[] = $derived(
    (stats.buckets ?? []).map((b) => ({
      bucket: b.bucket,
      costUsd: b.costUsd,
      tokens: b.outputTokens,
      unpricedRows: b.unpricedRows,
    })),
  );

  let grid: HeatmapColumn[] = $derived(buildHeatmapGrid(days, queryNowMs, HEATMAP_WEEKS));

  function tooltipFor(cell: HeatmapCell): string {
    const dateLabel = `${cell.monthShort} ${cell.dayOfMonth}`;
    const tokenLabel = `${formatTokens(cell.tokens)} tok`;
    const costLabel = formatUsageCostOrNull(cell.costUsd, cell.unpricedRows);
    return costLabel ? `${dateLabel} · ${tokenLabel} · ${costLabel}` : `${dateLabel} · ${tokenLabel}`;
  }
</script>

<div data-testid="usage-heatmap">
  <UsageHeatmapGrid columns={grid} tooltip={tooltipFor} cellTestId="usage-heatmap-cell" />
</div>
