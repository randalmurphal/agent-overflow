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
    /** Observes the element for its lifetime; returns cleanup. MUST be a
     * stable function reference — an inline closure would change identity
     * every parent render and re-run the registration effect (the
     * unobserve/observe churn the split below exists to avoid). */
    register: (element: HTMLElement) => () => void;
    /** Points measurement bookkeeping at the row's CURRENT index. Same
     * stable-identity requirement as `register`. */
    setRowIndex: (element: HTMLElement, index: number) => void;
  }

  let { children, item, index, offset, measured, register, setRowIndex }: Props = $props();

  let elementRef: HTMLElement | undefined = $state();

  // Observe once per element lifetime; cleanup on unmount only.
  $effect(() => {
    const element = elementRef;
    if (!element) return;
    return register(element);
  });

  // Head splices change a row's index without remounting it (keys are
  // item identity): update the index bookkeeping WITHOUT re-observing.
  // Per the RO spec every observe() schedules a fresh delivery, so
  // re-registering here would buy a spurious O(window) RO burst on every
  // load-older prepend / head-drop prune — exactly when the user is
  // paging at the window edge.
  $effect(() => {
    const element = elementRef;
    if (element) setRowIndex(element, index);
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
