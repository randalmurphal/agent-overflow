<script module lang="ts">
  // Test fixture for TimelineVirtualizer's browser + unit suites: a plain
  // fixed-size scroller with synthetic fixed-height rows and imperative
  // mutation hooks, so the adapter is exercised without MessageTimeline's
  // pane machinery (that integration is the V2 outcome suites' job).
  export interface HarnessRow {
    id: string;
    heightPx: number;
    label: string;
    /** Height of the row's leading block. The trailing block takes the
     * remainder, so a row can grow at its HEAD (pushing its own tail down)
     * rather than uniformly — the shape the straddling-row attribution
     * exists for. Zero means a single undifferentiated block. */
    headPx?: number;
  }
</script>

<script lang="ts">
  import TimelineVirtualizer from './TimelineVirtualizer.svelte';
  import type {
    ContentGeometrySample,
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
    intrinsicViewportMaxHeight?: string;
    headerSize?: number;
    onscroll?: (offset: number) => void;
    onscrollend?: () => void;
    onCompensation?: (compensation: EngineCompensation) => void;
    onContentGeometry?: (sample: ContentGeometrySample) => void;
    trackReadingAnchor?: () => boolean;
    /** Called from a template expression inside the row snippet, so it
     * fires once per snippet-content re-render for that row. The row-reuse
     * tripwire counts these: a projection pass that reuses an unchanged
     * row's object must not re-fire mounted rows. */
    onRowRender?: (id: string) => void;
  }

  let {
    initialRows,
    bufferSize = 400,
    estimate,
    renderAll = false,
    viewportPx = 600,
    intrinsicViewportMaxHeight,
    headerSize = 0,
    onscroll,
    onscrollend,
    onCompensation,
    onContentGeometry,
    trackReadingAnchor,
    onRowRender,
  }: Props = $props();

  // svelte-ignore state_referenced_locally -- seed copy by design; the
  // fixture owns the rows after mount (setRows/resizeRow).
  let rows = $state<HarnessRow[]>(initialRows);
  let scrollEl: HTMLElement | undefined = $state();
  let listRef: TimelineVirtualizerHandle | undefined = $state();
  // svelte-ignore state_referenced_locally -- fixture-owned after mount.
  let currentHeaderSize = $state(headerSize);
  // svelte-ignore state_referenced_locally -- fixture-owned after mount.
  let currentIntrinsicViewportMaxHeight = $state(intrinsicViewportMaxHeight);

  /** Replace the keyed data array; the virtualizer derives the mutation. */
  export function setRows(next: HarnessRow[]): void {
    rows = next;
  }

  export function getRows(): HarnessRow[] {
    return rows;
  }

  /** Change one row's rendered height in place (same key → same DOM). */
  export function resizeRow(id: string, heightPx: number): void {
    rows = rows.map((row) => (row.id === id ? { ...row, heightPx } : row));
  }

  /** Grow a row's LEADING block by `byPx`, pushing its own trailing block
   * down by the same amount (total height grows, tail content unchanged).
   * Models late typesetting landing above the reading position. */
  export function growRowHead(id: string, byPx: number): void {
    rows = rows.map((row) =>
      row.id === id
        ? { ...row, headPx: (row.headPx ?? 0) + byPx, heightPx: row.heightPx + byPx }
        : row,
    );
  }

  export function handle(): TimelineVirtualizerHandle | undefined {
    return listRef;
  }

  export function scroller(): HTMLElement | undefined {
    return scrollEl;
  }

  export function setHeaderSize(next: number): void {
    currentHeaderSize = next;
  }

  export function setIntrinsicViewportMaxHeight(next: string | undefined): void {
    currentIntrinsicViewportMaxHeight = next;
  }

  // The suites exercise the adapter without a scroll controller, so the
  // harness is the "chokepoint" for the required applyScrollTarget prop
  // and writes directly.
  function applyScrollTarget(top: number): void {
    if (scrollEl) scrollEl.scrollTop = top;
  }

  /** Renders as '' — exists so the row snippet carries a template
   * expression whose re-execution the reuse tripwire can count. */
  function tapRowRender(id: string): string {
    onRowRender?.(id);
    return '';
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
  style:height={currentIntrinsicViewportMaxHeight === undefined ? '100%' : 'auto'}
  style="box-sizing: border-box; overflow-y: auto; overflow-anchor: none;"
>
  <TimelineVirtualizer
    bind:this={listRef}
    data={rows}
    getKey={(row) => row.id}
    scrollRef={scrollEl}
    intrinsicViewportMaxHeight={currentIntrinsicViewportMaxHeight}
    {bufferSize}
    {estimate}
    {renderAll}
    {onscroll}
    {onscrollend}
    {onCompensation}
    {onContentGeometry}
    {trackReadingAnchor}
    {applyScrollTarget}
    headerSize={currentHeaderSize}
  >
    {#snippet header()}
      <div data-testid="virtual-header" style="height: 100%;">Header</div>
    {/snippet}
    {#snippet children(row: HarnessRow, index: number)}
      <div data-row-index={index} data-row-id={row.id} style="height: {row.heightPx}px;">{tapRowRender(row.id)}
        {#if row.headPx}
          <div data-row-head style="height: {row.headPx}px;">{row.label}</div>
          <div data-row-body style="height: {row.heightPx - row.headPx}px;"></div>
        {:else}
          {row.label}
        {/if}
      </div>
    {/snippet}
  </TimelineVirtualizer>
</div>
</div>
