<!--
  Harness that wraps a Popover around a deterministic anchor button and a
  deterministic content pane so tests can probe open/close, outside-click,
  and Escape handling without constructing snippets from TS.

  The harness exposes the `open` prop so tests can close the popover by
  passing `open=false` via rerender(), mimicking a real caller.
-->
<script lang="ts">
  import Popover from '../Popover.svelte';

  let {
    open = false,
    placement = 'bottom-start' as
      | 'bottom-start'
      | 'bottom-end'
      | 'top-start'
      | 'top-end'
      | 'right-start'
      | 'left-start',
    onClose = () => {},
    role = 'none' as 'dialog' | 'menu' | 'listbox' | 'none',
    matchAnchorWidth = false,
    claimTab = false,
    restoreFocusToAnchor = false,
  }: {
    open?: boolean;
    placement?:
      | 'bottom-start'
      | 'bottom-end'
      | 'top-start'
      | 'top-end'
      | 'right-start'
      | 'left-start';
    onClose?: () => void;
    role?: 'dialog' | 'menu' | 'listbox' | 'none';
    matchAnchorWidth?: boolean;
    /** Opt into the picker-in-dialog focus contract (Popover constraint #2). */
    claimTab?: boolean;
    restoreFocusToAnchor?: boolean;
  } = $props();

  let anchor: HTMLButtonElement | undefined = $state(undefined);
</script>

<button bind:this={anchor} data-testid="popover-anchor" type="button">Anchor</button>
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
