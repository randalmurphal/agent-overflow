<script lang="ts">
  // The menu of a ticket that another ticket names (a linked ticket, the
  // parent), on a right-click or long press of it: open it in the ticket's
  // place, or in a pane of its own where that is offered, and copy its key.
  // The caller's own rows (a link's Remove) sit before Copy key. A row
  // closes the menu, then acts.
  import type { Snippet } from 'svelte';
  import ContextMenu from '../primitives/ContextMenu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import { ticketPanesOffered } from '../../stores/ticketPanes.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { copyToClipboard } from '../../utils/clipboard';

  interface Props {
    x: number;
    y: number;
    ariaLabel: string;
    /** The ticket's key; with none there is no Copy key. */
    ticketKey: string;
    testIdPrefix: string;
    onOpen: (ownPane: boolean) => void;
    onDismiss: () => void;
    /** The caller's rows, given the runner that closes the menu first. */
    children?: Snippet<[run: (action: () => void) => void]>;
  }

  let { x, y, ariaLabel, ticketKey, testIdPrefix, onOpen, onDismiss, children }: Props = $props();

  function run(action: () => void): void {
    onDismiss();
    action();
  }

  async function copyKey(): Promise<void> {
    if (!(await copyToClipboard(ticketKey))) addToast('error', 'Could not copy the key.');
  }
</script>

<ContextMenu {x} {y} {ariaLabel} {onDismiss}>
  <MenuItem label="Open" testId="{testIdPrefix}-open" onSelect={() => run(() => onOpen(false))} />
  {#if ticketPanesOffered()}
    <MenuItem label="Open in its own pane" testId="{testIdPrefix}-own-pane" onSelect={() => run(() => onOpen(true))} />
  {/if}
  {@render children?.(run)}
  {#if ticketKey}
    <MenuItem label="Copy key" testId="{testIdPrefix}-copy-key" onSelect={() => run(() => { void copyKey(); })} />
  {/if}
</ContextMenu>
