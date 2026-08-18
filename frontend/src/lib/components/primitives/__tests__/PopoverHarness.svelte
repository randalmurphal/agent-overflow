<!--
  Harness that wraps a Popover around a deterministic anchor button and a
  deterministic content pane so tests can probe open/close, outside-click,
  and Escape handling without constructing snippets from TS.

  The harness exposes the `open` prop so tests can close the popover by
  passing `open=false` via rerender(), mimicking a real caller.
-->
<script lang="ts">
  import Popover from '../Popover.svelte';
  import type { PopoverCloseReason } from '../../../utils/popoverOwnership';
  import type { PopoverPlacement } from '../../../utils/popoverGeometry';

  let {
    open = false,
    placement = 'bottom-start' as PopoverPlacement,
    onClose = () => {},
    role = 'none' as 'dialog' | 'menu' | 'listbox' | 'none',
    matchAnchorWidth = false,
    claimTab = false,
    restoreFocusToAnchor = false,
    withClipBoundary = false,
  }: {
    open?: boolean;
    placement?: PopoverPlacement;
    onClose?: (reason?: PopoverCloseReason) => void;
    role?: 'dialog' | 'menu' | 'listbox' | 'none';
    matchAnchorWidth?: boolean;
    /** Opt into the picker-in-dialog focus contract (Popover constraint #2). */
    claimTab?: boolean;
    restoreFocusToAnchor?: boolean;
    /** Wrap the anchor in a `[data-popover-clip-boundary]` container. */
    withClipBoundary?: boolean;
  } = $props();

  let anchor: HTMLButtonElement | undefined = $state(undefined);
</script>

{#if withClipBoundary}
  <div data-popover-clip-boundary data-testid="clip-boundary">
    <button bind:this={anchor} data-testid="popover-anchor" type="button">Anchor</button>
  </div>
{:else}
  <button bind:this={anchor} data-testid="popover-anchor" type="button">Anchor</button>
{/if}
<Popover
  {anchor}
  {open}
  {onClose}
  {placement}
  {role}
  {matchAnchorWidth}
  {claimTab}
  restoreFocusTo={restoreFocusToAnchor ? anchor : undefined}
>
  {#snippet children()}
    <div data-testid="popover-content">
      <button type="button" data-testid="popover-inside-button">inside</button>
    </div>
  {/snippet}
</Popover>
<button data-testid="outside-button" type="button">outside</button>
