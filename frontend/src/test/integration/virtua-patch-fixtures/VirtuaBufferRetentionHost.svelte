<!--
  Browser-test fixture for virtua-patch-buffer-retention.browser.test.ts.

  A minimal, real <Virtualizer> mounted the way MessageTimeline mounts it
  (external scroll container passed via scrollRef, fixed itemSize, pixel
  bufferSize) with a lifecycle probe on every row so the test can observe
  mount/destroy bursts — the DOM-level signature of virtua dropping its
  above-viewport buffer. Not a mock: the whole point is exercising the
  patched virtua package (patches/virtua@0.49.1.patch).
-->
<script lang="ts" module>
  import type { VirtualizerHandle } from 'virtua/svelte';

  export interface VirtuaBufferRetentionControls {
    scrollEl: HTMLElement;
    handle: VirtualizerHandle;
    counters: { mounts: number; destroys: number };
  }
</script>

<script lang="ts">
  import { Virtualizer } from 'virtua/svelte';

  let {
    registerControls,
  }: {
    registerControls: (controls: VirtuaBufferRetentionControls) => void;
  } = $props();

  // 200 fixed-height rows × 40px = 8000px of content behind a 400px
  // viewport; itemSize matches the real row height so virtua's offsets are
  // exact and the test's scroll math is deterministic.
  const items = Array.from({ length: 200 }, (_, i) => i);

  let scrollEl: HTMLElement | undefined = $state();
  let handle: VirtualizerHandle | undefined = $state();

  const counters = { mounts: 0, destroys: 0 };

  function lifecycle(_node: HTMLElement) {
    counters.mounts += 1;
    return {
      destroy() {
        counters.destroys += 1;
      },
    };
  }

  $effect(() => {
    if (scrollEl && handle) registerControls({ scrollEl, handle, counters });
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
  >
    {#snippet children(item)}
      <div use:lifecycle style="height: 40px;">row {item}</div>
    {/snippet}
  </Virtualizer>
</div>
