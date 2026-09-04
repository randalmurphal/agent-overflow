<!--
  Harness for <RenderBoundary>: a test cannot hand a component a `children`
  snippet from a props object, so the snippet lives here and `child` picks
  what goes inside it.
-->
<script lang="ts">
  import RenderBoundary from '../../lib/components/shared/RenderBoundary.svelte';
  import ThrowsOnRender from './ThrowsOnRender.svelte';
  import ThrowsWhenTold from './ThrowsWhenTold.svelte';

  let {
    label = 'The test region',
    testId = 'boundary-render-error',
    child = 'ok',
    shouldThrow = () => true,
  }: {
    label?: string;
    testId?: string;
    child?: 'ok' | 'throws' | 'gated';
    shouldThrow?: () => boolean;
  } = $props();
</script>

<RenderBoundary {label} {testId}>
  {#if child === 'throws'}
    <ThrowsOnRender />
  {:else if child === 'gated'}
    <ThrowsWhenTold {shouldThrow} />
  {:else}
    <div data-testid="boundary-child">child content</div>
  {/if}
</RenderBoundary>
