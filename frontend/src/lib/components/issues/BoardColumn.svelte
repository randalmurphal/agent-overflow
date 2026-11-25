<script lang="ts">
  // One column of the Board screen: its name, count and estimate sum, and
  // its cards, virtualized so only the cards near the viewport are mounted.
  // A column the computer holds more of asks for its next page as the
  // reader nears the end of the rows held (`onNearEnd`); while Only mine or
  // Search filters it, the rest is read whole (the pane asks) and the
  // header says how much is loaded. It says it is loading only while one
  // of its pages is being read (`paging`), and that a read failed until
  // the next event or Refresh reads it again. A column the board's first
  // read did not carry (`column.unread`) says so until its first page is
  // read.
  import { untrack } from 'svelte';
  import LongListVirtualizer, { type LongListHandle } from '../virtual/LongListVirtualizer.svelte';
  import BoardCard from './BoardCard.svelte';
  import StatusMark from './StatusMark.svelte';
  import { columnCount } from './boardFormat';
  import type { RowView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import type { ScreenColumn } from '../../stores/trackerBoardModel';
  import type { CardField } from '../../stores/issuesPane.svelte';
  import type { RowEstimate } from '../../utils/virtual/types';

  const IS_TEST = import.meta.env.MODE === 'test'
    && typeof window !== 'undefined' && 'happyDOM' in window;

  const CARD_ESTIMATE_PX = 104;
  // Rows from the end of the held rows at which the next page is asked for.
  const NEAR_END_ROWS = 15;

  interface Props {
    column: ScreenColumn;
    /** The cards shown: the column's rows, filtered. */
    rows: readonly RowView[];
    filtered: boolean;
    unit: string;
    hidden: ReadonlySet<CardField>;
    now: number;
    /** A page of this column is being read. */
    paging: boolean;
    /** A page read of this column failed, until the next event or Refresh
     * reads it again. */
    failed: boolean;
    onNearEnd: () => void;
    /** False while the column is not on screen. */
    shown: boolean;
    /** Fills the pane (compact) rather than a fixed width. */
    wide?: boolean;
    /** The issue whose ticket is open, null for none. */
    openIssue: string | null;
    onOpenCard: (row: RowView) => void;
    onCardMenu: (row: RowView, event: MouseEvent) => void;
  }

  let { column, rows, filtered, unit, hidden, now, paging, failed, onNearEnd, shown, wide = false, openIssue, onOpenCard, onCardMenu }: Props = $props();

  const estimate: RowEstimate = { at: () => CARD_ESTIMATE_PX, isExact: () => false };
  const getKey = (row: RowView) => row.card.id;

  let scrollEl: HTMLElement | undefined = $state();
  let listRef: LongListHandle | undefined = $state();

  function applyScrollTarget(top: number): void {
    if (scrollEl) scrollEl.scrollTop = top;
  }

  const more = $derived(column.moreFrom !== null);

  function check(): void {
    if (!more || filtered || paging || !shown) return;
    const ref = listRef;
    if (!ref || rows.length === 0) {
      onNearEnd();
      return;
    }
    const viewport = ref.getViewportSize();
    if (viewport <= 0) return;
    const last = ref.findItemIndex(ref.getScrollOffset() + viewport - 1);
    if (last >= rows.length - NEAR_END_ROWS) onNearEnd();
  }

  // The engine places its window at the tail until its first scroll
  // input; a column opens at its top. A column shown again (a compact
  // screen back) re-reads its viewport.
  $effect(() => {
    const ref = listRef;
    const on = shown;
    if (!ref) return;
    untrack(() => {
      if (on) ref.revalidate();
      check();
    });
  });
  $effect(() => {
    void rows;
    void more;
    void paging;
    untrack(check);
  });
  $effect(() => {
    const ref = listRef;
    if (!ref) return;
    return ref.subscribeContentGeometry(() => untrack(check));
  });

  const countText = $derived(columnCount(column, rows.length, filtered, unit));
</script>

<section
  class="flex min-h-0 flex-col {wide ? 'min-w-0 flex-1' : 'w-72 shrink-0'}"
  aria-label={column.name}
  data-testid="issues-column"
  data-column={column.name}
>
  {#if !wide}
    <header class="flex min-w-0 items-center gap-1.5 px-1 pb-2 text-sm">
      <StatusMark group={column.statusGroups[0]} />
      <span class="truncate font-medium text-fg">{column.name}</span>
      <span class="ml-auto shrink-0 tabular-nums text-fg-muted" data-testid="issues-column-count">{countText}</span>
    </header>
  {/if}
  <div
    bind:this={scrollEl}
    class="min-h-0 flex-1 overflow-y-auto px-1 focus:outline-none"
    style:overflow-anchor="none"
    tabindex="-1"
    data-pane-scroller
    data-testid="issues-column-cards"
  >
    {#if rows.length > 0}
      <LongListVirtualizer
        bind:this={listRef}
        data={rows}
        {getKey}
        scrollRef={scrollEl}
        {estimate}
        renderAll={IS_TEST}
        {applyScrollTarget}
        onscroll={check}
      >
        {#snippet children(row: RowView)}
          <BoardCard {row} {hidden} {unit} {now} open={row.card.id === openIssue} onOpen={onOpenCard} onMenu={onCardMenu} />
        {/snippet}
      </LongListVirtualizer>
    {/if}
    {#if paging}
      <p class="px-2 py-2 text-xs text-fg-muted" data-testid="issues-column-loading">{column.unread ? 'Loading…' : 'Loading more…'}</p>
    {:else if failed}
      <p class="px-2 py-2 text-xs text-fg-muted" data-testid="issues-column-failed">{column.unread ? 'Could not read this column.' : 'Could not read more of this column.'}</p>
    {:else if column.unread}
      <p class="px-2 py-2 text-xs text-fg-muted" data-testid="issues-column-unread">Not read yet</p>
    {:else if rows.length === 0}
      <p class="px-2 py-2 text-xs text-fg-muted" data-testid="issues-column-empty">{!filtered ? 'Nothing here' : more ? 'No matching cards among those loaded' : 'No matching cards'}</p>
    {/if}
  </div>
</section>
