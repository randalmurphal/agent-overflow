<script lang="ts">
  // Presentation-only contribution grid: renders the columns it is handed
  // and owns nothing else. Two callers with different data sources share
  // it — UsageHeatmap.svelte (AO's own ledger) and UsageCodexAccount.svelte
  // (Codex's account-wide report) — so the two grids can never drift apart
  // visually or diverge on cell/legend geometry.
  //
  // Cell color is a single-hue sequential ramp derived from the app's
  // --accent token via color-mix (see the style block below) so the ramp
  // tracks the light/dark theme automatically instead of hardcoding hex
  // steps per mode.

  import type { HeatmapColumn, HeatmapCell, HeatmapLevel } from './heatmapGrid';

  interface Props {
    columns: HeatmapColumn[];
    /** Hover text for a non-future cell. */
    tooltip: (cell: HeatmapCell) => string;
    /** data-testid for the cells, so each caller's grid is addressable. */
    cellTestId: string;
  }

  let { columns, tooltip, cellTestId }: Props = $props();

  const CELL_PX = 11;
  const GAP_PX = 3;
  const LABEL_WIDTH_PX = 26;
  // Rows are Sun..Sat; label Mon/Wed/Fri (rows 1/3/5), GitHub-style.
  const WEEKDAY_LABELS = ['', 'Mon', '', 'Wed', '', 'Fri', ''];
  const LEVELS: readonly HeatmapLevel[] = [0, 1, 2, 3, 4];

  function heatClass(level: HeatmapLevel): string {
    return `heat-${level}`;
  }
</script>

<!-- The grid block has a fixed intrinsic width (one column per week), so
     items-center centers it in whatever width the modal gives it; the
     legend sits centered underneath on the same axis. -->
<div class="flex flex-col items-center gap-1.5">
  <div class="flex flex-col gap-1.5">
    <div class="flex" style="padding-left: {LABEL_WIDTH_PX + GAP_PX}px; gap: {GAP_PX}px">
      {#each columns as column (column.weekStartKey)}
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
        {#each columns as column (column.weekStartKey)}
          <div class="flex flex-col" style="gap: {GAP_PX}px">
            {#each column.cells as cell (cell.dateKey)}
              <div
                class="rounded-[3px] {heatClass(cell.level)} {cell.isFuture ? 'invisible' : ''}"
                style="width: {CELL_PX}px; height: {CELL_PX}px"
                title={cell.isFuture ? undefined : tooltip(cell)}
                data-testid={cellTestId}
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
