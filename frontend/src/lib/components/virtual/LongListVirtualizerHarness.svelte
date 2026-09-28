<script module lang="ts">
  // Test fixture for LongListVirtualizer's browser suite: a fixed-size
  // scroller over synthetic rows whose rendered height may differ from
  // their estimate, so moves of the held range can be driven by scrolling,
  // jumps, data changes and measurement.
  export interface LongListRow {
    id: string;
    index: number;
    heightPx: number;
    /** What the estimate reports; exact when equal to heightPx. */
    estimatePx: number;
  }
</script>

<script lang="ts">
  import LongListVirtualizer, { type LongListHandle } from './LongListVirtualizer.svelte';
  import type { HeldLimits } from '../../utils/virtual/heldRows';
  import type { RowEstimate } from '../../utils/virtual/types';

  interface Props {
    initialRows: LongListRow[];
    limits?: HeldLimits;
    viewportPx?: number;
  }

  let { initialRows, limits, viewportPx = 600 }: Props = $props();

  // svelte-ignore state_referenced_locally -- seed copy by design; the
  // fixture owns the rows after mount (setRows).
  let rows = $state.raw<LongListRow[]>(initialRows);
  let scrollEl: HTMLElement | undefined = $state();
  let listRef: LongListHandle | undefined = $state();

  const estimate: RowEstimate = {
    at: (index) => rows[index]?.estimatePx ?? 100,
    isExact: (index) => rows[index] !== undefined && rows[index].estimatePx === rows[index].heightPx,
  };

  export function setRows(next: LongListRow[]): void {
    rows = next;
  }

  export function handle(): LongListHandle | undefined {
    return listRef;
  }

  function applyScrollTarget(top: number): void {
    if (scrollEl) scrollEl.scrollTop = top;
  }
</script>

<div style="position: fixed; top: 0; left: 0; width: 800px; height: {viewportPx}px;">
  <div
    bind:this={scrollEl}
    data-testid="long-scroll"
    style="height: 100%; box-sizing: border-box; overflow-y: auto; overflow-anchor: none;"
  >
    <LongListVirtualizer
      bind:this={listRef}
      data={rows}
      getKey={(row) => row.id}
      scrollRef={scrollEl}
      {estimate}
      {limits}
      {applyScrollTarget}
      onCompensation={(compensation) => applyScrollTarget(compensation.target)}
    >
      {#snippet children(row: LongListRow)}
        <div data-row-id={row.id} data-row-index={row.index} style="height: {row.heightPx}px;">{row.id}</div>
      {/snippet}
    </LongListVirtualizer>
  </div>
</div>
