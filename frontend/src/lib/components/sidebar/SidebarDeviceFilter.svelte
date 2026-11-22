<script lang="ts">
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import Popover from '../primitives/Popover.svelte';
  import {
    sidebarDevices, sidebarDeviceFilterActive, sidebarDeviceStatus, setSidebarDeviceVisible, showAllSidebarDevices,
  } from '../../stores/sidebarDevices.svelte';
  import { retryCatalogLoad } from '../../stores/catalogLoad.svelte';
  import { remoteAccessAvailable } from '../../transport/buildVariant';

  // A build without remote access has one computer, so there is nothing to filter.
  const filterable = remoteAccessAvailable();

  let anchor: HTMLElement | undefined = $state();
  let open = $state(false);
  const devices = $derived(sidebarDevices());
  const filtered = $derived(sidebarDeviceFilterActive());
</script>

{#if filterable}
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
        {@const status = sidebarDeviceStatus(device.backend)}
        <MenuItem
          label={device.name}
          description={status.text}
          checkbox
          checked={device.visible}
          disabled={!device.id}
          title={status.failed ? status.text : !device.id ? 'Connect to this computer once to filter it.' : undefined}
          testId="sidebar-device-{device.key}"
          onSelect={() => setSidebarDeviceVisible(device.id, !device.visible)}
          actionText={status.failed ? 'Retry' : undefined}
          actionLabel={status.failed ? `Retry loading ${device.name}` : undefined}
          onAction={status.failed ? () => retryCatalogLoad(device.backend) : undefined}
        />
      {/each}
    </Menu>
  </Popover>
{:else}
  <span class="min-w-0 flex items-center gap-1 text-[0.6875rem] font-medium uppercase tracking-[0.18em] select-none compact:min-h-9 text-fg-subtle">
    Projects
  </span>
{/if}
