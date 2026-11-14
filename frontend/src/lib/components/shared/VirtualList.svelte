<script lang="ts" generics="T">
  // Fixed-height row virtualization. Renders only the rows visible in the
  // current viewport plus a small overscan buffer; everything else is
  // reserved by a spacer div so scrolling still behaves correctly. This
  // is the simplest form of virtualization — it trades flexibility (all
  // rows must agree on a height) for predictability.
  //
  // Usage:
  //
  //   <VirtualList items={rows} rowHeight={48}>
  //     {#snippet children(item, index)}
  //       <ThreadRow {thread} />
  //     {/snippet}
  //   </VirtualList>
  //
  // A row fills its `rowHeight` wrapper (`h-full`). Rows are keyed by
  // `key`, else by the item's `id`, else by index. The row holding
  // keyboard focus stays mounted when it scrolls out of the window, so
  // focus is never dropped by scrolling.

  import type { Snippet } from 'svelte';

  let {
    items,
    rowHeight,
    overscan = 6,
    ariaLabel,
    role = 'list',
    class: extraClass = '',
    children,
    key,
    viewportRef = $bindable(undefined),
  }: {
    items: T[];
    rowHeight: number;
    overscan?: number;
    ariaLabel?: string;
    role?: string;
    class?: string;
    children?: Snippet<[T, number]>;
    key?: (item: T, index: number) => string | number;
    viewportRef?: HTMLDivElement | undefined;
  } = $props();

  let scrollTop = $state(0);
  let clientHeight = $state(0);
  let internalRef: HTMLDivElement | undefined = $state(undefined);

  // Keep the bindable viewportRef in sync with our internal one so
  // callers can scroll programmatically.
  $effect(() => {
    viewportRef = internalRef;
  });

  // Visible window: how many rows fit, plus overscan on each side.
  // The window never starts past the last full window: a list that
  // shrinks under a scrolled reader shows its end at once, without waiting
  // for the browser's clamped scroll event.
  let visibleCount = $derived(Math.ceil(clientHeight / rowHeight) + overscan * 2);
  let startIndex = $derived(Math.max(0, Math.min(Math.floor(scrollTop / rowHeight) - overscan, items.length - visibleCount)));
  let endIndex = $derived(Math.min(items.length, startIndex + visibleCount));
  let offsetY = $derived(startIndex * rowHeight);
  let totalHeight = $derived(items.length * rowHeight);

  function rowKey(item: T, index: number): string | number {
    return key ? key(item, index) : keyOf(item, index);
  }

  // The key of the row that holds focus, while one does.
  let focusedKey: string | number | undefined = $state(undefined);
  let focusedIndex = $derived.by(() => {
    const wanted = focusedKey;
    return wanted === undefined ? -1 : items.findIndex((item, index) => rowKey(item, index) === wanted);
  });

  // A focused row that left the list was removed without a focusout.
  $effect(() => {
    if (focusedKey !== undefined && focusedIndex < 0) focusedKey = undefined;
  });

  // The window's rows in index order, with the focused row before or
  // after them when it is outside the window.
  let rows = $derived.by(() => {
    const out: { item: T; index: number; pinned: boolean }[] = [];
    if (focusedIndex >= 0 && focusedIndex < startIndex) {
      out.push({ item: items[focusedIndex], index: focusedIndex, pinned: true });
    }
    for (let index = startIndex; index < endIndex; index += 1) {
      out.push({ item: items[index], index, pinned: false });
    }
    if (focusedIndex >= endIndex) {
      out.push({ item: items[focusedIndex], index: focusedIndex, pinned: true });
    }
    return out;
  });

  function handleFocusIn(e: FocusEvent): void {
    const row = (e.target as Element).closest<HTMLElement>('[data-row-index]');
    const index = Number(row?.dataset.rowIndex ?? -1);
    focusedKey = index >= 0 && index < items.length ? rowKey(items[index], index) : undefined;
  }

  function handleFocusOut(e: FocusEvent): void {
    const container = e.currentTarget as HTMLElement;
    // Focus moving to another row is followed by its focusin. A window
    // that lost focus still has the row as its active element and gives
    // focus back to it.
    if (e.relatedTarget instanceof Node && container.contains(e.relatedTarget)) return;
    if (!document.hasFocus() && container.contains(document.activeElement)) return;
    focusedKey = undefined;
  }

  function handleScroll(e: Event): void {
    const target = e.currentTarget as HTMLDivElement;
    scrollTop = target.scrollTop;
  }

  $effect(() => {
    if (!internalRef) return;
    // Observe size changes of the viewport. ResizeObserver fires on
    // mount too, so we don't need a manual initial measurement.
    const observer = new ResizeObserver((entries) => {
      for (const entry of entries) {
        clientHeight = entry.contentRect.height;
      }
    });
    observer.observe(internalRef);
    return () => observer.disconnect();
  });
</script>

<div
  bind:this={internalRef}
  onscroll={handleScroll}
  class={`relative overflow-y-auto ${extraClass}`}
  {role}
  aria-label={ariaLabel}
  data-testid="virtual-list-viewport"
>
  <!-- Spacer keeps the scroll height correct so the native scrollbar
       reflects full content length. -->
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    style={`height: ${totalHeight}px; position: relative;`}
    data-testid="virtual-list-spacer"
    onfocusin={handleFocusIn}
    onfocusout={handleFocusOut}
  >
    <!-- The window's rows are in flow under a pad of the rows above them.
         A pinned row is out of flow at its own offset. -->
    <div style={`height: ${offsetY}px;`} aria-hidden="true"></div>
    {#each rows as row (rowKey(row.item, row.index))}
      <div
        style={row.pinned
          ? `position: absolute; top: ${row.index * rowHeight}px; left: 0; right: 0; height: ${rowHeight}px;`
          : `height: ${rowHeight}px;`}
        role={role === 'list' ? 'listitem' : undefined}
        aria-setsize={role === 'list' ? items.length : undefined}
        aria-posinset={role === 'list' ? row.index + 1 : undefined}
        data-testid="virtual-list-row"
        data-row-index={row.index}
      >
        {@render children?.(row.item, row.index)}
      </div>
    {/each}
  </div>
</div>

<script lang="ts" module>
  // Build a stable key for each row. We look for an `id` field on the
  // item first (the common Thread/Item/Message shape), falling back to
  // the absolute index when no id exists. The outer `startIndex +
  // windowIdx` ensures index-based keys stay unique even as the visible
  // window slides.
  function keyOf(item: unknown, index: number): string | number {
    if (item && typeof item === 'object' && 'id' in item) {
      const id = (item as { id: unknown }).id;
      if (typeof id === 'string' || typeof id === 'number') return id;
    }
    return index;
  }
</script>
