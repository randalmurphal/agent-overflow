<script lang="ts" generics="T">
  import type { Snippet } from 'svelte';

  // One absolutely-positioned row wrapper for TimelineVirtualizer. The
  // style contract is ported from virtua's ListItem (MIT — see
  // utils/virtual/VIRTUA_LICENSE): `contain: layout style` keeps row
  // layout self-contained, absolute positioning against the engine's
  // offset keeps reflow local, and rows stay `visibility: hidden` until
  // their first measurement so an estimate-placed row is never visible
  // at a wrong position.

  interface Props {
    children: Snippet<[item: T, index: number]>;
    item: T;
    index: number;
    /** Row top in content coordinates (engine geometry). */
    offset: number;
    /** False until the row's first ResizeObserver delivery. */
    measured: boolean;
    /** Registers the element under its CURRENT index; returns cleanup. */
    observe: (element: HTMLElement, index: number) => () => void;
  }

  let { children, item, index, offset, measured, observe }: Props = $props();

  let elementRef: HTMLElement | undefined = $state();

  // Head splices change a row's index without remounting it (keys are
  // item identity): re-register so measurements report the live index.
  // Effect cleanup covers both re-index and unmount — no destroy-order
  // bug class (upstream's ListItem needed manual bookkeeping here).
  $effect(() => {
    const element = elementRef;
    if (!element) return;
    return observe(element, index);
  });
</script>

<div
  bind:this={elementRef}
  style:contain="layout style"
  style:position="absolute"
  style:width="100%"
  style:left="0px"
  style:top="{offset}px"
  style:visibility={measured ? undefined : 'hidden'}
>
  {@render children(item, index)}
</div>
