<!--
  Stand-in for virtua/svelte's <Virtualizer>, used by
  messageTimelineVirtuaMarking.test.ts via vi.mock. Renders every row like a
  plain list (happy-dom has no geometry, so windowing would be fiction) and
  exposes the VirtualizerHandle surface as zero/no-op methods — except
  markProgrammaticScroll (the patched method, patches/virtua@0.49.1.patch),
  which records into virtuaMarkRecorder so the test can observe that
  MessageTimeline's onBeforeScrollTopWrite wiring reached the handle.
-->
<script lang="ts">
  import type { Snippet } from 'svelte';
  import { recordVirtuaMark } from './virtuaMarkRecorder';

  let {
    data,
    children,
  }: {
    data: readonly unknown[];
    children: Snippet<[unknown, number]>;
  } = $props();

  export const getCache = (): unknown => ({});
  export const getScrollOffset = (): number => 0;
  export const getScrollSize = (): number => 0;
  export const getViewportSize = (): number => 0;
  export const findItemIndex = (): number => 0;
  export const getItemOffset = (): number => 0;
  export const getItemSize = (): number => 0;
  export const scrollToIndex = (): void => {};
  export const scrollTo = (): void => {};
  export const scrollBy = (): void => {};
  export const markProgrammaticScroll = (): void => {
    recordVirtuaMark();
  };
  // Second patched method (scroll-applier seam). The stub accepts the
  // registration and drops it — no compensation writes exist here.
  export const setScrollApplier = (): void => {};
</script>

{#each data as node, index}
  {@render children(node, index)}
{/each}
