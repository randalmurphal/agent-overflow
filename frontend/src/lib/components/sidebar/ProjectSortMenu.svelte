<script lang="ts">
  // Dropdown for the project sort mode. A small icon-button trigger
  // anchors a Menu of three radio-like items: Latest activity, Created,
  // Manual. Selecting "Manual" enables drag reordering on each row.
  //
  // Keeps the dropdown self-contained so ProjectsSection's header stays
  // a thin shell. Sized and styled to
  // sit alongside the "Add project" icon button.

  import {
    getProjectSortMode,
    setProjectSortMode,
    type ProjectSortMode,
  } from '../../stores/sidebar.svelte';
  import ArrowDownUp from '@lucide/svelte/icons/arrow-down-up';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';

  const SORT_MODE_LABELS: Record<ProjectSortMode, string> = {
    lastActivity: 'Latest Activity',
    createdAt: 'Created',
    manual: 'Manual',
  };

  const SORT_MODE_ORDER: readonly ProjectSortMode[] = [
    'lastActivity',
    'createdAt',
    'manual',
  ];

  let triggerEl: HTMLElement | undefined = $state(undefined);
  let open = $state(false);

  let currentMode = $derived(getProjectSortMode());

  function handleToggle(): void {
    open = !open;
  }

  function handleClose(): void {
    open = false;
  }

  function handleSelect(mode: ProjectSortMode): void {
    setProjectSortMode(mode);
    open = false;
  }
</script>

<span bind:this={triggerEl}>
  <IconButton
    label={`Sort Projects (${SORT_MODE_LABELS[currentMode]})`}
    size="sm"
    onClick={handleToggle}
  >
    {#snippet children()}
      <span data-testid="sidebar-sort-icon" data-mode={currentMode} class="flex items-center">
        <Icon icon={ArrowDownUp} size={13} strokeWidth={2} class="opacity-80" />
      </span>
    {/snippet}
  </IconButton>
</span>

<Popover
  anchor={triggerEl}
  {open}
  onClose={handleClose}
  placement="bottom-end"
  role="none"
>
  {#snippet children()}
    <Menu ariaLabel="Sort Projects" onClose={handleClose}>
      {#snippet children()}
        {#each SORT_MODE_ORDER as mode (mode)}
          <MenuItem
            label={SORT_MODE_LABELS[mode]}
            checked={currentMode === mode}
            onSelect={() => handleSelect(mode)}
          />
        {/each}
      {/snippet}
    </Menu>
  {/snippet}
</Popover>
