<script lang="ts">
  // The virtualized catalogue. Owns nothing but presentation: the filtered
  // rows, the selection and the roving cursor all arrive as props from
  // SessionImportModal, which is where the keyboard handler for the whole
  // surface lives.
  //
  // The list carries its own `max-h` and its own scroller. Modal's body is
  // `overflow-y-auto` too, so without a bound here the body would grow to the
  // panel's max height and scroll the toolbar out of view — two nested
  // scrollers fighting over one gesture.

  import VirtualList from '../shared/VirtualList.svelte';
  import SessionImportRow from './SessionImportRow.svelte';
  import type { ImportRowResult, ImportableSession } from '../../types/sessionImport';

  interface Props {
    /** DOM id — the search box points `aria-controls` at it. */
    id: string;
    rows: ImportableSession[];
    selection: ReadonlySet<string>;
    /** Index into `rows`; -1 when there is nothing to point at. */
    activeIndex: number;
    /** Row DOM id under the cursor; the modal owns the id math. */
    activeDescendant: string | undefined;
    /**
     * One row's outcome in the current run, or undefined outside one. An
     * accessor rather than a map: the store mutates its results map in place
     * and gates reads on a version counter, so calling this inside the row
     * snippet is what subscribes the row to its own stamp.
     */
    resultFor: (id: string) => ImportRowResult | undefined;
    /** A run is in flight: rows stay visible but stop taking input. */
    disabled: boolean;
    /** Prefix the row DOM ids share with `activeDescendant`. */
    idPrefix: string;
    onToggle: (id: string) => void;
  }

  let {
    id,
    rows,
    selection,
    activeIndex,
    activeDescendant,
    resultFor,
    disabled,
    idPrefix,
    onToggle,
  }: Props = $props();

  // Fixed-height virtualization: VirtualList reserves `rows.length *
  // ROW_HEIGHT` of spacer, so this constant and the row's `h-full` are one
  // contract. 44px is the single-line row height.
  const ROW_HEIGHT = 44;

  let viewport: HTMLDivElement | undefined = $state(undefined);

  // Keep the roving cursor on screen. Fixed row height means the target
  // offset is arithmetic — no per-row measurement, and no dependency on the
  // row being rendered (it may still be outside the virtual window).
  //
  // Reads of `scrollTop`/`clientHeight` are DOM reads, not reactive ones, so
  // this only re-runs when the cursor or the viewport actually changes.
  $effect(() => {
    const index = activeIndex;
    const el = viewport;
    if (!el || index < 0) return;
    const top = index * ROW_HEIGHT;
    const bottom = top + ROW_HEIGHT;
    if (top < el.scrollTop) {
      el.scrollTop = top;
    } else if (bottom > el.scrollTop + el.clientHeight) {
      el.scrollTop = bottom - el.clientHeight;
    }
  });
</script>

<div
  {id}
  role="listbox"
  aria-multiselectable="true"
  aria-label="Importable provider sessions"
  aria-activedescendant={activeDescendant}
  aria-disabled={disabled ? 'true' : undefined}
  tabindex="0"
  data-testid="session-import-list"
  class={[
    'min-h-0 focus-visible:outline-none',
    disabled ? 'pointer-events-none opacity-60' : '',
  ].join(' ')}
>
  <VirtualList
    items={rows}
    rowHeight={ROW_HEIGHT}
    overscan={8}
    role="presentation"
    class="max-h-[60vh]"
    bind:viewportRef={viewport}
  >
    {#snippet children(row, index)}
      <SessionImportRow
        {row}
        domId={`${idPrefix}-${row.id}`}
        selected={selection.has(row.id)}
        active={index === activeIndex}
        result={resultFor(row.id)}
        {disabled}
        onToggle={() => onToggle(row.id)}
      />
    {/snippet}
  </VirtualList>
</div>
