<!--
  Harness that nests a submenu inside a parent Menu so tests can exercise
  ArrowRight-opens / ArrowLeft-closes / Escape-closes-inner / selection
  collapse behaviour.
-->
<script lang="ts">
  import Menu from '../Menu.svelte';
  import MenuItem from '../MenuItem.svelte';
  import MenuSubmenuItem from '../MenuSubmenuItem.svelte';

  let {
    onParentClose = () => {},
    onLeafSelect = (label: string) => { void label; },
  }: {
    onParentClose?: () => void;
    onLeafSelect?: (label: string) => void;
  } = $props();
</script>

<Menu ariaLabel="Parent" onClose={onParentClose}>
  {#snippet children()}
    <MenuItem label="First" onSelect={() => onLeafSelect('First')} />
    <MenuSubmenuItem label="More">
      {#snippet children()}
        <MenuItem label="Nested-One" onSelect={() => onLeafSelect('Nested-One')} />
        <MenuItem label="Nested-Two" onSelect={() => onLeafSelect('Nested-Two')} />
      {/snippet}
    </MenuSubmenuItem>
    <MenuItem label="Last" onSelect={() => onLeafSelect('Last')} />
  {/snippet}
</Menu>
