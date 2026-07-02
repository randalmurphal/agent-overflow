<!--
  Browser-test fixture for virtua-patch-scroll-applier.browser.test.ts.

  A minimal, real <Virtualizer> (external scroll container, pixel
  bufferSize) whose row heights are individually controllable, so a test
  can grow a row ABOVE the viewport and trigger virtua's scroll-jump
  compensation ($fixScrollJump) — the write the patched setScrollApplier
  seam routes to an external applier (patches/virtua@0.49.1.patch).
  Not a mock: the whole point is exercising the patched virtua package.
-->
<script lang="ts" module>
  import type { VirtualizerHandle } from 'virtua/svelte';

  export interface VirtuaApplierControls {
    scrollEl: HTMLElement;
    handle: VirtualizerHandle;
    growRow(index: number, heightPx: number): void;
    counters: { onscroll: number };
  }
</script>

<script lang="ts">
  import { Virtualizer } from 'virtua/svelte';

  let {
    registerControls,
  }: {
    registerControls: (controls: VirtuaApplierControls) => void;
  } = $props();

  // 200 rows × 40px = 8000px of content behind a 400px viewport; itemSize
  // matches the base row height so virtua's offsets are exact until a test
  // grows a specific row.
  const items = Array.from({ length: 200 }, (_, i) => i);

  let heights = $state<Record<number, number>>({});

  let scrollEl: HTMLElement | undefined = $state();
  let handle: VirtualizerHandle | undefined = $state();

  // The store fires UPDATE_SCROLL_EVENT on every ACTION_SCROLL — including
  // the patched core's decline-poke, which dispatches WITHOUT a DOM scroll
  // event. Counting onscroll is how the test observes the poke directly.
  const counters = { onscroll: 0 };

  $effect(() => {
    if (scrollEl && handle) {
      registerControls({
        scrollEl,
        handle,
        growRow: (index, heightPx) => {
          heights[index] = heightPx;
        },
        counters,
      });
    }
  });
</script>

<div bind:this={scrollEl} style="height: 400px; overflow-y: auto;">
  <Virtualizer
    bind:this={handle}
    data={items}
    getKey={(item) => item}
    scrollRef={scrollEl}
    bufferSize={600}
    itemSize={40}
    onscroll={() => { counters.onscroll += 1; }}
  >
    {#snippet children(item)}
      <div style="height: {heights[item] ?? 40}px;">row {item}</div>
    {/snippet}
  </Virtualizer>
</div>
