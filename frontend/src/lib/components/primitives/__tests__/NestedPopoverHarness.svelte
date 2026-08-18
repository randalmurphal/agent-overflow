<!--
  Harness for Popover-inside-Popover interactions. The inner popover's
  anchor lives inside the outer popover's floating element, so after
  portal-to-body both are body siblings but the anchor chain still
  encodes the parent/child relationship. Tests probe:

    - Outside-click in the inner popover's content does NOT close the
      outer popover (descendant-click detection).
    - Escape inside the inner closes ONLY the inner (hasOpenDescendant
      gate on the outer's document keydown listener).
    - With `withClipBoundary`, the INNER popover inherits the clip
      boundary of the chain's real trigger across the portal hop.
-->
<script lang="ts">
  import Popover from '../Popover.svelte';
  import type { PopoverCloseReason } from '../../../utils/popoverOwnership';

  let {
    outerOpen = true,
    innerOpen = true,
    onOuterClose = () => {},
    onInnerClose = () => {},
    withClipBoundary = false,
  }: {
    outerOpen?: boolean;
    innerOpen?: boolean;
    onOuterClose?: (reason?: PopoverCloseReason) => void;
    onInnerClose?: (reason?: PopoverCloseReason) => void;
    /** Wrap the OUTER anchor in a `[data-popover-clip-boundary]` container. */
    withClipBoundary?: boolean;
  } = $props();

  let outerAnchor: HTMLButtonElement | undefined = $state(undefined);
  let innerAnchor: HTMLButtonElement | undefined = $state(undefined);
</script>

{#if withClipBoundary}
  <div data-popover-clip-boundary data-testid="clip-boundary">
    <button bind:this={outerAnchor} data-testid="outer-anchor" type="button">Outer</button>
  </div>
{:else}
  <button bind:this={outerAnchor} data-testid="outer-anchor" type="button">Outer</button>
{/if}
<Popover anchor={outerAnchor} open={outerOpen} onClose={onOuterClose} placement="bottom-start" role="menu">
  {#snippet children()}
    <div data-testid="outer-content">
      <!-- Inner anchor lives inside the outer popover's floatingEl
           on purpose. After the outer is portaled to body, this
           anchor goes along with it — the child popover's descendant
           check relies on that. -->
      <button bind:this={innerAnchor} data-testid="inner-anchor" type="button">Inner</button>
      <Popover anchor={innerAnchor} open={innerOpen} onClose={onInnerClose} placement="right-start" role="menu">
        {#snippet children()}
          <div data-testid="inner-content">
            <button data-testid="inner-item" type="button">inner-item</button>
          </div>
        {/snippet}
      </Popover>
    </div>
  {/snippet}
</Popover>
<button data-testid="outside-button" type="button">outside</button>
