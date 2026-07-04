<script lang="ts">
  // GitHub-style contribution heatmap: last 26 Sunday-start weeks,
  // Sun..Sat rows, one cell per day. ALL date math lives in heatmapGrid.ts — this
  // component only fetches the day buckets and renders the cells it's
  // handed. Independent of the footer/modal period selector by design
  // (always the fixed 26-week window); only the provider filter and
  // usageRefresh bumps trigger a refetch.
  //
  // Cell color is a single-hue sequential ramp derived from the app's
  // --accent token via color-mix (see the component-level style block
  // below) so the ramp tracks the light/dark theme automatically
  // instead of hardcoding hex steps per mode.

  import { UsageQuery } from '../../stores/bindings';
  import { getUsageRefreshVersion } from '../../stores/usageRefresh.svelte';
  import { createUsageStats, localTzOffsetMinutes } from '../../stores/usageQuery.svelte';
  import { formatTokens } from '../../utils/format';
  import { formatUsageCostOrNull } from '../../utils/usageDisplay';
  import { buildHeatmapGrid, type HeatmapColumn, type HeatmapCell, type HeatmapLevel } from './heatmapGrid';

  interface Props {
    /** '' = all providers, else 'claude' | 'codex'. */
    provider: string;
    /** '' = all projects, else a project id. */
    projectId: string;
  }

  let { provider, projectId }: Props = $props();

  const HEATMAP_WEEKS = 26;
  const CELL_PX = 11;
  const GAP_PX = 3;
  const LABEL_WIDTH_PX = 26;
  // Rows are Sun..Sat; label Mon/Wed/Fri (rows 1/3/5), GitHub-style.
  const WEEKDAY_LABELS = ['', 'Mon', '', 'Wed', '', 'Fri', ''];
  const LEVELS: readonly HeatmapLevel[] = [0, 1, 2, 3, 4];

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

  let grid: HeatmapColumn[] = $derived(buildHeatmapGrid(stats.buckets ?? [], queryNowMs, HEATMAP_WEEKS));

  function heatClass(level: HeatmapLevel): string {
    return `heat-${level}`;
  }

  function tooltipFor(cell: HeatmapCell): string {
    const dateLabel = `${cell.monthShort} ${cell.dayOfMonth}`;
    const tokenLabel = `${formatTokens(cell.tokens)} tok`;
    const costLabel = formatUsageCostOrNull(cell.costUsd, cell.unpricedRows);
    return costLabel ? `${dateLabel} · ${tokenLabel} · ${costLabel}` : `${dateLabel} · ${tokenLabel}`;
  }
</script>

<!-- The grid block has a fixed intrinsic width (26 week columns), so
     items-center centers it in whatever width the modal gives it; the
     legend sits centered underneath on the same axis. -->
<div class="flex flex-col items-center gap-1.5" data-testid="usage-heatmap">
  <div class="flex flex-col gap-1.5">
    <div class="flex" style="padding-left: {LABEL_WIDTH_PX + GAP_PX}px; gap: {GAP_PX}px">
      {#each grid as column (column.weekStartKey)}
        <div
          class="text-[0.625rem] leading-none text-fg-subtle whitespace-nowrap"
          style="width: {CELL_PX}px"
        >
          {column.monthLabel ?? ''}
        </div>
      {/each}
    </div>
    <div class="flex" style="gap: {GAP_PX}px">
      <div class="flex flex-col shrink-0" style="gap: {GAP_PX}px; width: {LABEL_WIDTH_PX}px">
        {#each WEEKDAY_LABELS as label, i (i)}
          <div
            class="flex items-center text-[0.625rem] leading-none text-fg-subtle"
            style="height: {CELL_PX}px"
          >
            {label}
          </div>
        {/each}
      </div>
      <div class="flex" style="gap: {GAP_PX}px">
        {#each grid as column (column.weekStartKey)}
          <div class="flex flex-col" style="gap: {GAP_PX}px">
            {#each column.cells as cell (cell.dateKey)}
              <div
                class="rounded-[3px] {heatClass(cell.level)} {cell.isFuture ? 'invisible' : ''}"
                style="width: {CELL_PX}px; height: {CELL_PX}px"
                title={cell.isFuture ? undefined : tooltipFor(cell)}
                data-testid="usage-heatmap-cell"
                data-date={cell.dateKey}
                data-level={cell.level}
              ></div>
            {/each}
          </div>
        {/each}
      </div>
    </div>
  </div>
  <div class="flex items-center gap-1 text-[0.625rem] leading-none text-fg-subtle">
    <span>Less</span>
    {#each LEVELS as level (level)}
      <div class="rounded-[3px] {heatClass(level)}" style="width: {CELL_PX}px; height: {CELL_PX}px"></div>
    {/each}
    <span>More</span>
  </div>
</div>

<style>
  /* Single-hue sequential ramp over the app's accent token. Level 0 is
     the base surface (no data / zero); levels 1-4 step through
     increasing accent mix so the ramp stays perceptibly distinct in
     both themes without hardcoding hex per mode. */
  .heat-0 {
    background-color: var(--surface-2);
  }
  .heat-1 {
    background-color: color-mix(in oklab, var(--accent) 22%, var(--surface-2));
  }
  .heat-2 {
    background-color: color-mix(in oklab, var(--accent) 44%, var(--surface-2));
  }
  .heat-3 {
    background-color: color-mix(in oklab, var(--accent) 66%, var(--surface-2));
  }
  .heat-4 {
    background-color: color-mix(in oklab, var(--accent) 88%, var(--surface-2));
  }
</style>
