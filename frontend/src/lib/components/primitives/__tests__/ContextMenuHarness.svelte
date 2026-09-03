<!--
  Harness that wires a ContextMenu around two MenuItems so tests can
  exercise its placement and dismissal without constructing snippets
  from TS.
-->
<script lang="ts">
  import ContextMenu from '../ContextMenu.svelte';
  import MenuItem from '../MenuItem.svelte';

  let {
    x = 40,
    y = 60,
    onDismiss = undefined as (() => void) | undefined,
  }: {
    x?: number;
    y?: number;
    onDismiss?: () => void;
  } = $props();
</script>

<ContextMenu {x} {y} ariaLabel="Test menu" onDismiss={() => onDismiss?.()}>
  <MenuItem label="Apple" />
  <MenuItem label="Banana" />
</ContextMenu>
