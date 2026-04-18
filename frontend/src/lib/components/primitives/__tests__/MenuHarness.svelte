<!--
  Harness that wires a Menu around a predictable set of MenuItems so
  tests can exercise arrow navigation, typeahead, and selection without
  constructing snippets from TS.
-->
<script lang="ts">
  import Menu from '../Menu.svelte';
  import MenuItem from '../MenuItem.svelte';
  import MenuDivider from '../MenuDivider.svelte';

  let {
    onClose = undefined as (() => void) | undefined,
    onSelect = undefined as ((label: string) => void) | undefined,
    disableSecond = false,
  }: {
    onClose?: () => void;
    onSelect?: (label: string) => void;
    disableSecond?: boolean;
  } = $props();
</script>

<Menu ariaLabel="Test menu" {onClose}>
  {#snippet children()}
    <MenuItem label="Apple" onSelect={() => onSelect?.('Apple')} />
    <MenuItem label="Banana" disabled={disableSecond} onSelect={() => onSelect?.('Banana')} />
    <MenuDivider />
    <MenuItem label="Cherry" onSelect={() => onSelect?.('Cherry')} />
    <MenuItem label="Date" onSelect={() => onSelect?.('Date')} />
  {/snippet}
</Menu>
