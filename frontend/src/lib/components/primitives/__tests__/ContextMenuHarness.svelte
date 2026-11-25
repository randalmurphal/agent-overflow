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
    more = 0,
  }: {
    x?: number;
    y?: number;
    onDismiss?: () => void;
    /** Rows after the two, for a menu taller than the viewport. */
    more?: number;
  } = $props();
</script>

<ContextMenu {x} {y} ariaLabel="Test menu" onDismiss={() => onDismiss?.()}>
  <MenuItem label="Apple" />
  <MenuItem label="Banana" />
  {#each { length: more }, i (i)}
    <MenuItem label="Row {i + 1}" />
  {/each}
</ContextMenu>
