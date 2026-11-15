<script lang="ts" generics="T">
  // Fixed-height row virtualization. Mounts the rows in the viewport plus
  // `overscan` on each side; a spacer holds the scroll height of every row.
  // All rows share `rowHeight`, and a row fills its wrapper (`h-full`).
  // Rows are keyed by `key`, else by the item's `id`, else by index.
  //
  // Some rows stay mounted outside the window, each at its own offset:
  // the row holding keyboard focus and its two neighbors, so scrolling
  // never drops focus and Tab and Shift+Tab from it reach the rows beside
  // it, and the `keepIndex` row, for a caller that names a row by id
  // (`aria-activedescendant`).

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
    keepIndex,
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
    /** Index of an item that stays mounted outside the window. */
    keepIndex?: number;
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

  // Every mounted row in index order, so the document's tab order is the
  // list's: the kept rows above the window, the window, the kept rows
  // below it.
  let rows = $derived.by(() => {
    const wanted = focusedIndex >= 0 ? [focusedIndex - 1, focusedIndex, focusedIndex + 1] : [];
    if (keepIndex !== undefined) wanted.push(keepIndex);
    const outside = [...new Set(wanted)]
      .filter((index) => index >= 0 && index < items.length && (index < startIndex || index >= endIndex))
      .sort((a, b) => a - b);
    const out: { item: T; index: number; kept: boolean }[] = [];
    for (const index of outside) {
      if (index < startIndex) out.push({ item: items[index], index, kept: true });
    }
    for (let index = startIndex; index < endIndex; index += 1) {
      out.push({ item: items[index], index, kept: false });
    }
    for (const index of outside) {
      if (index >= endIndex) out.push({ item: items[index], index, kept: true });
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
         A kept row is out of flow at its own offset. -->
    <div style={`height: ${offsetY}px;`} aria-hidden="true"></div>
    {#each rows as row (rowKey(row.item, row.index))}
      <div
        style={row.kept
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
  // A row's key without a `key` prop: the item's `id` when it has one,
  // else its absolute index in `items`.
  function keyOf(item: unknown, index: number): string | number {
    if (item && typeof item === 'object' && 'id' in item) {
      const id = (item as { id: unknown }).id;
      if (typeof id === 'string' || typeof id === 'number') return id;
    }
    return index;
  }
</script>
