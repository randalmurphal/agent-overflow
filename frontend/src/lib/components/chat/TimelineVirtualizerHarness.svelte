<script module lang="ts">
  // Test fixture for TimelineVirtualizer's browser + unit suites: a plain
  // fixed-size scroller with synthetic fixed-height rows and imperative
  // mutation hooks, so the adapter is exercised without MessageTimeline's
  // pane machinery (that integration is the V2 outcome suites' job).
  export interface HarnessRow {
    id: string;
    heightPx: number;
    label: string;
  }
</script>

<script lang="ts">
  import TimelineVirtualizer from './TimelineVirtualizer.svelte';
  import type {
    EngineCompensation,
    RowEstimate,
    TimelineVirtualizerHandle,
  } from '../../utils/virtual/types';

  interface Props {
    initialRows: HarnessRow[];
    bufferSize?: number;
    estimate?: RowEstimate;
    renderAll?: boolean;
    viewportPx?: number;
    onscroll?: (offset: number) => void;
    onscrollend?: () => void;
    onCompensation?: (compensation: EngineCompensation) => void;
  }

  let {
    initialRows,
    bufferSize = 400,
    estimate,
    renderAll = false,
    viewportPx = 600,
    onscroll,
    onscrollend,
    onCompensation,
  }: Props = $props();

  // svelte-ignore state_referenced_locally -- seed copy by design; the
  // fixture owns the rows after mount (setRows/resizeRow).
  let rows = $state<HarnessRow[]>(initialRows);
  let shift = $state(false);
  let scrollEl: HTMLElement | undefined = $state();
  let listRef: TimelineVirtualizerHandle | undefined = $state();

  /** Replace the data array; `shift: true` marks a head splice. */
  export function setRows(next: HarnessRow[], opts: { shift?: boolean } = {}): void {
    shift = opts.shift ?? false;
    rows = next;
  }

  export function getRows(): HarnessRow[] {
    return rows;
  }

  /** Change one row's rendered height in place (same key → same DOM). */
  export function resizeRow(id: string, heightPx: number): void {
    shift = false;
    rows = rows.map((row) => (row.id === id ? { ...row, heightPx } : row));
  }

  export function handle(): TimelineVirtualizerHandle | undefined {
    return listRef;
  }

  export function scroller(): HTMLElement | undefined {
    return scrollEl;
  }
</script>

<!-- The fixed viewport lives on the HOST, not the scroller: a
     position:fixed scroller has offsetParent === null, which the RO's
     display:none guard (ported from upstream) would skip. This mirrors
     the app topology (fixed test host > normal-flow scroll container). -->
<div
  style="position: fixed; top: 0; left: 0; width: 800px; height: {viewportPx}px; background: #111;"
>
<div
  bind:this={scrollEl}
  data-testid="virt-scroll"
  style="height: 100%; overflow-y: auto; overflow-anchor: none;"
>
  <TimelineVirtualizer
    bind:this={listRef}
    data={rows}
    getKey={(row) => row.id}
    scrollRef={scrollEl}
    {bufferSize}
    {estimate}
    {renderAll}
    {shift}
    {onscroll}
    {onscrollend}
    {onCompensation}
  >
    {#snippet children(row: HarnessRow, index: number)}
      <div data-row-index={index} data-row-id={row.id} style="height: {row.heightPx}px;">
        {row.label}
      </div>
    {/snippet}
  </TimelineVirtualizer>
</div>
</div>
