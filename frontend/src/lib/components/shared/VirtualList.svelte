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

  import type { Snippet } from 'svelte';

  let {
    items,
    rowHeight,
    overscan = 6,
    ariaLabel,
    role = 'list',
    class: extraClass = '',
    children,
    viewportRef = $bindable(undefined),
  }: {
    items: T[];
    rowHeight: number;
    overscan?: number;
    ariaLabel?: string;
    role?: string;
    class?: string;
    children?: Snippet<[T, number]>;
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
  let startIndex = $derived(Math.max(0, Math.floor(scrollTop / rowHeight) - overscan));
  let visibleCount = $derived(Math.ceil(clientHeight / rowHeight) + overscan * 2);
  let endIndex = $derived(Math.min(items.length, startIndex + visibleCount));
  let offsetY = $derived(startIndex * rowHeight);
  let totalHeight = $derived(items.length * rowHeight);

  let windowItems = $derived(items.slice(startIndex, endIndex));

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
  <div
    style={`height: ${totalHeight}px; position: relative;`}
    data-testid="virtual-list-spacer"
  >
    <div
      style={`transform: translateY(${offsetY}px); position: absolute; top: 0; left: 0; right: 0;`}
    >
      {#each windowItems as item, windowIdx (keyOf(item, startIndex + windowIdx))}
        <div
          style={`height: ${rowHeight}px;`}
          data-testid="virtual-list-row"
          data-row-index={startIndex + windowIdx}
        >
          {@render children?.(item, startIndex + windowIdx)}
        </div>
      {/each}
    </div>
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
