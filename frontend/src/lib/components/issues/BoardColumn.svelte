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
  //
  // While a card is carried (BoardScreen), the column under it says
  // whether it can take the card (`dropNote`), its own column draws the
  // drop slot as one line moved by a transform (`slot`), and the carry
  // reads the column's geometry through `carryColumn`, which asks the
  // virtualizer, not the DOM, where a slot falls.
  import { untrack } from 'svelte';
  import LongListVirtualizer, { type LongListHandle } from '../virtual/LongListVirtualizer.svelte';
  import BoardCard from './BoardCard.svelte';
  import StatusMark from './StatusMark.svelte';
  import type { RowView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import type { ScreenColumn } from '../../stores/trackerBoardModel';
  import type { CardField } from '../../stores/issuesPane.svelte';
  import type { TrackerBoardRef } from '../../stores/trackerBoard.svelte';
  import type { CardEditField } from './ticketEditors';
  import type { CarryColumn } from './boardCarry';
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
    /** The header's count (`columnCount`). */
    count: string;
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
    boardRef: TrackerBoardRef;
    /** The tracker has issue types (not Linear). */
    typed: boolean;
    compact: boolean;
    /** The issue being carried, null for none. */
    carriedIssue: string | null;
    /** The card whose field editor is open, and the field. */
    editing: { readonly issue: string; readonly field: CardEditField } | null;
    /** What the column says while a card is carried over it; null for
     * nothing. */
    dropNote: string | null;
    /** The drop slot in the carried card's own column (an index into
     * `rows`), null for none. */
    slot: number | null;
    onOpenCard: (row: RowView) => void;
    onCardMenu: (row: RowView, event: MouseEvent) => void;
    onEditCard: (row: RowView, field: CardEditField | null) => void;
    onMoveCard: (row: RowView, anchor: HTMLElement) => void;
  }

  let {
    column, rows, count, filtered, unit, hidden, now, paging, failed, onNearEnd, shown, wide = false, openIssue,
    boardRef, typed, compact, carriedIssue, editing, dropNote, slot, onOpenCard, onCardMenu, onEditCard, onMoveCard,
  }: Props = $props();

  const estimate: RowEstimate = { at: () => CARD_ESTIMATE_PX, isExact: () => false };
  const getKey = (row: RowView) => row.card.id;

  let scrollEl: HTMLElement | undefined = $state();
  let listRef: LongListHandle | undefined = $state();

  let sectionEl: HTMLElement | undefined = $state();

  /** The scroll offset of the gap before row `index` (`rows.length`: after
   * the last), or null when the virtualizer does not hold that row. */
  function slotOffset(index: number): number | null {
    const ref = listRef;
    if (!ref || rows.length === 0) return null;
    if (index < rows.length) return ref.holds(index) ? ref.getItemOffset(index) : null;
    const last = rows.length - 1;
    if (!ref.holds(last)) return null;
    const el = scrollEl?.querySelector<HTMLElement>(`[data-issue="${CSS.escape(rows[last].card.id)}"]`);
    return ref.getItemOffset(last) + (el?.offsetHeight ?? CARD_ESTIMATE_PX);
  }

  /** The column as a carry tests it, read once at pick-up. */
  export function carryColumn(): CarryColumn {
    const rect = sectionEl!.getBoundingClientRect();
    const cardsRect = scrollEl!.getBoundingClientRect();
    return {
      rect,
      cardsRect,
      slotAt(clientY) {
        const ref = listRef;
        if (!ref || !scrollEl || rows.length === 0) return null;
        const y = clientY - cardsRect.top + scrollEl.scrollTop;
        const i = Math.min(ref.findItemIndex(y), rows.length - 1);
        const top = slotOffset(i);
        const bottom = slotOffset(i + 1);
        if (top === null || bottom === null) return i;
        return y > (top + bottom) / 2 ? i + 1 : i;
      },
      scrollBy(dy) {
        if (scrollEl) scrollEl.scrollTop += dy;
      },
    };
  }

  // The slot's line, placed in the gap above the card it goes before.
  const slotY = $derived(slot === null ? null : slotOffset(slot));

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

</script>

<section
  bind:this={sectionEl}
  class="relative flex min-h-0 flex-col {wide ? 'min-w-0 flex-1' : 'w-72 shrink-0'}"
  aria-label={column.name}
  data-testid="issues-column"
  data-column={column.name}
  data-drop={dropNote !== null ? '' : undefined}
>
  {#if !wide}
    <header class="flex min-w-0 items-center gap-1.5 px-1 pb-2 text-sm">
      <StatusMark group={column.statusGroups[0]} />
      <span class="truncate font-medium text-fg">{column.name}</span>
      <span class="ml-auto shrink-0 tabular-nums text-fg-muted" data-testid="issues-column-count">{count}</span>
    </header>
  {/if}
  <div
    bind:this={scrollEl}
    class="relative min-h-0 flex-1 overflow-y-auto px-1 focus:outline-none"
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
          <BoardCard
            {row}
            {boardRef}
            {hidden}
            {unit}
            {typed}
            {compact}
            {now}
            open={row.card.id === openIssue}
            carried={row.card.id === carriedIssue}
            editing={editing?.issue === row.card.id ? editing.field : null}
            onOpen={onOpenCard}
            onMenu={onCardMenu}
            onEdit={onEditCard}
            onMove={onMoveCard}
          />
        {/snippet}
      </LongListVirtualizer>
    {/if}
    {#if slotY !== null}
      <div class="pointer-events-none absolute left-1 right-1 top-0 h-0.5 rounded-full bg-accent" style:transform="translateY({slotY - 4}px)" data-testid="issues-column-slot"></div>
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
  {#if dropNote !== null}
    <div class="pointer-events-none absolute inset-x-0 bottom-0 top-8 z-[2] flex items-start justify-center rounded-[var(--radius-field)] border border-dashed border-border bg-surface-0/80 p-3" data-testid="issues-column-drop-note">
      <p class="text-center text-sm text-fg-muted" role="status">{dropNote}</p>
    </div>
  {/if}
</section>
