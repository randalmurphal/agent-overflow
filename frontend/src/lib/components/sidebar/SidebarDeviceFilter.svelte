<script lang="ts">
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import Popover from '../primitives/Popover.svelte';
  import { sidebarDevices, sidebarDeviceFilterActive, setSidebarDeviceVisible, showAllSidebarDevices } from '../../stores/sidebarDevices.svelte';

  let anchor: HTMLElement | undefined = $state();
  let open = $state(false);
  const devices = $derived(sidebarDevices());
  const filtered = $derived(sidebarDeviceFilterActive());
</script>

<button
  bind:this={anchor}
  type="button"
  aria-label="Filter projects by computer"
  aria-haspopup="menu"
  aria-expanded={open}
  onclick={() => { open = !open; }}
  class="min-w-0 flex items-center gap-1 text-[0.6875rem] font-medium uppercase tracking-[0.18em] select-none compact:min-h-9 hover:text-fg focus-visible:outline-accent {filtered ? 'text-accent' : 'text-fg-subtle'}"
>
  Projects
  <Icon icon={ChevronDown} size={12} />
</button>
<Popover {anchor} {open} onClose={() => { open = false; }} placement="bottom-start" role="none">
  <Menu ariaLabel="Computers shown in sidebar" onClose={() => { open = false; }}>
    <MenuItem label="All computers" checked={!filtered} onSelect={showAllSidebarDevices} />
    <MenuDivider />
    {#each devices as device (device.key)}
      <MenuItem
        label={device.name}
        checkbox
        checked={device.visible}
        disabled={!device.id}
        title={!device.id ? 'Connect to this computer once to filter it.' : undefined}
        onSelect={() => setSidebarDeviceVisible(device.id, !device.visible)}
      />
    {/each}
  </Menu>
</Popover>
