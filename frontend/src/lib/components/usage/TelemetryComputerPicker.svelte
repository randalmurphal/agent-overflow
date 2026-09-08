<script lang="ts">
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import Popover from '../primitives/Popover.svelte';
  import { telemetryComputers, telemetrySelection, telemetrySelectionLabel, setTelemetrySelection, toggleTelemetryComputer, missingTelemetrySelection, type TelemetryKind } from '../../stores/telemetryComputers.svelte';

  let { kind }: { kind: TelemetryKind } = $props();
  let anchor: HTMLElement | undefined = $state();
  let open = $state(false);
  const computers = $derived(telemetryComputers(kind));
  const selectedCount = $derived(computers.filter((c) => c.selected).length);
</script>

<button bind:this={anchor} type="button" class="inline-flex min-w-0 max-w-full items-center gap-1 text-[0.6875rem] text-fg-muted hover:text-fg" aria-label={kind === 'usage' ? 'Computers included in usage' : 'Computers shown in system stats'} aria-haspopup="menu" aria-expanded={open} onclick={() => { open = !open; }}>
  <span class="truncate">{telemetrySelectionLabel(kind)}</span><span class="shrink-0"><Icon icon={ChevronDown} size={12} /></span>
</button>
<Popover {anchor} {open} onClose={() => { open = false; }} placement="bottom-start" role="none">
  <Menu minWidthClass="w-[min(20rem,calc(100vw-2rem))] min-w-0" ariaLabel="Computer selection" onClose={() => { open = false; }}>
    <MenuItem label={kind === 'usage' ? 'All computers' : 'Default computer'} checked={telemetrySelection(kind) === null} onSelect={() => setTelemetrySelection(kind, null)} />
    <MenuDivider />
    {#if missingTelemetrySelection(kind) > 0}
      <MenuItem label="A selected computer was removed" description="Choose the default to reset this selection." disabled />
    {/if}
    {#each computers as computer (computer.key)}
      <MenuItem label={computer.name} title={computer.name} suffix={computer.connected ? undefined : 'offline'} checkbox checked={computer.selected} disabled={computer.selected && selectedCount === 1} onSelect={() => toggleTelemetryComputer(kind, computer.key)} />
    {/each}
  </Menu>
</Popover>
