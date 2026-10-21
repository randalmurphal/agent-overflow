<script lang="ts" generics="R extends { key: string }">
  import { onDestroy, untrack, type Snippet } from 'svelte';
  import LongListVirtualizer, { type LongListHandle } from '../virtual/LongListVirtualizer.svelte';
  import PullsGroupHeader from './PullsGroupHeader.svelte';
  import PullsProjectError from './PullsProjectError.svelte';
  import { headingIndexOf, type PullsListHeading, type PullsListItem } from './pullsListItems';
  import type { RowEstimate } from '../../utils/virtual/types';

  // The scrolling body of one of the page's lists, virtualized: only the
  // rows near the viewport are mounted, so a list of any length costs the
  // rows on screen. A list that asks (`onVisible`) is told the rows on
  // screen, the virtualizer's viewport range, by key on every scroll,
  // resize and data change, and none while the list is not shown (the
  // compact detail covers it) or once it unmounts. The virtualizer keeps
  // the row at the viewport top in view when the rows move under it (a
  // refresh re-sorts by update time while the reader reads); an order the
  // reader chose (`orderKey`) scrolls the list to its top instead. The
  // heading of
  // the group under the viewport top sticks over the list once its own
  // row has scrolled under the top; the rows' own headings cannot stick,
  // each row being positioned on its own.

  const IS_TEST = import.meta.env.MODE === 'test'
    && typeof window !== 'undefined' && 'happyDOM' in window;

  const HEADING_ESTIMATE_PX = 29;
  const ERROR_ESTIMATE_PX = 80;

  interface Props {
    testid: string;
    items: readonly PullsListItem<R>[];
    /** A row's estimated height; the virtualizer measures the rows it mounts. */
    rowEstimatePx: number;
    /** False while the list is hidden: no row is on screen. */
    shown: boolean;
    /** The order the reader chose; a change scrolls the list to its top. */
    orderKey?: string;
    /** The rows on screen, for a list whose store reads detail for them. */
    onVisible?: (keys: string[]) => void;
    onRetry: () => void;
    retrying: boolean;
    row: Snippet<[R]>;
    /** The empty, loading or error state under the rows. */
    after?: Snippet;
  }

  let { testid, items, rowEstimatePx, shown, orderKey = '', onVisible, onRetry, retrying, row: renderRow, after }: Props = $props();

  // Stable identity for the virtualizer's constructor; reads the current items.
  const estimate: RowEstimate = {
    at: (index) => {
      const item = items[index];
      if (!item || item.kind === 'row') return rowEstimatePx;
      return item.kind === 'heading' ? HEADING_ESTIMATE_PX : ERROR_ESTIMATE_PX;
    },
    isExact: () => false,
  };
  const getKey = (item: PullsListItem<R>) => item.key;

  let scrollEl: HTMLElement | undefined = $state();
  let listRef: LongListHandle | undefined = $state();
  let stuck: PullsListHeading | null = $state(null);
  let reported = '';

  // This list is the only writer of its scrollTop.
  function applyScrollTarget(top: number): void {
    if (scrollEl) scrollEl.scrollTop = top;
  }

  function update(): void {
    const ref = listRef;
    const viewport = ref?.getViewportSize() ?? 0;
    const keys: string[] = [];
    let heading: PullsListHeading | null = null;
    if (ref && shown && items.length > 0 && viewport > 0) {
      const offset = ref.getScrollOffset();
      const first = ref.findItemIndex(offset);
      const last = ref.findItemIndex(offset + viewport - 1);
      for (let index = first; index <= last; index += 1) {
        const item = items[index];
        if (item?.kind === 'row') keys.push(item.row.key);
      }
      const owner = headingIndexOf(items, first);
      const item = items[owner];
      if (item && item.kind !== 'row' && (owner !== first || ref.getItemOffset(first) < offset)) heading = item.heading;
    }
    if (stuck !== heading) stuck = heading;
    if (!onVisible) return;
    const joined = keys.join('\u0000');
    if (joined === reported) return;
    reported = joined;
    onVisible(keys);
  }

  // The engine places its window at the tail until its first scroll
  // input; this list opens at its top. A list shown again after the
  // compact detail hid it re-reads its viewport and position.
  $effect(() => {
    const ref = listRef;
    const on = shown;
    if (!ref) return;
    untrack(() => {
      if (on) ref.revalidate();
      update();
    });
  });
  $effect(() => {
    void items;
    untrack(update);
  });
  // The reader picked another order: the list opens it from the top.
  let shownOrder = untrack(() => orderKey);
  $effect(() => {
    const order = orderKey;
    untrack(() => {
      if (order === shownOrder) return;
      shownOrder = order;
      listRef?.scrollToIndex(0, { align: 'start' });
      update();
    });
  });
  $effect(() => {
    const ref = listRef;
    if (!ref) return;
    // The subscription replays its sample at once, inside this effect.
    return ref.subscribeContentGeometry(() => untrack(update));
  });
  // A list that goes (the tab switches) leaves no row on screen: the
  // store holds what it was last told.
  onDestroy(() => {
    if (reported !== '') onVisible?.([]);
  });
</script>

<div
  bind:this={scrollEl}
  class="min-h-0 flex-1 overflow-y-auto border-t border-border-subtle"
  style:overflow-anchor="none"
  data-testid={testid}
>
  <!-- Takes no height, so the rows' offsets are the scroller's. -->
  {#if stuck}
    <div class="sticky top-0 z-[1] h-0" aria-hidden="true">
      <div class="absolute inset-x-0 top-0">
        <PullsGroupHeader label={stuck.label} count={stuck.count} testid="pulls-list-stuck-heading" />
      </div>
    </div>
  {/if}
  <LongListVirtualizer
    bind:this={listRef}
    data={items}
    {getKey}
    scrollRef={scrollEl}
    {estimate}
    renderAll={IS_TEST}
    {applyScrollTarget}
    onCompensation={(compensation) => applyScrollTarget(compensation.target)}
    onscroll={update}
  >
    {#snippet children(item: PullsListItem<R>)}
      {#if item.kind === 'row'}
        {@render renderRow(item.row)}
      {:else if item.kind === 'heading'}
        <PullsGroupHeader label={item.heading.label} count={item.heading.count} testid={item.heading.testid} />
      {:else}
        <PullsGroupHeader label={item.heading.label} testid={item.heading.testid} />
        <PullsProjectError message={item.error.message} failure={item.error.failure} forge={item.error.forge} cause={item.error.cause} {onRetry} {retrying} />
      {/if}
    {/snippet}
  </LongListVirtualizer>
  {@render after?.()}
</div>
