<!--
  Harness for Popover-inside-Popover interactions. The inner popover's
  anchor lives inside the outer popover's floating element, so after
  portal-to-body both are body siblings but the anchor chain still
  encodes the parent/child relationship. Tests probe:

    - Outside-click in the inner popover's content does NOT close the
      outer popover (descendant-click detection).
    - Escape inside the inner closes ONLY the inner (hasOpenDescendant
      gate on the outer's document keydown listener).
-->
<script lang="ts">
  import Popover from '../Popover.svelte';

  let {
    outerOpen = true,
    innerOpen = true,
    onOuterClose = () => {},
    onInnerClose = () => {},
  }: {
    outerOpen?: boolean;
    innerOpen?: boolean;
    onOuterClose?: () => void;
    onInnerClose?: () => void;
  } = $props();

  let outerAnchor: HTMLButtonElement | undefined = $state(undefined);
  let innerAnchor: HTMLButtonElement | undefined = $state(undefined);
</script>

<button bind:this={outerAnchor} data-testid="outer-anchor" type="button">Outer</button>
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
